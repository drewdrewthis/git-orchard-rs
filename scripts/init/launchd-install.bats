#!/usr/bin/env bats
# T2: L2 envelope assertions for init/launchd-install.sh, plus the #749
# contract — the rendered plist must carry absolute log paths and the state
# directory must exist before launchd tries to open the redirect targets.

setup() {
  SCRIPT="$BATS_TEST_DIRNAME/launchd-install.sh"
  FAKE_HOME="$(mktemp -d)"
  DEST_DIR="$(mktemp -d)"
  XDG_DIR="$(mktemp -d)"
}

teardown() {
  rm -rf "$FAKE_HOME" "$DEST_DIR" "$XDG_DIR"
}

_json_field() {
  python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('$1','MISSING'))" 2>/dev/null || echo "PARSE_ERROR"
}

_json_data_field() {
  python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('data',{}).get('$1','MISSING'))" 2>/dev/null || echo "PARSE_ERROR"
}

_json_err_code() {
  python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('error',{}).get('code','MISSING'))" 2>/dev/null || echo "PARSE_ERROR"
}

# Extract the <string> value following a given <key> from a plist on stdin.
_plist_value() {
  python3 -c "
import re,sys
content = sys.stdin.read()
m = re.search(r'<key>\s*$1\s*</key>\s*<string>\s*(.*?)\s*</string>', content, re.S)
print(m.group(1) if m else 'MISSING')
"
}

@test "--print renders without the placeholder" {
  output="$(HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --print)"
  [[ "$output" != *"__ORCHARD_STATE_DIR__"* ]]
}

@test "--print yields absolute StandardOutPath under the state dir" {
  output="$(HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --print)"
  out_path="$(echo "$output" | _plist_value StandardOutPath)"
  [ "$out_path" = "$FAKE_HOME/.local/state/orchard/orchard.out.log" ]
}

@test "--print yields absolute StandardErrorPath under the state dir" {
  output="$(HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --print)"
  err_path="$(echo "$output" | _plist_value StandardErrorPath)"
  [ "$err_path" = "$FAKE_HOME/.local/state/orchard/orchard.err.log" ]
}

@test "--print leaves no tilde in the log paths" {
  output="$(HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --print)"
  out_path="$(echo "$output" | _plist_value StandardOutPath)"
  [[ "$out_path" != *"~"* ]]
}

@test "--print installs nothing" {
  HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --print >/dev/null
  [ ! -e "$FAKE_HOME/Library/LaunchAgents/com.gitorchard.orchard.plist" ]
}

@test "XDG_STATE_HOME wins over HOME, matching orchpaths.StateDir" {
  output="$(HOME="$FAKE_HOME" XDG_STATE_HOME="$XDG_DIR" "$SCRIPT" --print)"
  out_path="$(echo "$output" | _plist_value StandardOutPath)"
  [ "$out_path" = "$XDG_DIR/orchard/orchard.out.log" ]
}

@test "install creates the state dir so launchd can open the redirects" {
  HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --dest "$DEST_DIR" >/dev/null
  [ -d "$FAKE_HOME/.local/state/orchard" ]
}

@test "install writes the plist into --dest" {
  HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --dest "$DEST_DIR" >/dev/null
  [ -f "$DEST_DIR/com.gitorchard.orchard.plist" ]
}

@test "install --json: ok=true with the resolved paths" {
  output="$(HOME="$FAKE_HOME" XDG_STATE_HOME= "$SCRIPT" --json --dest "$DEST_DIR")"
  [ "$(echo "$output" | _json_field ok)" = "True" ]
  [ "$(echo "$output" | _json_data_field path)" = "$DEST_DIR/com.gitorchard.orchard.plist" ]
  [ "$(echo "$output" | _json_data_field outLog)" = "$FAKE_HOME/.local/state/orchard/orchard.out.log" ]
  [ "$(echo "$output" | _json_data_field errLog)" = "$FAKE_HOME/.local/state/orchard/orchard.err.log" ]
}

@test "no HOME and no XDG_STATE_HOME: ok=false, code=no_home" {
  output="$(env -u HOME -u XDG_STATE_HOME "$SCRIPT" --json --print 2>/dev/null || true)"
  [ "$(echo "$output" | _json_field ok)" = "False" ]
  [ "$(echo "$output" | _json_err_code)" = "no_home" ]
}

@test "unknown arg exits 2" {
  run env HOME="$FAKE_HOME" "$SCRIPT" --nope
  [ "$status" -eq 2 ]
}
