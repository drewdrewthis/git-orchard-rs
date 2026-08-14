#!/usr/bin/env bats
# sidebar-open.sh behavior, hermetic: a stub tmux on PATH records every call
# and plays back scripted answers, so the idempotence and heal-resize branches
# are pinned without a live tmux server (CI has none).

setup() {
  SCRIPT="$BATS_TEST_DIRNAME/sidebar-open.sh"
  STUB_DIR="$(mktemp -d)"
  export TMUX_STUB_LOG="$STUB_DIR/log"
  : > "$TMUX_STUB_LOG"

  # scripted answers, overridable per test via env
  export STUB_WIDTH_OPTION="${STUB_WIDTH_OPTION:-38}"
  export STUB_PANES=""       # list-panes output; empty = no sidebar open
  export STUB_PANE_WIDTH=""  # display-message output for the existing pane

  cat > "$STUB_DIR/tmux" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$TMUX_STUB_LOG"
case "$1" in
  show-option)      echo "$STUB_WIDTH_OPTION" ;;
  list-panes)       printf '%s\n' "$STUB_PANES" ;;
  display-message)  echo "$STUB_PANE_WIDTH" ;;
esac
exit 0
EOF
  chmod +x "$STUB_DIR/tmux"
  # the script needs an orchard-sidebar on PATH to get past its bin check
  printf '#!/usr/bin/env bash\nexit 0\n' > "$STUB_DIR/orchard-sidebar"
  chmod +x "$STUB_DIR/orchard-sidebar"
  export PATH="$STUB_DIR:$PATH"
}

teardown() {
  rm -rf "$STUB_DIR"
}

@test "no sidebar yet: opens one at the option width" {
  run bash "$SCRIPT" s1
  [ "$status" -eq 0 ]
  grep -q "split-window -hb -d -l 38 -t s1:" "$TMUX_STUB_LOG"
}

@test "sidebar already open and wide enough: no-op (no split, no resize)" {
  export STUB_PANES="%7 /usr/local/bin/orchard-sidebar"
  export STUB_PANE_WIDTH="40"
  run bash "$SCRIPT" s1
  [ "$status" -eq 0 ]
  ! grep -q "split-window" "$TMUX_STUB_LOG"
  ! grep -q "resize-pane" "$TMUX_STUB_LOG"
}

@test "sidebar squeezed under the floor: healed back to the option width" {
  export STUB_PANES="%7 /usr/local/bin/orchard-sidebar"
  export STUB_PANE_WIDTH="20"
  run bash "$SCRIPT" s1
  [ "$status" -eq 0 ]
  grep -q "resize-pane -t %7 -x 38" "$TMUX_STUB_LOG"
  ! grep -q "split-window" "$TMUX_STUB_LOG"
}

@test "unreadable pane width: heal is skipped, not guessed" {
  export STUB_PANES="%7 /usr/local/bin/orchard-sidebar"
  export STUB_PANE_WIDTH="not-a-number"
  run bash "$SCRIPT" s1
  [ "$status" -eq 0 ]
  ! grep -q "resize-pane" "$TMUX_STUB_LOG"
}

@test "width option below the compact floor is clamped to 34" {
  export STUB_WIDTH_OPTION="10"
  run bash "$SCRIPT" s1
  [ "$status" -eq 0 ]
  grep -q "split-window -hb -d -l 34 -t s1:" "$TMUX_STUB_LOG"
}
