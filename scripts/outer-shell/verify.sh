#!/usr/bin/env bash
# verify.sh — Automated proof for the outer-tmux-wrapper spike (issue #747).
#
# Usage: verify.sh
#
# Drives the REAL launch.sh (not a reimplementation) against throwaway
# tmux sockets, using only tmux send-keys / capture-pane / display -p as
# the probe surface, plus a real attached pty client for the popup check
# (popups render only to a client's composited stream — capture-pane
# cannot see them). Prints PASS/FAIL per check; exits nonzero on any FAIL.
#
# Sockets used (never the user's real servers):
#   -L orchard-shell-test  (outer)
#   -L orchard-inner-test  (inner)
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
LAUNCH="$SCRIPT_DIR/launch.sh"

OUTER="orchard-shell-test"
INNER="orchard-inner-test"
OUTER_SESSION="shell"
INNER_SESSION="work"

SCRATCH="$(mktemp -d)"
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# The outer session's default-path (and so its sidebar pane's shell cwd)
# is inherited from wherever this script is invoked from. The placeholder
# sidebar command (`watch -n1 ...`, homebrew/procps watch — not BSD watch)
# writes timestamped snapshot files to its cwd when it can't fully control
# a real terminal, which a detached tmux pane never has. Pin cwd to the
# scratch dir before booting anything so any such side effect is cleaned
# up by the trap instead of leaking into the caller's working directory.
cd "$SCRATCH"

cleanup() {
  [[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
  [[ -n "$CHILD_ATTACH_PID" ]] && kill "$CHILD_ATTACH_PID" 2>/dev/null
  tmux -L "$OUTER" kill-server 2>/dev/null
  tmux -L "$INNER" kill-server 2>/dev/null
  rm -rf "$SCRATCH"
  return 0
}
trap cleanup EXIT

PASS=0
FAIL=0
RESULTS=()

record() {
  local status="$1" name="$2" detail="$3"
  RESULTS+=("$status|$name|$detail")
  if [[ "$status" == "PASS" ]]; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
  fi
  printf '[%s] %s (%s)\n' "$status" "$name" "$detail"
}

check_width40() {
  local name="$1"
  local actual
  actual="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_width}' 2>/dev/null)"
  if [[ "$actual" == "40" ]]; then
    record PASS "$name" "expected=40 actual=$actual"
  else
    record FAIL "$name" "expected=40 actual=${actual:-<no pane>}"
  fi
}

# --- clean slate on the -test sockets only ---------------------------------
tmux -L "$OUTER" kill-server 2>/dev/null
tmux -L "$INNER" kill-server 2>/dev/null

# --- boot inner session with a 2-pane starting window -----------------------
tmux -L "$INNER" new-session -d -s "$INNER_SESSION" -x 200 -y 50
tmux -L "$INNER" split-window -h -t "$INNER_SESSION"

# --- boot outer via the REAL launch.sh --------------------------------------
# stdin/stdout redirected away from a tty: launch.sh's final `exec ... attach`
# fails fast ("not a terminal") without harming the session it just built —
# this lets verify.sh drive the actual production script non-interactively.
BOOT_LOG="$SCRATCH/boot.log"
OUTER_SOCKET="$OUTER" "$LAUNCH" "$INNER" "$INNER_SESSION" </dev/null >"$BOOT_LOG" 2>&1

if ! tmux -L "$OUTER" has-session -t "$OUTER_SESSION" 2>/dev/null; then
  record FAIL "outer session boots" "session '$OUTER_SESSION' not found after launch.sh; see $BOOT_LOG"
  cat "$BOOT_LOG" >&2
  echo "cannot continue without a booted outer session" >&2
  exit 1
fi
record PASS "outer session boots" "launch.sh created session '$OUTER_SESSION' on socket '$OUTER'"

# Pin to an exact, deterministic baseline size regardless of the calling
# environment's own tty (verify.sh has none, so launch.sh's tput fallback
# already targets 160x45, but resize-window here makes the baseline explicit
# and exercises the window-resized hook at the same time).
tmux -L "$OUTER" resize-window -t "$OUTER_SESSION" -x 160 -y 45
check_width40 "initial layout (160x45)"

# --- idempotent-boot check: re-running launch.sh must not error or duplicate
OUTER_SOCKET="$OUTER" "$LAUNCH" "$INNER" "$INNER_SESSION" </dev/null >"$SCRATCH/boot2.log" 2>&1
WINCOUNT="$(tmux -L "$OUTER" list-windows -t "$OUTER_SESSION" 2>/dev/null | wc -l | tr -d ' ')"
if [[ "$WINCOUNT" == "1" ]]; then
  record PASS "idempotent re-run" "still exactly 1 outer window after second launch.sh invocation"
else
  record FAIL "idempotent re-run" "expected 1 outer window, found $WINCOUNT"
fi
tmux -L "$OUTER" resize-window -t "$OUTER_SESSION" -x 160 -y 45 >/dev/null 2>&1

# --- inner churn: none of this should ever move the outer sidebar ----------
tmux -L "$INNER" new-window -t "$INNER_SESSION"
check_width40 "after inner new-window"

tmux -L "$INNER" kill-pane -t "$INNER_SESSION"
check_width40 "after inner kill-pane"

tmux -L "$INNER" select-layout -t "$INNER_SESSION" even-horizontal
check_width40 "after inner select-layout even-horizontal"

tmux -L "$INNER" resize-pane -t "$INNER_SESSION" -L 20
check_width40 "after inner resize-pane -L 20"

tmux -L "$INNER" resize-pane -t "$INNER_SESSION" -Z
check_width40 "after inner resize-pane -Z (zoom)"

tmux -L "$INNER" split-window -t "$INNER_SESSION"
check_width40 "after inner split-window"

# --- outer resize-window: scripted resize on a (still detached) session ----
tmux -L "$OUTER" resize-window -t "$OUTER_SESSION" -x 200 -y 50
check_width40 "after outer resize-window -x 200 -y 50"

tmux -L "$OUTER" resize-window -t "$OUTER_SESSION" -x 120 -y 40
check_width40 "after outer resize-window -x 120 -y 40"

# --- no leaked "nested with care" refusal text in either outer pane --------
CAP0="$(tmux -L "$OUTER" capture-pane -p -t "$OUTER_SESSION:0.0" -S -100 2>/dev/null)"
CAP1="$(tmux -L "$OUTER" capture-pane -p -t "$OUTER_SESSION:0.1" -S -100 2>/dev/null)"
if printf '%s\n%s\n' "$CAP0" "$CAP1" | grep -qi "nested with care"; then
  record FAIL "no 'nested with care' warning" "found in outer pane capture (TMUX= clearing failed)"
else
  record PASS "no 'nested with care' warning" "absent from both outer panes"
fi

# --- popup renders over the layout ------------------------------------------
# capture-pane cannot see popup content: display-popup composites only into
# an attached CLIENT's rendered stream, never into any pane's grid buffer.
# Proof requires a real attached client whose raw pty bytes we can inspect.
PYHELPER="$SCRATCH/attach_client.py"
cat > "$PYHELPER" <<'PY_EOF'
#!/usr/bin/env python3
import fcntl, os, pty, struct, sys, termios
socket, session, cols, rows, logpath = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
master_fd, slave_fd = pty.openpty()
fcntl.ioctl(slave_fd, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols, 0, 0))
pid = os.fork()
if pid == 0:
    os.close(master_fd)
    os.setsid()
    fcntl.ioctl(slave_fd, termios.TIOCSCTTY, 0)
    os.dup2(slave_fd, 0); os.dup2(slave_fd, 1); os.dup2(slave_fd, 2)
    if slave_fd > 2:
        os.close(slave_fd)
    os.execvp('tmux', ['tmux', '-L', socket, 'attach', '-t', session])
    os._exit(1)
else:
    os.close(slave_fd)
    print(f'PTY_PID={pid}')
    sys.stdout.flush()
    with open(logpath, 'wb') as log:
        while True:
            try:
                data = os.read(master_fd, 4096)
            except OSError:
                break
            if not data:
                break
            log.write(data)
            log.flush()
PY_EOF

CUR_SIZE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
CUR_COLS="${CUR_SIZE%x*}"
CUR_ROWS="${CUR_SIZE#*x}"
PTYLOG="$SCRATCH/ptystream.log"
PTYOUT="$SCRATCH/ptyattach.out"
: >"$PTYOUT"

python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$CUR_COLS" "$CUR_ROWS" "$PTYLOG" >"$PTYOUT" 2>&1 &
PYWRAP_PID=$!

ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$PTYOUT" 2>/dev/null; then
    ATTACHED=1
    break
  fi
  sleep 0.1
done

if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$PTYOUT" | head -1 | cut -d= -f2)"
  sleep 0.3
  tmux -L "$OUTER" display-popup -t "$OUTER_SESSION" -E 'echo POPUPMARKER-HI; sleep 1' >/dev/null 2>&1 &
  POPUP_PID=$!
  sleep 1.3
  if grep -qa "POPUPMARKER-HI" "$PTYLOG" 2>/dev/null; then
    record PASS "popup renders over layout" "found POPUPMARKER-HI in attached client's raw stream"
  else
    record FAIL "popup renders over layout" "marker not found in raw client stream ($PTYLOG)"
  fi
  wait "$POPUP_PID" 2>/dev/null
else
  record FAIL "popup renders over layout" "real pty client never attached (see $PTYOUT)"
fi

[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
[[ -n "$CHILD_ATTACH_PID" ]] && kill "$CHILD_ATTACH_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# --- summary -----------------------------------------------------------------
echo
echo "=== verify.sh summary ==="
printf '%-6s %-40s %s\n' "STATUS" "CHECK" "DETAIL"
for r in "${RESULTS[@]}"; do
  IFS='|' read -r status name detail <<<"$r"
  printf '%-6s %-40s %s\n' "$status" "$name" "$detail"
done
echo
echo "PASS=$PASS FAIL=$FAIL"

if [[ "$FAIL" -gt 0 ]]; then
  exit 1
fi
exit 0
