#!/usr/bin/env bash
# install.sh -- curl-able bootstrap installer for the orchard suite (#747
# Step 7).
#
#   curl -fsSL https://raw.githubusercontent.com/drewdrewthis/orchardist/main/scripts/install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --version orchard-v1.2.3 --prefix ~/bin
#
# Resolves the host's rust target triple, downloads that release's suite
# tarball (orchard-suite-<triple>.tar.gz) + SHA256SUMS, verifies the
# checksum, and installs each binary atomically to --prefix (backing up any
# existing file first as NAME.bak-<UTC timestamp>, keeping the 3 newest
# backups per binary). Idempotent -- safe to re-run. If orchard-daemon.service
# or orchard.service is already an active systemd --user unit, it's restarted
# after install; otherwise orchard.service is (re)installed from the
# template.
#
# Flags:
#   --version TAG|X.Y.Z   Install this release instead of latest. Accepts a
#                         full tag (orchard-vX.Y.Z), a v-prefixed version, or
#                         a bare semver -- all normalize to the real tag.
#   --prefix PATH         Install location. Default: /usr/local/bin if
#                         writable, else ~/.local/bin.
#   --system              Prefer /usr/local/bin; fail if it isn't writable
#                         (rather than silently falling back). Elevates
#                         filesystem operations via `sudo -n` when needed; if
#                         passwordless sudo isn't available, re-run the whole
#                         command yourself under sudo.
#   --from-source         Build locally (requires go + cargo) instead of
#                         downloading a release. Uses the current checkout if
#                         run from one; otherwise clones ORCHARD_RELEASE_REPO.
#   --no-service          Skip restarting/installing the systemd user unit.
#   --json                Emit one JSON line -- {"ok":..,"data":..,"error":..}
#                         -- instead of human-readable text.
#   -h, --help             Show this help.
#
# Env:
#   ORCHARD_RELEASE_REPO      owner/repo (default drewdrewthis/orchardist), or
#                             an absolute URL to use as the API root instead
#                             of GitHub (fixture/enterprise). Mirrors
#                             internal/release.resolveTarget's exact contract.
#   ORCHARD_RELEASE_BASE_URL  Test/mirror override: skip the GitHub API
#                             entirely and fetch assets directly from
#                             <this>/<asset-name> (the flat layout `make
#                             dist` produces under dist/).
#   ORCHARD_GITHUB_TOKEN / GH_TOKEN / GITHUB_TOKEN
#                             Optional bearer token for the GitHub API call
#                             (raises the unauthenticated rate limit).
set -euo pipefail

DEFAULT_REPO="drewdrewthis/orchardist"
DEFAULT_API="https://api.github.com"
SUITE_PACKAGE="orchard-suite"
SUMS_ASSET="SHA256SUMS"
# Mirrors internal/release/assets.go's SuiteBinaries -- the Go upgrade
# client's ground truth for what a release/install directory may hold.
SUITE_BINARIES=(orchard-daemon orchard-sidebar orchard-shell orchard-upgrade orchard-tui orchard)
# Priority order: orchard-daemon.service is the unit this repo actually
# ships as active today; orchard.service is install_service's own template
# name, checked second so an older install using it is still recognized.
SERVICE_UNITS=(orchard-daemon.service orchard.service)
BACKUP_KEEP=3

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-.}")" 2>/dev/null && pwd || true)"

VERSION=""
PREFIX=""
SYSTEM=0
FROM_SOURCE=0
NO_SERVICE=0
JSON_MODE=0
JSON_EMITTED=0

API_ROOT=""
REPO_SLUG=""
RESOLVED_TAG=""
SUITE_DOWNLOAD_URL=""
SUMS_DOWNLOAD_URL=""
STAGED_DIR=""
FROM_SOURCE_DIR=""
CLEANUP_PATHS=()
INSTALL_SUDO=()
SERVICE_ACTION=""

usage() {
  sed -n '2,41p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

json_escape() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\t'/\\t}
  s=${s//$'\r'/\\r}
  printf '%s' "$s"
}

fail() {
  local msg=$1
  if [ "$JSON_MODE" -eq 1 ]; then
    printf '{"ok":false,"data":null,"error":"%s"}\n' "$(json_escape "$msg")"
    JSON_EMITTED=1
  else
    echo "error: $msg" >&2
  fi
  exit 1
}

on_err() {
  local code=$?
  if [ "$JSON_MODE" -eq 1 ] && [ "$JSON_EMITTED" -eq 0 ]; then
    printf '{"ok":false,"data":null,"error":"%s"}\n' "$(json_escape "install failed (exit $code)")"
    JSON_EMITTED=1
  fi
  return "$code"
}
trap on_err ERR

# cleanup is registered on EXIT, so its own last command's status would
# otherwise become the script's real exit code (a classic bash trap
# gotcha) -- save/restore $? explicitly so a clean `exit 0` (e.g. --help,
# or --from-source with nothing queued for cleanup) can't be clobbered by
# an empty-array `[ -n "" ]` test turning success into exit 1.
cleanup() {
  local status=$? p
  for p in "${CLEANUP_PATHS[@]:-}"; do
    [ -n "$p" ] && rm -rf "$p"
  done
  return "$status"
}
trap cleanup EXIT

have_jq() { command -v jq >/dev/null 2>&1; }
have_sha256sum() { command -v sha256sum >/dev/null 2>&1; }

# extract_json_string KEY -- reads a JSON object from stdin, prints its
# top-level string value for KEY.
extract_json_string() {
  local key=$1
  if have_jq; then
    jq -r --arg k "$key" '.[$k] // empty'
    return
  fi
  grep -o "\"$key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -1 | sed -E 's/.*:[[:space:]]*"([^"]*)"/\1/'
}

# extract_asset_url NAME -- reads a GitHub release JSON from stdin, prints
# the browser_download_url of the asset named exactly NAME.
extract_asset_url() {
  local name=$1
  if have_jq; then
    jq -r --arg n "$name" '.assets[] | select(.name == $n) | .browser_download_url' | head -1
    return
  fi
  # jq-free fallback: GitHub pretty-prints one key per line with "name"
  # before "browser_download_url" in each asset object -- a heuristic, not a
  # real parser, kept only as a portability safety net when jq is missing.
  local escaped
  escaped=$(printf '%s' "$name" | sed 's/\./\\./g')
  grep -A5 "\"name\": *\"$escaped\"" | grep -o '"browser_download_url": *"[^"]*"' | head -1 | sed -E 's/.*"([^"]*)"$/\1/'
}

detect_triple() {
  local os arch
  os=$(uname -s)
  arch=$(uname -m)
  case "$os" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) fail "unsupported OS: $os" ;;
  esac
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) fail "unsupported architecture: $arch" ;;
  esac
  case "$os/$arch" in
    linux/amd64) echo x86_64-unknown-linux-gnu ;;
    linux/arm64) echo aarch64-unknown-linux-gnu ;;
    darwin/amd64) echo x86_64-apple-darwin ;;
    darwin/arm64) echo aarch64-apple-darwin ;;
    *) fail "no orchard release target for $os/$arch" ;;
  esac
}

resolve_prefix() {
  if [ -n "$PREFIX" ]; then
    printf '%s' "$PREFIX"
    return
  fi
  if [ "$SYSTEM" -eq 1 ]; then
    if [ -w /usr/local/bin ] 2>/dev/null || { [ ! -e /usr/local/bin ] && [ -w /usr/local ] 2>/dev/null; }; then
      printf '/usr/local/bin'
      return
    fi
    fail "--system given but /usr/local/bin is not writable (try sudo, or drop --system)"
  fi
  if [ -w /usr/local/bin ] 2>/dev/null; then
    printf '/usr/local/bin'
    return
  fi
  printf '%s/.local/bin' "$HOME"
}

# file_owner_uid FILE -- prints FILE's owning UID. Tries GNU stat (Linux,
# the real target) then BSD stat (macOS, local dev/testing only) --
# mirrors this file's existing shasum/sha256sum dual-tool-support pattern.
file_owner_uid() {
  stat -c '%u' "$1" 2>/dev/null || stat -f '%u' "$1" 2>/dev/null
}

# root_owned_suite_binary DIR -- prints the first SUITE_BINARIES member in
# DIR that's owned by uid 0; fails (empty stdout) if none are.
root_owned_suite_binary() {
  local dir=$1 name
  for name in "${SUITE_BINARIES[@]}"; do
    [ -e "$dir/$name" ] || continue
    [ "$(file_owner_uid "$dir/$name")" = "0" ] && { printf '%s' "$name"; return 0; }
  done
  return 1
}

# check_root_owned_conflict PREFIX -- refuses to silently fight a
# root-owned install. Checks both PREFIX and (when different) the canonical
# /usr/local/bin -- the latter catches resolve_prefix's own silent fallback
# to ~/.local/bin when /usr/local/bin already holds root-owned binaries and
# isn't writable, which would otherwise "succeed" by installing somewhere
# not on PATH. Only relevant when we're not already root and --system
# wasn't given (that's the acknowledged opt-in to elevate, see
# resolve_install_sudo).
check_root_owned_conflict() {
  local prefix=$1 hit dir
  [ "$SYSTEM" -eq 1 ] && return 0
  [ "$(id -u)" -eq 0 ] && return 0
  for dir in "$prefix" /usr/local/bin; do
    if hit=$(root_owned_suite_binary "$dir"); then
      fail "$dir/$hit is root-owned; re-run with sudo (sudo $0 --system), or choose a --prefix you own"
    fi
  done
}

# resolve_install_sudo -- when --system needs elevation (prefix operations
# as a non-root user), sets INSTALL_SUDO to a `sudo -n` prefix if
# passwordless sudo works; otherwise fails with instructions to re-run
# under sudo manually. Never applied to systemctl --user (a per-user-uid
# concept sudo would corrupt) -- only to $prefix filesystem operations.
resolve_install_sudo() {
  [ "$SYSTEM" -eq 1 ] || return 0
  [ "$(id -u)" -eq 0 ] && return 0
  if command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    INSTALL_SUDO=(sudo -n)
    return 0
  fi
  fail "--system needs root; re-run with sudo: sudo $0 --system (or drop --system to install under \$HOME)"
}

# resolve_repo_target -- mirrors internal/release.resolveTarget exactly:
# ORCHARD_RELEASE_REPO is either an owner/repo slug (resolved against
# GitHub's API) or an absolute URL (a fixture/enterprise API root, repo slug
# stays the default). Sets API_ROOT and REPO_SLUG.
resolve_repo_target() {
  local v="${ORCHARD_RELEASE_REPO:-}"
  if [ -z "$v" ]; then
    API_ROOT="$DEFAULT_API"
    REPO_SLUG="$DEFAULT_REPO"
    return
  fi
  case "$v" in
    http://* | https://*)
      API_ROOT="${v%/}"
      REPO_SLUG="$DEFAULT_REPO"
      ;;
    *)
      API_ROOT="$DEFAULT_API"
      REPO_SLUG="${v#/}"
      REPO_SLUG="${REPO_SLUG%/}"
      ;;
  esac
}

curl_json() {
  local url=$1
  local tok="${ORCHARD_GITHUB_TOKEN:-${GH_TOKEN:-${GITHUB_TOKEN:-}}}"
  if [ -n "$tok" ]; then
    curl -fsSL -H "Accept: application/vnd.github+json" -H "Authorization: Bearer $tok" "$url"
  else
    curl -fsSL -H "Accept: application/vnd.github+json" "$url"
  fi
}

# normalize_tag VALUE -- accepts a bare semver, a v-prefixed one, or an
# already-full orchard-v<semver> tag; always returns the full release tag
# (release-please's manifest-mode component-prefixed format, e.g.
# orchard-v1.1.0 -- see release-please-config.json).
normalize_tag() {
  local v=$1
  case "$v" in
    orchard-v*) printf '%s' "$v" ;;
    v*) printf 'orchard-%s' "$v" ;;
    *) printf 'orchard-v%s' "$v" ;;
  esac
}

# resolve_release_assets TRIPLE -- hits the GitHub Releases API and sets
# RESOLVED_TAG, SUITE_DOWNLOAD_URL, SUMS_DOWNLOAD_URL.
resolve_release_assets() {
  local triple=$1
  local endpoint json suite_name
  resolve_repo_target
  if [ -n "$VERSION" ]; then
    endpoint="$API_ROOT/repos/$REPO_SLUG/releases/tags/$(normalize_tag "$VERSION")"
  else
    endpoint="$API_ROOT/repos/$REPO_SLUG/releases/latest"
  fi
  json=$(curl_json "$endpoint") || fail "could not reach $endpoint"
  RESOLVED_TAG=$(printf '%s' "$json" | extract_json_string tag_name)
  [ -n "$RESOLVED_TAG" ] || fail "release response from $endpoint had no tag_name"
  suite_name="${SUITE_PACKAGE}-${triple}.tar.gz"
  SUITE_DOWNLOAD_URL=$(printf '%s' "$json" | extract_asset_url "$suite_name")
  SUMS_DOWNLOAD_URL=$(printf '%s' "$json" | extract_asset_url "$SUMS_ASSET")
  [ -n "$SUITE_DOWNLOAD_URL" ] || fail "release $RESOLVED_TAG has no asset $suite_name (this platform may not be built yet; try --from-source)"
  [ -n "$SUMS_DOWNLOAD_URL" ] || fail "release $RESOLVED_TAG has no asset $SUMS_ASSET"
}

# resolve_release_assets_or_base_url TRIPLE -- ORCHARD_RELEASE_BASE_URL, when
# set, bypasses the GitHub API entirely and points at a flat asset server
# (e.g. `python3 -m http.server` over `make dist`'s dist/) -- this is what
# lets the release path be tested without a real GitHub release.
resolve_release_assets_or_base_url() {
  local triple=$1
  if [ -n "${ORCHARD_RELEASE_BASE_URL:-}" ]; then
    local base="${ORCHARD_RELEASE_BASE_URL%/}"
    RESOLVED_TAG="${VERSION:-local}"
    SUITE_DOWNLOAD_URL="$base/${SUITE_PACKAGE}-${triple}.tar.gz"
    SUMS_DOWNLOAD_URL="$base/$SUMS_ASSET"
    return
  fi
  resolve_release_assets "$triple"
}

# verify_and_stage TRIPLE WORKDIR -- downloads the suite tarball + sums into
# WORKDIR, verifies the checksum, extracts, and sets STAGED_DIR.
verify_and_stage() {
  local triple=$1 work=$2
  local suite_name="${SUITE_PACKAGE}-${triple}.tar.gz"
  local expected actual

  curl -fsSL -o "$work/$suite_name" "$SUITE_DOWNLOAD_URL" || fail "download failed: $SUITE_DOWNLOAD_URL"
  curl -fsSL -o "$work/$SUMS_ASSET" "$SUMS_DOWNLOAD_URL" || fail "download failed: $SUMS_DOWNLOAD_URL"

  expected=$(awk -v f="$suite_name" '{ name=$2; sub(/^\*/, "", name); if (name == f) { print $1; exit } }' "$work/$SUMS_ASSET")
  [ -n "$expected" ] || fail "$SUMS_ASSET has no entry for $suite_name"

  if have_sha256sum; then
    actual=$(sha256sum "$work/$suite_name" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "$work/$suite_name" | awk '{print $1}')
  fi
  [ "$expected" = "$actual" ] || fail "checksum mismatch for $suite_name (expected $expected, got $actual)"

  mkdir -p "$work/extracted"
  tar xzf "$work/$suite_name" -C "$work/extracted"
  STAGED_DIR="$work/extracted"
}

# rotate_backups DEST_DIR NAME -- keeps only the BACKUP_KEEP newest
# NAME.bak-* in DEST_DIR, deleting older ones. Lexical sort matches time
# order for the fixed-width UTC timestamp backups use (YYYYMMDDTHHMMSSZ).
rotate_backups() {
  local dest_dir=$1 name=$2
  local backups=() f
  for f in "$dest_dir/$name".bak-*; do
    [ -e "$f" ] || continue
    backups+=("$f")
  done
  local count=${#backups[@]}
  [ "$count" -le "$BACKUP_KEEP" ] && return 0
  local excess=$((count - BACKUP_KEEP)) old
  while IFS= read -r old; do
    [ -n "$old" ] && "${INSTALL_SUDO[@]}" rm -f "$old"
  done < <(printf '%s\n' "${backups[@]}" | sort | head -n "$excess")
}

# atomic_install SRC DESTDIR NAME -- copies SRC into DESTDIR/NAME via a
# same-directory temp file + rename, so DESTDIR/NAME is never observed
# truncated or half-written. An existing DESTDIR/NAME is backed up first as
# NAME.bak-<UTC timestamp> rather than silently overwritten (rotate_backups
# then prunes to the newest BACKUP_KEEP). Filesystem operations run through
# INSTALL_SUDO (empty unless --system needed elevation).
atomic_install() {
  local src=$1 dest_dir=$2 name=$3
  local tmp
  "${INSTALL_SUDO[@]}" mkdir -p "$dest_dir"
  tmp="$dest_dir/.orchard-install-tmp-$name-$$"
  "${INSTALL_SUDO[@]}" cp "$src" "$tmp"
  "${INSTALL_SUDO[@]}" chmod 755 "$tmp"
  if [ -e "$dest_dir/$name" ]; then
    "${INSTALL_SUDO[@]}" mv -f "$dest_dir/$name" "$dest_dir/$name.bak-$(date -u +%Y%m%dT%H%M%SZ)"
    rotate_backups "$dest_dir" "$name"
  fi
  "${INSTALL_SUDO[@]}" mv -f "$tmp" "$dest_dir/$name"
}

# find_local_checkout -- prints a repo root if the cwd or this script's own
# parent directory looks like an orchard checkout; fails (empty stdout,
# nonzero exit) otherwise, so callers fall back to a fresh clone.
find_local_checkout() {
  local candidate
  for candidate in "$PWD" "$(cd "${SCRIPT_DIR:-.}/.." 2>/dev/null && pwd || true)"; do
    if [ -n "$candidate" ] && [ -f "$candidate/Cargo.toml" ] && [ -d "$candidate/cmd" ] && [ -f "$candidate/Makefile" ]; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}

# build_from_source -- builds the suite locally (current checkout if found,
# else a fresh clone) and sets FROM_SOURCE_DIR + RESOLVED_TAG.
build_from_source() {
  local src ver
  command -v go >/dev/null 2>&1 || fail "--from-source requires go (not found on PATH)"
  command -v cargo >/dev/null 2>&1 || fail "--from-source requires cargo (not found on PATH)"

  if src=$(find_local_checkout); then
    :
  else
    command -v git >/dev/null 2>&1 || fail "--from-source needs a local checkout or git (neither found)"
    resolve_repo_target
    src=$(mktemp -d)
    CLEANUP_PATHS+=("$src")
    if [ -n "$VERSION" ]; then
      git clone --depth 1 --branch "$(normalize_tag "$VERSION")" "https://github.com/$REPO_SLUG.git" "$src" || fail "git clone --branch $(normalize_tag "$VERSION") failed"
    else
      git clone --depth 1 "https://github.com/$REPO_SLUG.git" "$src" || fail "git clone failed"
    fi
  fi

  ( cd "$src" && make dispatcher rust daemon sidebar shell upgrade ) || fail "build failed in $src"
  FROM_SOURCE_DIR="$src"
  ver=$(awk -F'"' '/^version = / { print $2; exit }' "$src/crates/orchard/Cargo.toml" 2>/dev/null || true)
  RESOLVED_TAG="${ver:-unknown} (from source)"
}

# install_service PREFIX -- rewrites scripts/init/orchard.service's
# ExecStart to PREFIX and installs it as a systemd user unit. Not gated on
# uname: writing the unit file is harmless on a non-Linux host (nothing
# reads ~/.config/systemd/user there), and the only Linux-specific step --
# `systemctl --user daemon-reload` -- is naturally skipped wherever
# systemctl isn't on PATH. --no-service is the explicit opt-out.
install_service() {
  local prefix=$1
  local src="" dest_dir tmp

  if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/init/orchard.service" ]; then
    src="$SCRIPT_DIR/init/orchard.service"
  else
    local ref raw_tmp
    resolve_repo_target
    ref="${RESOLVED_TAG:-main}"
    raw_tmp=$(mktemp)
    CLEANUP_PATHS+=("$raw_tmp")
    if curl -fsSL -o "$raw_tmp" "https://raw.githubusercontent.com/$REPO_SLUG/$ref/scripts/init/orchard.service" 2>/dev/null; then
      src="$raw_tmp"
    fi
  fi

  if [ -z "$src" ]; then
    echo "note: scripts/init/orchard.service not found locally or remotely, skipping service install" >&2
    return 1
  fi

  dest_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
  mkdir -p "$dest_dir"
  tmp="$dest_dir/.orchard-service-tmp-$$"
  sed "s#^ExecStart=.*#ExecStart=$prefix/orchard daemon start#" "$src" >"$tmp"
  mv -f "$tmp" "$dest_dir/orchard.service"

  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload 2>/dev/null || echo "note: systemctl --user daemon-reload failed (non-fatal)" >&2
  fi
  return 0
}

# detect_active_service -- prints the first SERVICE_UNITS member that's an
# active systemd --user unit (priority order); fails (empty stdout) if
# systemctl is missing or none are active. Always run as the invoking user,
# never through sudo -- systemd --user is per-uid, and root's session isn't
# the real user's.
detect_active_service() {
  command -v systemctl >/dev/null 2>&1 || return 1
  local unit
  for unit in "${SERVICE_UNITS[@]}"; do
    if systemctl --user is-active "$unit" >/dev/null 2>&1; then
      printf '%s' "$unit"
      return 0
    fi
  done
  return 1
}

# handle_service PREFIX -- restarts whichever SERVICE_UNITS member is
# already active; if none are, falls back to install_service's template
# install. Sets SERVICE_ACTION to "restarted:<unit>" or "installed" on
# success. Returns 1 (SERVICE_ACTION left "") if neither applies.
handle_service() {
  local prefix=$1 active
  if active=$(detect_active_service); then
    systemctl --user restart "$active"
    SERVICE_ACTION="restarted:$active"
    return 0
  fi
  if install_service "$prefix"; then
    SERVICE_ACTION="installed"
    return 0
  fi
  return 1
}

# compute_path_warning PREFIX -- prints a remedy message when PREFIX isn't
# on $PATH; prints nothing otherwise.
compute_path_warning() {
  local prefix=$1
  case ":$PATH:" in
    *":$prefix:"*) return 0 ;;
  esac
  # shellcheck disable=SC2016  # literal $PATH shown to the user, not expanded here
  printf 'add %s to your $PATH (e.g. echo '"'"'export PATH="%s:$PATH"'"'"' >> ~/.profile)' "$prefix" "$prefix"
}

emit_success() {
  local triple=$1 prefix=$2 tag=$3
  shift 3
  local names=("$@")
  local path_warning
  path_warning=$(compute_path_warning "$prefix")
  local service_installed=0
  [ -n "$SERVICE_ACTION" ] && service_installed=1

  if [ "$JSON_MODE" -eq 1 ]; then
    local bins_json="" n
    for n in "${names[@]:-}"; do
      [ -z "$n" ] && continue
      [ -n "$bins_json" ] && bins_json="$bins_json,"
      bins_json="$bins_json\"$(json_escape "$n")\""
    done
    local path_warning_json="null"
    [ -n "$path_warning" ] && path_warning_json="\"$(json_escape "$path_warning")\""
    printf '{"ok":true,"data":{"version":"%s","prefix":"%s","triple":"%s","binaries":[%s],"from_source":%s,"service_installed":%s,"path_warning":%s},"error":null}\n' \
      "$(json_escape "$tag")" "$(json_escape "$prefix")" "$(json_escape "$triple")" "$bins_json" \
      "$([ "$FROM_SOURCE" -eq 1 ] && echo true || echo false)" \
      "$([ "$service_installed" -eq 1 ] && echo true || echo false)" \
      "$path_warning_json"
    JSON_EMITTED=1
  else
    echo "installed orchard $tag to $prefix ($triple)"
    echo "binaries: ${names[*]}"
    case "$SERVICE_ACTION" in
      restarted:*) echo "systemd user unit restarted: ${SERVICE_ACTION#restarted:}" ;;
      installed) echo "systemd user unit installed: ${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/orchard.service" ;;
    esac
    [ -n "$path_warning" ] && echo "note: $path_warning"
  fi
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --version)
        VERSION=$2
        shift 2
        ;;
      --version=*)
        VERSION="${1#*=}"
        shift
        ;;
      --prefix)
        PREFIX=$2
        shift 2
        ;;
      --prefix=*)
        PREFIX="${1#*=}"
        shift
        ;;
      --system)
        SYSTEM=1
        shift
        ;;
      --from-source)
        FROM_SOURCE=1
        shift
        ;;
      --no-service)
        NO_SERVICE=1
        shift
        ;;
      --json)
        JSON_MODE=1
        shift
        ;;
      -h | --help)
        usage
        exit 0
        ;;
      *)
        fail "unknown argument: $1"
        ;;
    esac
  done
}

main() {
  parse_args "$@"

  local triple prefix staged_dir
  triple=$(detect_triple)
  prefix=$(resolve_prefix)
  check_root_owned_conflict "$prefix"
  resolve_install_sudo

  if [ "$FROM_SOURCE" -eq 1 ]; then
    build_from_source
    staged_dir="$FROM_SOURCE_DIR"
    local stage
    stage=$(mktemp -d)
    CLEANUP_PATHS+=("$stage")
    [ -f "$FROM_SOURCE_DIR/target/release/orchard" ] && cp "$FROM_SOURCE_DIR/target/release/orchard" "$stage/"
    [ -f "$FROM_SOURCE_DIR/target/release/orchard-tui" ] && cp "$FROM_SOURCE_DIR/target/release/orchard-tui" "$stage/"
    local gobin
    for gobin in orchard-daemon orchard-sidebar orchard-shell orchard-upgrade; do
      [ -f "$FROM_SOURCE_DIR/bin/$gobin" ] && cp "$FROM_SOURCE_DIR/bin/$gobin" "$stage/"
    done
    staged_dir="$stage"
  else
    local work
    work=$(mktemp -d)
    CLEANUP_PATHS+=("$work")
    resolve_release_assets_or_base_url "$triple"
    verify_and_stage "$triple" "$work"
    staged_dir="$STAGED_DIR"
  fi

  local installed_names=() name
  for name in "${SUITE_BINARIES[@]}"; do
    if [ -f "$staged_dir/$name" ]; then
      atomic_install "$staged_dir/$name" "$prefix" "$name"
      installed_names+=("$name")
    fi
  done
  [ "${#installed_names[@]}" -gt 0 ] || fail "no suite binaries found to install"

  if [ "$NO_SERVICE" -eq 0 ]; then
    handle_service "$prefix" || true
  fi

  emit_success "$triple" "$prefix" "$RESOLVED_TAG" "${installed_names[@]}"
}

main "$@"
