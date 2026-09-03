#!/usr/bin/env bash
# launchd-install.sh — render and install the orchard launchd unit (macOS).
#
# Usage:
#   launchd-install.sh [--json] [--print] [--dest <dir>]
#
#   --print       render the plist to stdout; install nothing
#   --dest <dir>  install into <dir> instead of ~/Library/LaunchAgents
#   --json        L2 machine output:
#                   {"ok": true, "data": {"path": "...", "stateDir": "...",
#                                         "outLog": "...", "errLog": "..."}}
#                   {"ok": false, "error": {"code": "...", "message": "..."}}
#
# launchd does NOT expand `~` or environment variables in StandardOutPath /
# StandardErrorPath — it open(2)s the string verbatim — so the shipped unit
# cannot name ~/.local/state/orchard directly. It ships as a template with an
# __ORCHARD_STATE_DIR__ placeholder that this script substitutes with the
# absolute path, resolved the same way internal/orchpaths.StateDir does
# (XDG_STATE_HOME wins, else $HOME/.local/state/orchard) so the daemon's logs
# and its pidfile never land in different directories.
#
# The state directory is created here because launchd opens the redirect
# targets BEFORE exec'ing the program: a missing parent directory silently
# yields no log at all (issue #749).
#
# Per L3 this is a bash script. Per L11 it does NOT call the daemon.
set -euo pipefail

TEMPLATE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="$TEMPLATE_DIR/com.gitorchard.orchard.plist.template"
LABEL="com.gitorchard.orchard"
PLACEHOLDER="__ORCHARD_STATE_DIR__"

JSON=false
PRINT_ONLY=false
DEST=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --json) JSON=true; shift ;;
    --print) PRINT_ONLY=true; shift ;;
    --dest) DEST="${2:-}"; shift 2 ;;
    -h|--help) sed -n '2,26p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

# json_escape backslash-escapes a string for embedding in a JSON string
# literal. `\` and `"` are the only bytes that break the envelope here —
# paths and error messages are the only values that flow through, and
# neither is expected to carry control characters.
json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '%s' "$s"
}

emit_err() {
  local code="$1" msg="$2"
  if $JSON; then
    printf '{"ok":false,"error":{"code":"%s","message":"%s"}}\n' "$code" "$(json_escape "$msg")"
  else
    echo "error: $msg" >&2
  fi
  exit 1
}

[[ -f "$TEMPLATE" ]] || emit_err "template_missing" "template not found at $TEMPLATE"

# Mirror internal/orchpaths.StateDir: XDG_STATE_HOME wins for state only.
if [[ -n "${XDG_STATE_HOME:-}" ]]; then
  STATE_DIR="$XDG_STATE_HOME/orchard"
else
  [[ -n "${HOME:-}" ]] || emit_err "no_home" "neither XDG_STATE_HOME nor HOME is set"
  STATE_DIR="$HOME/.local/state/orchard"
fi

# The substituted value must be absolute or launchd resolves it against its
# own working directory, which is not the user's.
[[ "$STATE_DIR" = /* ]] || emit_err "relative_state_dir" "resolved state dir $STATE_DIR is not absolute"

render() {
  # `${var//pat/rep}` avoids sed, so a state dir containing `/` (always) or
  # any sed metacharacter needs no escaping.
  local content
  content="$(cat "$TEMPLATE")"
  printf '%s\n' "${content//$PLACEHOLDER/$STATE_DIR}"
}

if $PRINT_ONLY; then
  render
  exit 0
fi

if [[ -z "$DEST" ]]; then
  [[ -n "${HOME:-}" ]] || emit_err "no_home" "HOME is not set; pass --dest explicitly"
  DEST="$HOME/Library/LaunchAgents"
fi

mkdir -p "$STATE_DIR" || emit_err "mkdir_failed" "cannot create state dir $STATE_DIR"
mkdir -p "$DEST" || emit_err "mkdir_failed" "cannot create destination $DEST"

PLIST_PATH="$DEST/$LABEL.plist"
render > "$PLIST_PATH" || emit_err "write_failed" "cannot write $PLIST_PATH"

if $JSON; then
  printf '{"ok":true,"data":{"path":"%s","stateDir":"%s","outLog":"%s","errLog":"%s"}}\n' \
    "$(json_escape "$PLIST_PATH")" "$(json_escape "$STATE_DIR")" \
    "$(json_escape "$STATE_DIR/orchard.out.log")" "$(json_escape "$STATE_DIR/orchard.err.log")"
else
  echo "installed $PLIST_PATH"
  echo "logs: $STATE_DIR/orchard.out.log, $STATE_DIR/orchard.err.log"
  echo "next: launchctl load -w $PLIST_PATH"
fi
