#!/usr/bin/env bash
# verify_shell.sh — run scripts/outer-shell/verify.sh's whole battery against
# cmd/orchard-shell instead of scripts/outer-shell/launch.sh.
#
# Usage: cmd/orchard-shell/verify_shell.sh
#
# Why a staging copy rather than an env var: verify.sh sets
# `LAUNCH="$SCRIPT_DIR/launch.sh"` as an unconditional assignment, so no
# environment variable can redirect it without editing that file — and it is
# owned by another change in flight. This script instead copies verify.sh
# outer.conf and a launch.sh SHIM into a staging directory, and runs the copy.
# Every check verify.sh performs runs verbatim; the staged copy differs from
# the original in exactly two mechanical ways -- what `$LAUNCH` resolves to,
# and the throwaway socket names (renamed so a concurrent run of the original
# cannot fight this one for the same tmux servers).
#
# The staging directory must sit exactly two levels below the repo root:
# verify.sh derives the repo root as "$SCRIPT_DIR/../.." for the two checks
# that `go build` a test sidebar. bin/ is that, and it is already gitignored.
#
# Env:
#   ORCHARD_SHELL_VERIFY_CONF=repo   pass --conf <staged outer.conf> instead of
#                                    letting the binary use its embedded copy
#   ORCHARD_SHELL_VERIFY_KEEP=1      leave the staging directory in place
#   ORCHARD_SHELL_VERIFY_TAG=<s>     socket-name suffix (default: vs). The
#                                    staged copy's throwaway socket names are
#                                    renamed with it so this run can never
#                                    collide with a concurrent verify.sh.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." >/dev/null 2>&1 && cd .. && pwd)"
STAGE="$REPO_ROOT/bin/outer-shell-verify"
SRC_DIR="$REPO_ROOT/scripts/outer-shell"

cleanup() {
  if [[ "${ORCHARD_SHELL_VERIFY_KEEP:-}" == "1" ]]; then
    return 0
  fi
  rm -rf "$STAGE"
  return 0
}
trap cleanup EXIT

# The embedded conf is a copy of the canonical one. A drift means the binary
# boots a config nobody is reading -- fail here rather than run a battery
# against the wrong file. (conf_test.go asserts the same thing in Go.)
if ! cmp -s "$SRC_DIR/outer.conf" "$SCRIPT_DIR/outer.conf"; then
  echo "error: cmd/orchard-shell/outer.conf has drifted from scripts/outer-shell/outer.conf" >&2
  echo "  re-copy it:  cp $SRC_DIR/outer.conf $SCRIPT_DIR/outer.conf" >&2
  exit 1
fi

# Build to a temp path and `mv` into bin/, never `go build -o` straight onto
# the live path: go build truncates the existing inode, and overwriting a
# Mach-O a process is still executing from gets that process SIGKILLed by the
# kernel (macOS). The user's own `orchard shell` may well be running
# bin/orchard-sidebar right now. `mv` only repoints the directory entry.
echo "==> building bin/orchard-shell and bin/orchard-sidebar"
mkdir -p "$REPO_ROOT/bin"
BUILD_TMP="$(mktemp -d)"
( cd "$REPO_ROOT" && go build -o "$BUILD_TMP/orchard-shell" ./cmd/orchard-shell )
mv -f "$BUILD_TMP/orchard-shell" "$REPO_ROOT/bin/orchard-shell"

# The sidebar is a different change's subject and may not compile right now.
# Warn and keep whatever bin/ already holds rather than aborting the whole
# battery -- but say so, because verify.sh builds its own sidebar for the
# hand-back and scroll checks and those WILL fail while it is broken.
if ( cd "$REPO_ROOT" && go build -o "$BUILD_TMP/orchard-sidebar" ./cmd/orchard-sidebar ); then
  mv -f "$BUILD_TMP/orchard-sidebar" "$REPO_ROOT/bin/orchard-sidebar"
else
  echo "warning: cmd/orchard-sidebar does not compile; reusing the existing bin/orchard-sidebar." >&2
  echo "warning: verify.sh's sidebar-dependent checks will fail until it does." >&2
fi
rm -rf "$BUILD_TMP"

rm -rf "$STAGE"
mkdir -p "$STAGE"
# Rename the throwaway sockets in the STAGED copy only. verify.sh hardcodes
# orchard-{shell,inner}-test[2-4]; a concurrent run of the original would
# fight this one for the same servers.
TAG="${ORCHARD_SHELL_VERIFY_TAG:-vs}"
sed -e "s/orchard-shell-test/orchard-shell-${TAG}test/g" \
    -e "s/orchard-inner-test/orchard-inner-${TAG}test/g" \
    "$SRC_DIR/verify.sh" >"$STAGE/verify.sh"
cp "$SRC_DIR/outer.conf" "$STAGE/outer.conf"
chmod +x "$STAGE/verify.sh"

CONF_ARGS=()
if [[ "${ORCHARD_SHELL_VERIFY_CONF:-}" == "repo" ]]; then
  CONF_ARGS=(--conf "$STAGE/outer.conf")
fi

# The shim speaks launch.sh's interface (INNER_SOCKET SESSION positionally,
# OUTER_SOCKET from the environment) and calls orchard-shell's.
cat >"$STAGE/launch.sh" <<SHIM
#!/usr/bin/env bash
set -euo pipefail
exec "$REPO_ROOT/bin/orchard-shell" \\
  --inner-socket "\$1" \\
  --session "\$2" \\
  --outer-socket "\${OUTER_SOCKET:-orchard-shell}" ${CONF_ARGS[*]:-}
SHIM
chmod +x "$STAGE/launch.sh"

echo "==> running verify.sh against bin/orchard-shell (staged at $STAGE)"
bash "$STAGE/verify.sh"
