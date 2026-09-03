#!/usr/bin/env bash
# dist.sh — assembles dist/: per-binary tarballs, per-triple
# orchard-suite-<triple>.tar.gz, and an aggregate SHA256SUMS. Invoked by
# `make dist` (#747 Step 6).
#
# Go binaries cross-compile unconditionally (CGO_ENABLED=0, no toolchain
# dependency). Rust binaries use build_rust_pair's fallback chain: native
# direct build (host triple, or an already-installed rustup target) ->
# cargo-zigbuild -> cross -> docker. A triple's suite tarball is assembled
# only when every binary in SUITE_BINS built for it; otherwise that triple
# is skipped with a warning (per-binary tarballs for whichever binaries DID
# build are still written, so a partial local run still produces something
# usable).
#
# GOOS/GOARCH<->triple pairs and the six-binary suite roster mirror
# internal/release/assets.go (`triples`, `SuiteBinaries`) -- the Go upgrade
# client's ground truth for what a release must contain. The dispatcher's
# tarball name/contents (orchard-<triple>.tar.gz, containing one file named
# `orchard`) is unchanged: npm/install.js hardcodes it (plan AC12).
#
# Usage: scripts/dist.sh [VERSION] [--only TRIPLE]
#   VERSION   defaults to the VERSION env var, else the version in
#             crates/orchard/Cargo.toml, else "dev"
#   --only    limit the run to one PLATFORMS triple, e.g.
#             --only aarch64-unknown-linux-gnu (also: make dist TRIPLE=...)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"

# derive_version -- mirrors the Makefile's own Cargo.toml-derived default
# (VERSION ?= ... crates/orchard/Cargo.toml), so a direct invocation of
# this script (bypassing `make dist`) still bakes a real version into the
# Go binaries instead of the "dev" placeholder.
derive_version() {
  awk -F'"' '/^version = / { print $2; exit }' "$ROOT/crates/orchard/Cargo.toml" 2>/dev/null || true
}

VERSION="${VERSION:-$(derive_version)}"
VERSION="${VERSION:-dev}"

# REVISION is the VCS commit baked into every Go binary so doctor's
# suite-revisions check can compare builds (orchardist#803). Empty when the
# source tree is not a git checkout (a downloaded tarball); the compiler's own
# vcs.revision stamp then fills in where it can.
REVISION="${REVISION:-$(cd "$ROOT" && git rev-parse HEAD 2>/dev/null || true)}"
GO_LDFLAGS="-X main.version=$VERSION -X github.com/drewdrewthis/orchardist/internal/release.revision=$REVISION"

ONLY_TRIPLE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --only)
      ONLY_TRIPLE=$2
      shift 2
      ;;
    --only=*)
      ONLY_TRIPLE="${1#*=}"
      shift
      ;;
    *)
      VERSION="$1"
      shift
      ;;
  esac
done

rm -rf "$DIST"
mkdir -p "$DIST"

# "goos goarch triple" rows -- must match internal/release/assets.go's
# `triples` map.
PLATFORMS=(
  "linux amd64 x86_64-unknown-linux-gnu"
  "linux arm64 aarch64-unknown-linux-gnu"
  "darwin amd64 x86_64-apple-darwin"
  "darwin arm64 aarch64-apple-darwin"
)

if [ -n "$ONLY_TRIPLE" ]; then
  filtered=()
  for entry in "${PLATFORMS[@]}"; do
    read -r _ _ t <<<"$entry"
    [ "$t" = "$ONLY_TRIPLE" ] && filtered+=("$entry")
  done
  if [ "${#filtered[@]}" -eq 0 ]; then
    echo "error: --only $ONLY_TRIPLE is not a known triple (see PLATFORMS in scripts/dist.sh)" >&2
    exit 1
  fi
  PLATFORMS=("${filtered[@]}")
fi

GO_BINS=(orchard-daemon orchard-sidebar orchard-shell orchard-upgrade)

installed_rust_targets="$(rustup target list --installed 2>/dev/null || true)"
host_triple="$(rustc -vV 2>/dev/null | sed -n 's/^host: //p')"

# build_rust_native TRIPLE PLATFORM_DIR -- plain `cargo build --target`,
# valid when TRIPLE needs no cross linker: it's the host triple, or its
# rustup target is already installed with a working linker for it.
build_rust_native() {
  local triple=$1 platform_dir=$2
  rustup target add "$triple" >/dev/null 2>&1 || true
  ( cd "$ROOT" && cargo build --release --target "$triple" -p orchard-dispatcher -p orchard ) || return 1
  cp "$ROOT/target/$triple/release/orchard" "$platform_dir/orchard" || return 1
  cp "$ROOT/target/$triple/release/orchard-tui" "$platform_dir/orchard-tui" || return 1
}

# build_via_zigbuild TRIPLE PLATFORM_DIR -- cargo-zigbuild, using zig as a
# cross-capable linker. Only attempted if cargo-zigbuild is already
# installed, or zig is present (in which case `cargo install cargo-zigbuild`
# is allowed -- zig itself is never installed by this script).
build_via_zigbuild() {
  local triple=$1 platform_dir=$2
  if ! command -v cargo-zigbuild >/dev/null 2>&1; then
    command -v zig >/dev/null 2>&1 || return 1
    cargo install cargo-zigbuild >/dev/null 2>&1 || return 1
  fi
  rustup target add "$triple" >/dev/null 2>&1 || return 1
  ( cd "$ROOT" && cargo zigbuild --release --target "$triple" -p orchard-dispatcher -p orchard ) || return 1
  cp "$ROOT/target/$triple/release/orchard" "$platform_dir/orchard" || return 1
  cp "$ROOT/target/$triple/release/orchard-tui" "$platform_dir/orchard-tui" || return 1
}

# build_via_cross TRIPLE PLATFORM_DIR -- the `cross` cargo subcommand, which
# manages its own build containers. Only attempted if already installed.
build_via_cross() {
  local triple=$1 platform_dir=$2
  command -v cross >/dev/null 2>&1 || return 1
  ( cd "$ROOT" && cross build --release --target "$triple" -p orchard-dispatcher -p orchard ) || return 1
  cp "$ROOT/target/$triple/release/orchard" "$platform_dir/orchard" || return 1
  cp "$ROOT/target/$triple/release/orchard-tui" "$platform_dir/orchard-tui" || return 1
}

# build_via_docker TRIPLE PLATFORM_DIR -- last resort: build inside the
# official rust:1-bookworm image for TRIPLE's OS/arch, via a running docker
# daemon (e.g. colima). The image's --platform variant makes the container
# arch match the target, so this is a NATIVE build inside the container, not
# a cross one -- no extra linker setup needed. Source is bind-mounted
# read-only; only the target/ cache dir is writable, so the build cannot
# touch anything outside its own output.
build_via_docker() {
  local triple=$1 platform_dir=$2
  local docker_platform
  case "$triple" in
    x86_64-unknown-linux-gnu) docker_platform="linux/amd64" ;;
    aarch64-unknown-linux-gnu) docker_platform="linux/arm64" ;;
    *) return 1 ;; # no Linux container image mapping (e.g. a darwin triple)
  esac
  command -v docker >/dev/null 2>&1 || return 1
  docker info >/dev/null 2>&1 || return 1

  local cache="$ROOT/target/docker-cross/$triple"
  mkdir -p "$cache/.cargo-home"

  docker run --rm --platform "$docker_platform" \
    -u "$(id -u):$(id -g)" \
    -e HOME=/tmp \
    -e CARGO_HOME=/repo/target/.cargo-home \
    -v "$ROOT:/repo:ro" \
    -v "$cache:/repo/target" \
    -w /repo \
    rust:1-bookworm \
    cargo build --release --target "$triple" -p orchard-dispatcher -p orchard || return 1

  cp "$cache/$triple/release/orchard" "$platform_dir/orchard" || return 1
  cp "$cache/$triple/release/orchard-tui" "$platform_dir/orchard-tui" || return 1
}

# build_rust_pair TRIPLE PLATFORM_DIR -- builds orchard + orchard-tui for
# TRIPLE into PLATFORM_DIR via the first working method: native (host triple
# or already-installed rustup target) -> cargo-zigbuild -> cross -> docker.
# Prints which method won; returns 1 with no output files if every
# applicable method failed or none applied.
build_rust_pair() {
  local triple=$1 platform_dir=$2

  if [ "$triple" = "$host_triple" ] && build_rust_native "$triple" "$platform_dir"; then
    echo "  rust: built natively for $triple (host triple)"
    return 0
  fi

  if [ "$triple" != "$host_triple" ] && grep -qx "$triple" <<<"$installed_rust_targets" \
      && build_rust_native "$triple" "$platform_dir"; then
    echo "  rust: built via already-installed rustup target for $triple"
    return 0
  fi

  if build_via_zigbuild "$triple" "$platform_dir"; then
    echo "  rust: built via cargo-zigbuild for $triple"
    return 0
  fi

  if build_via_cross "$triple" "$platform_dir"; then
    echo "  rust: built via cross for $triple"
    return 0
  fi

  if build_via_docker "$triple" "$platform_dir"; then
    echo "  rust: built via docker (rust:1-bookworm) for $triple"
    return 0
  fi

  echo "  rust: no working build method for $triple (tried: native/rustup-installed-target, cargo-zigbuild, cross, docker)"
  echo "  rust:   to enable: rustup target add $triple; or install zig (we'll cargo install cargo-zigbuild); or install cross; or start docker (e.g. colima start)"
  return 1
}

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for entry in "${PLATFORMS[@]}"; do
  read -r goos goarch triple <<<"$entry"
  echo "== $triple (GOOS=$goos GOARCH=$goarch) =="

  platform_dir="$work/$triple"
  mkdir -p "$platform_dir"
  have_go=1
  have_rust=1

  # --- Go binaries: always attempted, no toolchain dependency. ---
  for bin in "${GO_BINS[@]}"; do
    src="cmd/$bin"
    if [[ ! -d "$ROOT/$src" ]]; then
      echo "  skip $bin: $src does not exist yet"
      have_go=0
      continue
    fi
    out="$platform_dir/$bin"
    if ! (cd "$ROOT" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
        go build -ldflags "$GO_LDFLAGS" -o "$out" "./$src" 2>&1); then
      echo "  FAILED building $bin for $triple"
      have_go=0
      continue
    fi
    tarball="$DIST/$bin-$triple.tar.gz"
    tar czf "$tarball" -C "$platform_dir" "$bin"
    echo "  wrote $(basename "$tarball")"
  done

  # --- Rust binaries: build_rust_pair's native -> zigbuild -> cross ->
  # docker fallback chain. ---
  if build_rust_pair "$triple" "$platform_dir"; then
    tar czf "$DIST/orchard-$triple.tar.gz" -C "$platform_dir" orchard
    echo "  wrote orchard-$triple.tar.gz"
    tar czf "$DIST/orchard-tui-$triple.tar.gz" -C "$platform_dir" orchard-tui
    echo "  wrote orchard-tui-$triple.tar.gz"
  else
    have_rust=0
  fi

  # --- Suite tarball: only when every binary built for this triple. ---
  if [[ "$have_go" -eq 1 && "$have_rust" -eq 1 ]]; then
    suite_dir="$work/suite-$triple"
    mkdir -p "$suite_dir"
    for bin in "${GO_BINS[@]}" orchard-tui orchard; do
      cp "$platform_dir/$bin" "$suite_dir/$bin"
    done
    tar czf "$DIST/orchard-suite-$triple.tar.gz" -C "$suite_dir" .
    echo "  wrote orchard-suite-$triple.tar.gz"
  else
    echo "  WARNING: skipping orchard-suite-$triple.tar.gz (missing Go and/or Rust binaries for $triple)"
  fi
done

# --- Aggregate checksums over everything produced. ---
if compgen -G "$DIST"/*.tar.gz >/dev/null; then
  (
    cd "$DIST"
    if command -v shasum >/dev/null 2>&1; then
      shasum -a 256 -- *.tar.gz > SHA256SUMS
    else
      sha256sum -- *.tar.gz > SHA256SUMS
    fi
  )
  echo "wrote SHA256SUMS ($(wc -l < "$DIST/SHA256SUMS" | tr -d ' ') entries)"
else
  echo "no tarballs produced; skipping SHA256SUMS"
fi

# --- Self-check: prove every built Go binary actually baked in $VERSION.
# `go version -m` reads a binary's embedded build info (including ldflags)
# without executing it, so this works on foreign-arch cross binaries too.
# Catches a direct `bash scripts/dist.sh` invocation where VERSION landed
# on "dev" (env unset, Cargo.toml unreadable), or a future regression that
# stops forwarding VERSION into the build. ---
version_check_failed=0
for entry in "${PLATFORMS[@]}"; do
  read -r _ _ triple <<<"$entry"
  platform_dir="$work/$triple"
  for bin in "${GO_BINS[@]}"; do
    bin_path="$platform_dir/$bin"
    [ -f "$bin_path" ] || continue
    if ! go version -m "$bin_path" 2>/dev/null | grep -qF -- "-X main.version=$VERSION"; then
      echo "ERROR: $bin ($triple) did not bake in VERSION=$VERSION" >&2
      version_check_failed=1
    fi
  done
done
if [ "$version_check_failed" -eq 1 ]; then
  echo "error: one or more Go binaries do not report VERSION=$VERSION (see above)" >&2
  exit 1
fi
echo "verified: all built Go binaries report VERSION=$VERSION"
