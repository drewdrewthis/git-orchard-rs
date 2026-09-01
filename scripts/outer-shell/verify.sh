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
OUTER2="orchard-shell-test2"  # second, independent outer wrapper -- used only
                              # by the "switch-client only moves the
                              # wrapper's own client" check below, so it
                              # never collides with the primary $OUTER
                              # instance the checks above are still using.

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
  tmux -L "$OUTER2" kill-server 2>/dev/null
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
tmux -L "$OUTER2" kill-server 2>/dev/null
tmux -L "$INNER" kill-server 2>/dev/null

# --- boot inner session with a 2-pane starting window -----------------------
# -f /dev/null: this throwaway inner session is test scaffolding, not the
# thing under test — it must not inherit the invoking user's real
# ~/.tmux.conf (e.g. a coincidental C-a prefix rebind there would confound
# the swallow-check below).
tmux -L "$INNER" -f /dev/null new-session -d -s "$INNER_SESSION" -x 200 -y 50
tmux -L "$INNER" split-window -h -t "$INNER_SESSION"

# Marker visible in the inner session's active pane BEFORE outer ever
# attaches to it. This is what lets the boot-correctness checks below
# prove the inner attach actually landed in outer's 0.1 pane, rather than
# just trusting that a pane exists there.
tmux -L "$INNER" send-keys -t "$INNER_SESSION" 'echo INNER_MARKER_747' Enter
sleep 0.2

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

# --- boot correctness: the right command must be in the right pane ---------
# Regression guard for the send-keys-before-split bug: sending the sidebar
# command to "0.0" before split-window runs lands it in the pre-split sole
# pane, which the split then renumbers to 0.1 — so the sidebar command and
# the inner attach both end up typed into the SAME physical pane (the
# attach keystrokes get swallowed as input to the already-running watch
# TUI and never reach a shell prompt), while the other pane stays a bare
# shell. Neither check above (pane width) nor the "nested with care" check
# further down would catch that regression: width is a window-geometry
# property independent of what's running in the pane, and the warning
# text never appears if the attach command was never actually executed.
# Poll briefly: unlike pane_width (an instant window-flag read), this
# waits on a real subprocess (nested tmux client) spawning and completing
# its handshake with the inner server.
CMD01=""
for _ in $(seq 1 20); do
  CMD01="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_current_command}' 2>/dev/null)"
  [[ "$CMD01" == "tmux" ]] && break
  sleep 0.1
done
CMD00="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_current_command}' 2>/dev/null)"
CAP01_BOOT="$(tmux -L "$OUTER" capture-pane -p -t "$OUTER_SESSION:0.1" -S -100 2>/dev/null)"

if printf '%s' "$CAP01_BOOT" | grep -q "INNER_MARKER_747"; then
  record PASS "inner attach lands in right pane (0.1)" "INNER_MARKER_747 visible in outer 0.1 capture"
else
  record FAIL "inner attach lands in right pane (0.1)" "marker not found in outer 0.1 capture"
fi

if [[ ( "$CMD00" == "watch" || "$CMD00" == "orchard-sidebar" ) && "$CMD01" == "tmux" ]]; then
  record PASS "pane commands: 0.0=watch|orchard-sidebar, 0.1=tmux" "0.0=$CMD00 0.1=$CMD01"
else
  record FAIL "pane commands: 0.0=watch|orchard-sidebar, 0.1=tmux" "expected 0.0 in {watch,orchard-sidebar} 0.1=tmux; actual 0.0=${CMD00:-<none>} 0.1=${CMD01:-<none>}"
fi

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

# --- shared real-attached-client helper --------------------------------------
# Both the popup check and the prefix-swallow check below need a REAL
# attached tmux client (not send-keys, not capture-pane): popups composite
# only into an attached client's rendered stream, never into any pane's
# grid buffer, and the per-client prefix/key-table dispatch this wrapper
# depends on only runs for a real client's input, never for send-keys
# (which injects straight into a pane, bypassing client-level dispatch
# entirely). Optional argv[6] is a base64-encoded byte string written to
# the client's stdin ~0.5s after attach, for checks that need to inject
# real keystrokes rather than just observe output; omitted, behavior is
# identical to the original 5-arg popup-only helper.
PYHELPER="$SCRATCH/attach_client.py"
cat > "$PYHELPER" <<'PY_EOF'
#!/usr/bin/env python3
import base64, fcntl, os, pty, struct, sys, termios, time
socket, session, cols, rows, logpath = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
send_keys = base64.b64decode(sys.argv[6]) if len(sys.argv) > 6 else None
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
    if send_keys is not None:
        time.sleep(0.5)  # let the client finish attaching + set raw mode
        os.write(master_fd, send_keys)
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

# --- prefix-less outer: nothing typed at a real client gets eaten -----------
# Regression guard for the double-swallow bug: with `C-a` as outer's prefix,
# a real Ctrl-A consumed itself to enter prefix-table dispatch, then
# consumed the NEXT keystroke too (unbound in that table, silently
# dropped) before falling through — so "echo swallow747" arrived at the
# inner shell as "cho swallow747", missing its leading char. send-keys
# cannot reproduce this (it bypasses client-level key-table dispatch
# entirely), so this drives a real attached pty client, the same way the
# popup check below does.
#
# Force pane 0.1 active first: a brand-new client displays whatever pane is
# already active on the window, and earlier checks in this script leave
# 0.0 active (split-window's newly-created pane, which becomes 0.0, takes
# focus at boot; nothing since has changed it). Key-table dispatch happens
# client-side before a byte is routed to any pane, so forcing focus here
# only sets up the precondition — it doesn't touch the mechanism under test.
tmux -L "$OUTER" select-pane -t "$OUTER_SESSION:0.1" 2>/dev/null

SWALLOW_SIZE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
SWALLOW_COLS="${SWALLOW_SIZE%x*}"
SWALLOW_ROWS="${SWALLOW_SIZE#*x}"
SWALLOW_KEYS_B64="$(printf '\x01echo swallow747\r' | base64 | tr -d '\n')"
SWALLOWLOG="$SCRATCH/swallow.log"
SWALLOWOUT="$SCRATCH/swallowattach.out"
: >"$SWALLOWOUT"

python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$SWALLOW_COLS" "$SWALLOW_ROWS" "$SWALLOWLOG" "$SWALLOW_KEYS_B64" >"$SWALLOWOUT" 2>&1 &
PYWRAP_PID=$!

ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$SWALLOWOUT" 2>/dev/null; then
    ATTACHED=1
    break
  fi
  sleep 0.1
done

if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$SWALLOWOUT" | head -1 | cut -d= -f2)"
  sleep 1.2  # settle: attach handshake + injected keys (written ~0.5s in) + shell round-trip
  INNER_CAP="$(tmux -L "$INNER" capture-pane -p -t "$INNER_SESSION" -S -200 2>/dev/null)"
  if printf '%s' "$INNER_CAP" | grep -qF "echo swallow747"; then
    record PASS "outer prefix does not swallow keystrokes" "'echo swallow747' landed intact in inner pane"
  else
    record FAIL "outer prefix does not swallow keystrokes" "'echo swallow747' missing/mangled in inner capture (a leading char was eaten)"
  fi
  kill "$CHILD_ATTACH_PID" 2>/dev/null
else
  record FAIL "outer prefix does not swallow keystrokes" "real pty client never attached (see $SWALLOWOUT)"
fi
[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# --- popup renders over the layout ------------------------------------------
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

# --- switch-client only moves the wrapper's own client ----------------------
# Regression guard for the live-pass defect (#747 defect 2): on a SHARED
# inner server, a plain `switch-client -t <session>` (no -c) lets tmux pick
# an ARBITRARY attached client to move -- in the wild this hijacked the
# user's unrelated terminal instead of the wrapper's own pane. The fix
# scopes every switch to -c $ORCHARD_TMUX_CLIENT, which launch.sh resolves
# from outer pane 0.1's #{pane_tty} (see launch.sh, INNER_TTY). This proves
# BOTH halves: the scoped command moves only the wrapper's own client, and
# -- informational only, never a gate -- that the OLD unscoped form really
# is capable of moving a bystander, evidence the -c fix is load-bearing.
tmux -L "$INNER" new-session -d -s A
tmux -L "$INNER" new-session -d -s B

# A second, independent outer wrapper attached to inner session A via the
# REAL launch.sh, so the wrapper's own inner client below is produced
# exactly the way production does it -- not synthesized.
OUTER_SOCKET="$OUTER2" "$LAUNCH" "$INNER" A </dev/null >"$SCRATCH/boot_switchcheck.log" 2>&1

SWITCH_INNER_TTY=""
for _ in $(seq 1 20); do
  if [[ "$(tmux -L "$INNER" list-clients -t A 2>/dev/null | wc -l | tr -d ' ')" -ge 1 ]]; then
    SWITCH_INNER_TTY="$(tmux -L "$INNER" list-clients -t A -F '#{client_tty}' 2>/dev/null | head -1)"
    break
  fi
  sleep 0.1
done

if [[ -z "$SWITCH_INNER_TTY" ]]; then
  record FAIL "switch-client only moves the wrapper's own client" "wrapper's own inner client never attached to A; see $SCRATCH/boot_switchcheck.log"
else
  # The bystander: a second, independent client attached to A, reusing the
  # same real-pty-client helper the swallow/popup checks above use.
  BYSTANDER_LOG="$SCRATCH/bystander.log"
  BYSTANDER_OUT="$SCRATCH/bystanderattach.out"
  : >"$BYSTANDER_OUT"

  python3 "$PYHELPER" "$INNER" A 80 24 "$BYSTANDER_LOG" >"$BYSTANDER_OUT" 2>&1 &
  PYWRAP_PID=$!

  BYSTANDER_ATTACHED=0
  for _ in $(seq 1 50); do
    if grep -q '^PTY_PID=' "$BYSTANDER_OUT" 2>/dev/null; then
      BYSTANDER_ATTACHED=1
      break
    fi
    sleep 0.1
  done

  if [[ "$BYSTANDER_ATTACHED" -ne 1 ]]; then
    record FAIL "switch-client only moves the wrapper's own client" "bystander pty client never attached to A; see $BYSTANDER_OUT"
  else
    CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$BYSTANDER_OUT" | head -1 | cut -d= -f2)"
    sleep 0.3
    BYSTANDER_TTY="$(tmux -L "$INNER" list-clients -t A -F '#{client_tty}' 2>/dev/null | grep -vF "$SWITCH_INNER_TTY" | head -1)"

    if [[ -z "$BYSTANDER_TTY" ]]; then
      record FAIL "switch-client only moves the wrapper's own client" "could not identify a distinct bystander client tty on A"
    else
      # The exact command the sidebar builds when ORCHARD_TMUX_CLIENT is
      # set -- see switchClientArgs in cmd/orchard-sidebar/main.go.
      tmux -L "$INNER" switch-client -c "$SWITCH_INNER_TTY" -t B

      WRAPPER_SESS="$(tmux -L "$INNER" list-clients -F '#{client_tty} #{client_session}' 2>/dev/null | awk -v t="$SWITCH_INNER_TTY" '$1==t{print $2}')"
      BYSTANDER_SESS="$(tmux -L "$INNER" list-clients -F '#{client_tty} #{client_session}' 2>/dev/null | awk -v t="$BYSTANDER_TTY" '$1==t{print $2}')"

      if [[ "$WRAPPER_SESS" == "B" && "$BYSTANDER_SESS" == "A" ]]; then
        record PASS "switch-client only moves the wrapper's own client" "scoped -c moved the wrapper to B; bystander stayed on A"
      else
        record FAIL "switch-client only moves the wrapper's own client" "wrapper_session=${WRAPPER_SESS:-<none>} (want B) bystander_session=${BYSTANDER_SESS:-<none>} (want A)"
      fi

      # Negative control (informational only -- never gates the run): put
      # both clients back on a shared baseline, then run the OLD unscoped
      # form and see which client tmux actually chose to move.
      tmux -L "$INNER" switch-client -c "$SWITCH_INNER_TTY" -t A 2>/dev/null
      tmux -L "$INNER" switch-client -c "$BYSTANDER_TTY" -t A 2>/dev/null
      tmux -L "$INNER" switch-client -t B 2>/dev/null
      MOVED_TO_B="$(tmux -L "$INNER" list-clients -F '#{client_tty} #{client_session}' 2>/dev/null | awk '$2=="B"{print $1}')"
      if printf '%s\n' "$MOVED_TO_B" | grep -qF "$BYSTANDER_TTY"; then
        echo "[INFO] evidence: unscoped 'switch-client -t B' (no -c) moved the BYSTANDER client -- proves the -c scoping fix is load-bearing, not defensive"
      elif [[ -n "$MOVED_TO_B" ]]; then
        echo "[INFO] unscoped 'switch-client -t B' moved a different client ($MOVED_TO_B) this run, not the bystander -- tmux's unscoped client choice isn't guaranteed reproducible; the -c fix removes the ambiguity regardless"
      else
        echo "[INFO] unscoped 'switch-client -t B' moved no client this run"
      fi
    fi

    kill "$CHILD_ATTACH_PID" 2>/dev/null
  fi

  [[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
  wait "$PYWRAP_PID" 2>/dev/null
  PYWRAP_PID=""
  CHILD_ATTACH_PID=""
fi

tmux -L "$OUTER2" kill-server 2>/dev/null

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
