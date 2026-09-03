#!/usr/bin/env bats
# install.sh behavior, hermetic: ORCHARD_RELEASE_BASE_URL points at a
# file://-served fixture suite tarball (curl supports file:// URLs, so no
# HTTP server or GitHub access is needed) and a stub systemctl on PATH
# records every call and plays back a scripted active unit -- so the
# service detect/restart/install-fallback, PATH warning, and backup
# rotation behaviors are pinned without a live systemd or real releases.
#
# Covers: systemd unit detection + priority + --no-service, --json
# path_warning, and atomic_install's backup-before-replace + rotation.
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
