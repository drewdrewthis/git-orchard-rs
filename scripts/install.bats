#!/usr/bin/env bats
bats_require_minimum_version 1.5.0

# install.sh behavior, hermetic: ORCHARD_RELEASE_BASE_URL points at a
# file://-served fixture suite tarball (curl supports file:// URLs, so no
# HTTP server or GitHub access is needed) and a stub systemctl on PATH
# records every call and plays back a scripted active unit -- so the
# service detect/restart/install-fallback, PATH warning, and backup
# rotation behaviors are pinned without a live systemd or real releases.
#
# Covers: systemd unit detection + priority + --no-service, --json
# path_warning, atomic_install's backup-before-replace + rotation,
# idempotent re-run (unchanged/updated/installed actions + changed count),
# --json's stderr-only progress lines, and the http-downgrade rejection for
# ORCHARD_RELEASE_REPO/ORCHARD_RELEASE_BASE_URL.
# NOT covered here: the --system + sudo -n elevation path (needs real root
# to exercise meaningfully; the root-owned-prefix guard and sudo -n
# fallback are implemented in install.sh but left to manual/box testing).

# host_triple_for_fixture -- duplicates install.sh's own detect_triple()
# uname mapping, so the fixture tarball is named for whatever host actually
# runs this test file (install.sh's triple detection itself isn't
# stubbable).
host_triple_for_fixture() {
  local os arch
  os=$(uname -s)
  arch=$(uname -m)
  case "$os" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
  esac
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
  esac
  case "$os/$arch" in
    linux/amd64) echo x86_64-unknown-linux-gnu ;;
    linux/arm64) echo aarch64-unknown-linux-gnu ;;
    darwin/amd64) echo x86_64-apple-darwin ;;
    darwin/arm64) echo aarch64-apple-darwin ;;
  esac
}

# sha256_of FILE -- portable sha256 hex digest, mirroring install.sh's own
# sha256_file dual-tool support (sha256sum Linux, shasum -a 256 macOS).
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# mtime_of FILE -- portable mtime as an epoch integer (GNU stat -c, BSD
# stat -f), mirroring install.sh's own file_owner_uid dual-tool pattern.
mtime_of() {
  stat -c '%Y' "$1" 2>/dev/null || stat -f '%m' "$1" 2>/dev/null
}

setup() {
  SCRIPT="$BATS_TEST_DIRNAME/install.sh"
  TRIPLE="$(host_triple_for_fixture)"

  STUB_DIR="$(mktemp -d)"
  export SYSTEMCTL_LOG="$STUB_DIR/systemctl.log"
  : > "$SYSTEMCTL_LOG"
  export STUB_ACTIVE_UNIT=""

  cat > "$STUB_DIR/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$SYSTEMCTL_LOG"
if [ "$1" = "--user" ] && [ "$2" = "is-active" ]; then
  [ "$3" = "$STUB_ACTIVE_UNIT" ] && exit 0
  exit 3
fi
exit 0
EOF
  chmod +x "$STUB_DIR/systemctl"

  RELEASE_DIR="$(mktemp -d)"
  BUILD_DIR="$(mktemp -d)"
  PREFIX_DIR="$(mktemp -d)"
  FAKE_HOME="$(mktemp -d)"

  # Minimal fixture "binary" -- content is irrelevant, only presence and a
  # correct checksum matter to verify_and_stage/atomic_install.
  printf '#!/usr/bin/env bash\necho fake-orchard-daemon\n' > "$BUILD_DIR/orchard-daemon"
  chmod +x "$BUILD_DIR/orchard-daemon"
  ( cd "$BUILD_DIR" && tar czf "$RELEASE_DIR/orchard-suite-$TRIPLE.tar.gz" orchard-daemon )
  if command -v shasum >/dev/null 2>&1; then
    ( cd "$RELEASE_DIR" && shasum -a 256 "orchard-suite-$TRIPLE.tar.gz" > SHA256SUMS )
  else
    ( cd "$RELEASE_DIR" && sha256sum "orchard-suite-$TRIPLE.tar.gz" > SHA256SUMS )
  fi

  export ORCHARD_RELEASE_BASE_URL="file://$RELEASE_DIR"
  export XDG_CONFIG_HOME="$FAKE_HOME/.config"
  export HOME="$FAKE_HOME"
  export PATH="$STUB_DIR:$PATH"
}

teardown() {
  rm -rf "$STUB_DIR" "$RELEASE_DIR" "$BUILD_DIR" "$PREFIX_DIR" "$FAKE_HOME"
}

# --- systemd unit detection -------------------------------------------

@test "no active service unit: falls back to installing the template" {
  export STUB_ACTIVE_UNIT=""
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  [ -x "$PREFIX_DIR/orchard-daemon" ]
  grep -qF '"service_installed":true' <<<"$output"
  [ -f "$FAKE_HOME/.config/systemd/user/orchard.service" ]
  grep -qF "ExecStart=$PREFIX_DIR/orchard daemon start" "$FAKE_HOME/.config/systemd/user/orchard.service"
  grep -qF "daemon-reload" "$SYSTEMCTL_LOG"
  ! grep -qF "restart" "$SYSTEMCTL_LOG"
}

@test "orchard-daemon.service active: restarted, template not (re)installed" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF -- "--user restart orchard-daemon.service" "$SYSTEMCTL_LOG"
  ! grep -qF "orchard.service" "$SYSTEMCTL_LOG"
  [ ! -f "$FAKE_HOME/.config/systemd/user/orchard.service" ]
  grep -qF '"service_installed":true' <<<"$output"
}

@test "orchard.service active (no orchard-daemon.service): restarted, priority order honoured" {
  export STUB_ACTIVE_UNIT="orchard.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  # both units are checked (daemon-first priority) before the match...
  grep -qF -- "--user is-active orchard-daemon.service" "$SYSTEMCTL_LOG"
  grep -qF -- "--user is-active orchard.service" "$SYSTEMCTL_LOG"
  # ...but only the active one is restarted.
  grep -qF -- "--user restart orchard.service" "$SYSTEMCTL_LOG"
  ! grep -qF -- "--user restart orchard-daemon.service" "$SYSTEMCTL_LOG"
}

@test "--no-service: systemctl never invoked" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --no-service --json
  [ "$status" -eq 0 ]
  [ ! -s "$SYSTEMCTL_LOG" ]
  grep -qF '"service_installed":false' <<<"$output"
}

# --- --json path_warning ------------------------------------------------

@test "path_warning: set (with a remedy) when --prefix is not on PATH" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF "\"path_warning\":\"add $PREFIX_DIR to your \$PATH" <<<"$output"
}

@test "path_warning: null when --prefix is already on PATH" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  export PATH="$PREFIX_DIR:$PATH"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF '"path_warning":null' <<<"$output"
}

@test "path_warning: human text mode prints a note line" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR"
  [ "$status" -eq 0 ]
  grep -qF "note: add $PREFIX_DIR to your \$PATH" <<<"$output"
}

# --- --json plugin hint (issue #772) --------------------------------------

# write_plugins_manifest [KEY] -- writes $FAKE_HOME/.claude/plugins/installed_plugins.json,
# optionally containing the given plugin key.
write_plugins_manifest() {
  mkdir -p "$FAKE_HOME/.claude/plugins"
  if [ -n "${1:-}" ]; then
    printf '{"version":3,"plugins":{"%s":[{"scope":"user"}]}}' "$1" > "$FAKE_HOME/.claude/plugins/installed_plugins.json"
  else
    printf '{"version":3,"plugins":{"other@foo":[{"scope":"user"}]}}' > "$FAKE_HOME/.claude/plugins/installed_plugins.json"
  fi
}

@test "hints: present with the install remedy when the plugin manifest is missing" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF '"hints":["sidebar cards need the claude-session-state plugin' <<<"$output"
  grep -qF '/plugin marketplace add drewdrewthis/orchardist' <<<"$output"
}

@test "hints: present when the manifest exists but lacks the plugin key" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  write_plugins_manifest
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF '"hints":["sidebar cards need the claude-session-state plugin' <<<"$output"
}

@test "hints: empty when the plugin key is present" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  write_plugins_manifest "claude-session-state@orchardist"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF '"hints":[]' <<<"$output"
}

@test "hints: human text mode prints a hint line when the plugin is missing" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR"
  [ "$status" -eq 0 ]
  grep -qF "hint: sidebar cards need the claude-session-state plugin" <<<"$output"
}

@test "hints: human text mode prints no hint line when the plugin is present" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  write_plugins_manifest "claude-session-state@orchardist"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR"
  [ "$status" -eq 0 ]
  ! grep -qF "hint:" <<<"$output"
}

# --- backup rotation ------------------------------------------------------

@test "backup rotation: fresh install creates no backup" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  local count
  count=$(find "$PREFIX_DIR" -maxdepth 1 -name 'orchard-daemon.bak-*' | wc -l | tr -d ' ')
  [ "$count" -eq 0 ]
}

@test "backup rotation: existing binary backed up, oldest pruned beyond 3" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  printf 'old-1' > "$PREFIX_DIR/orchard-daemon.bak-20260101T000000Z"
  printf 'old-2' > "$PREFIX_DIR/orchard-daemon.bak-20260102T000000Z"
  printf 'old-3' > "$PREFIX_DIR/orchard-daemon.bak-20260103T000000Z"
  printf 'live'  > "$PREFIX_DIR/orchard-daemon"

  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]

  local remaining
  remaining=$(find "$PREFIX_DIR" -maxdepth 1 -name 'orchard-daemon.bak-*' | wc -l | tr -d ' ')
  [ "$remaining" -eq 3 ]
  [ ! -e "$PREFIX_DIR/orchard-daemon.bak-20260101T000000Z" ]
  [ -e "$PREFIX_DIR/orchard-daemon.bak-20260102T000000Z" ]
  [ -e "$PREFIX_DIR/orchard-daemon.bak-20260103T000000Z" ]

  local today
  today="$(date -u +%Y%m%d)"
  find "$PREFIX_DIR" -maxdepth 1 -name "orchard-daemon.bak-${today}T*" | grep -q .
}

# --- idempotent re-run ----------------------------------------------------

@test "idempotent re-run: second identical run reports unchanged, no new backups, mtime stable" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF '"name":"orchard-daemon","action":"installed"' <<<"$output"

  local hash1 mtime1
  hash1=$(sha256_of "$PREFIX_DIR/orchard-daemon")
  mtime1=$(mtime_of "$PREFIX_DIR/orchard-daemon")
  : > "$SYSTEMCTL_LOG"

  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF '"name":"orchard-daemon","action":"unchanged"' <<<"$output"
  grep -qF '"changed":0' <<<"$output"

  local hash2 mtime2
  hash2=$(sha256_of "$PREFIX_DIR/orchard-daemon")
  mtime2=$(mtime_of "$PREFIX_DIR/orchard-daemon")
  [ "$hash1" = "$hash2" ]
  [ "$mtime1" = "$mtime2" ]

  local count
  count=$(find "$PREFIX_DIR" -maxdepth 1 -name 'orchard-daemon.bak-*' | wc -l | tr -d ' ')
  [ "$count" -eq 0 ]
  ! grep -qF "restart" "$SYSTEMCTL_LOG"
}

@test "tamper one binary locally: only that one reports updated, gets exactly one backup" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"

  # Rebuild the fixture with a second binary so we can prove only the
  # tampered one moves.
  printf '#!/usr/bin/env bash\necho fake-orchard-sidebar\n' > "$BUILD_DIR/orchard-sidebar"
  chmod +x "$BUILD_DIR/orchard-sidebar"
  ( cd "$BUILD_DIR" && tar czf "$RELEASE_DIR/orchard-suite-$TRIPLE.tar.gz" orchard-daemon orchard-sidebar )
  if command -v shasum >/dev/null 2>&1; then
    ( cd "$RELEASE_DIR" && shasum -a 256 "orchard-suite-$TRIPLE.tar.gz" > SHA256SUMS )
  else
    ( cd "$RELEASE_DIR" && sha256sum "orchard-suite-$TRIPLE.tar.gz" > SHA256SUMS )
  fi

  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]

  # Tamper with just orchard-daemon locally.
  printf 'tampered-content' > "$PREFIX_DIR/orchard-daemon"
  : > "$SYSTEMCTL_LOG"

  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]
  grep -qF '"name":"orchard-daemon","action":"updated"' <<<"$output"
  grep -qF '"name":"orchard-sidebar","action":"unchanged"' <<<"$output"
  grep -qF '"changed":1' <<<"$output"

  local count
  count=$(find "$PREFIX_DIR" -maxdepth 1 -name 'orchard-daemon.bak-*' | wc -l | tr -d ' ')
  [ "$count" -eq 1 ]
  count=$(find "$PREFIX_DIR" -maxdepth 1 -name 'orchard-sidebar.bak-*' | wc -l | tr -d ' ')
  [ "$count" -eq 0 ]
}

# --- progress output --------------------------------------------------

@test "--json: progress lines go to stderr, stdout is exactly one JSON object" {
  export STUB_ACTIVE_UNIT="orchard-daemon.service"
  run --separate-stderr bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -eq 0 ]

  echo "$output" | jq -e . >/dev/null
  [ "$(echo "$output" | wc -l | tr -d ' ')" -eq 1 ]

  grep -qF "resolving release" <<<"$stderr"
  grep -qF "downloading" <<<"$stderr"
  grep -qF "verifying checksums" <<<"$stderr"
  grep -qF "installing" <<<"$stderr"
  grep -qF "restarting" <<<"$stderr"
}

# --- http downgrade protection ------------------------------------------

@test "ORCHARD_RELEASE_BASE_URL: plain http from a non-loopback host is rejected" {
  export ORCHARD_RELEASE_BASE_URL="http://example.invalid/releases"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -ne 0 ]
  grep -qF "refusing plain http" <<<"$output"
}

@test "ORCHARD_RELEASE_BASE_URL: plain http on loopback passes the scheme gate" {
  export ORCHARD_RELEASE_BASE_URL="http://127.0.0.1:1/releases"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -ne 0 ]
  ! grep -qF "refusing plain http" <<<"$output"
}

@test "ORCHARD_RELEASE_ALLOW_HTTP=1: opts a non-loopback plain http host back in" {
  export ORCHARD_RELEASE_BASE_URL="http://example.invalid/releases"
  export ORCHARD_RELEASE_ALLOW_HTTP=1
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -ne 0 ]
  ! grep -qF "refusing plain http" <<<"$output"
}

@test "ORCHARD_RELEASE_REPO: plain http absolute URL from a non-loopback host is rejected" {
  unset ORCHARD_RELEASE_BASE_URL
  export ORCHARD_RELEASE_REPO="http://example.invalid"
  run bash "$SCRIPT" --prefix "$PREFIX_DIR" --json
  [ "$status" -ne 0 ]
  grep -qF "refusing plain http" <<<"$output"
}

# --- check_root_owned_conflict: explicit --prefix vs implicit fallback ---
# Sources install.sh's function/variable definitions only (drops the
# trailing `main "$@"` so nothing actually runs) directly into this test's
# own bats subshell, then stubs `id` (force non-root) and
# root_owned_suite_binary (report a hit only for /usr/local/bin) to
# exercise check_root_owned_conflict in isolation -- a real root-owned
# /usr/local/bin/orchard-daemon can't be fixtured without actual root.

@test "check_root_owned_conflict: PREFIX unset (implicit fallback) still flags a root-owned /usr/local/bin" {
  # shellcheck disable=SC1090
  source <(sed '$d' "$SCRIPT")

  id() { echo 501; }
  root_owned_suite_binary() {
    [ "$1" = "/usr/local/bin" ] && { printf 'orchard-daemon'; return 0; }
    return 1
  }

  PREFIX=""
  SYSTEM=0

  run check_root_owned_conflict "$FAKE_HOME/.local/bin"
  [ "$status" -eq 1 ]
  grep -qF "/usr/local/bin/orchard-daemon is root-owned" <<<"$output"
}

@test "check_root_owned_conflict: explicit --prefix does not check /usr/local/bin" {
  # shellcheck disable=SC1090
  source <(sed '$d' "$SCRIPT")

  id() { echo 501; }
  root_owned_suite_binary() {
    [ "$1" = "/usr/local/bin" ] && { printf 'orchard-daemon'; return 0; }
    return 1
  }

  PREFIX="$PREFIX_DIR"
  SYSTEM=0

  run check_root_owned_conflict "$PREFIX_DIR"
  [ "$status" -eq 0 ]
}
