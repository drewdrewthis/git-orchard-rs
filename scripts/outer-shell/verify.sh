#!/usr/bin/env bash
# verify.sh — Automated proof for the outer tmux wrapper (issue #747).
#
# Usage: verify.sh
#
# Drives the REAL bin/orchard-shell (built fresh below from cmd/orchard-shell,
# not a reimplementation) against throwaway tmux sockets, using only tmux
# send-keys / capture-pane / display -p as the probe surface, plus a real
# attached pty client for the popup check (popups render only to a client's
# composited stream — capture-pane cannot see them). Prints PASS/FAIL per
# check; exits nonzero on any FAIL.
#
# Sockets used (never the user's real servers):
#   -L orchard-shell-test  (outer)
#   -L orchard-inner-test  (inner)
#   -L orchard-shell-test4 / orchard-inner-test4 (scroll + launch-modal check)
set -uo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." >/dev/null 2>&1 && pwd)"

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
HB_OUTER=""
HB_INNER=""
FK_OUTER=""
FK_INNER=""
FK_POPUP_PATTERN=""
ST_OUTER=""
ST_INNER=""
SH_OUTER=""
SH_INNER=""
HB_REPO_BIN=""
HB_REPO_BIN_BACKUP=""

# The outer session's default-path (and so its sidebar pane's shell cwd)
# is inherited from wherever this script is invoked from. The placeholder
# sidebar command (`watch -n1 ...`, homebrew/procps watch — not BSD watch)
# writes timestamped snapshot files to its cwd when it can't fully control
# a real terminal, which a detached tmux pane never has. Pin cwd to the
# scratch dir before booting anything so any such side effect is cleaned
# up by the trap instead of leaking into the caller's working directory.
cd "$SCRATCH" || exit 1

# Build bin/orchard-shell fresh so this always drives current source, not a
# stale binary. mv into place rather than `go build -o` straight onto a live
# path: go build truncates the existing inode, and overwriting a Mach-O a
# process is still executing from gets that process SIGKILLed by the kernel
# (macOS) -- irrelevant for THIS build (nothing is running bin/orchard-shell
# yet) but kept consistent with the same pattern used for orchard-sidebar
# further down.
echo "==> building bin/orchard-shell"
mkdir -p "$REPO_ROOT/bin"
SHELL_BUILD_TMP="$(mktemp -d)"
if ! ( cd "$REPO_ROOT" && go build -o "$SHELL_BUILD_TMP/orchard-shell" ./cmd/orchard-shell ) >"$SCRATCH/shell-build.log" 2>&1; then
  cat "$SCRATCH/shell-build.log" >&2
  echo "error: go build ./cmd/orchard-shell failed" >&2
  exit 1
fi
mv -f "$SHELL_BUILD_TMP/orchard-shell" "$REPO_ROOT/bin/orchard-shell"
rm -rf "$SHELL_BUILD_TMP"

# render_conf OUTER_SOCKET INNER_SOCKET: prints the path to outer.conf
# rendered via the real binary's `render-conf` subcommand -- the same
# substituteConf (cmd/orchard-shell/conf.go) the boot path itself uses.
# Every direct outer.conf load below goes through this instead of `-f`/
# `source-file`-ing scripts/outer-shell/outer.conf raw, which would leave
# its @ORCHARD_SHELL@/@INNER_SOCKET@/@OUTER_SOCKET@ tokens unsubstituted --
# a single source of truth for the rendered bytes rather than two paths
# that can drift.
render_conf() {
  local outer_socket="$1" inner_socket="$2"
  local out="$SCRATCH/outer-rendered-${outer_socket}.conf"
  local log="$SCRATCH/render-conf-${outer_socket}.log"
  if ! "$REPO_ROOT/bin/orchard-shell" render-conf \
      --self "$REPO_ROOT/bin/orchard-shell" \
      --inner-socket "$inner_socket" \
      --outer-socket "$outer_socket" >"$out" 2>"$log"; then
    cat "$log" >&2
    echo "error: orchard-shell render-conf failed for socket $outer_socket" >&2
    exit 1
  fi
  printf '%s' "$out"
}
RENDERED_CONF="$(render_conf "$OUTER" "$INNER")"

# LAUNCH speaks the same interface every check below already calls it with
# (INNER_SOCKET SESSION positionally, OUTER_SOCKET from the environment) --
# only what it execs underneath has changed, from the old shell prototype to
# the real binary that replaced it. --detach: boot and return 0 without an
# attach, since nothing invoking $LAUNCH below has a tty to attach to.
LAUNCH="$SCRATCH/launch-shim.sh"
cat >"$LAUNCH" <<SHIM
#!/usr/bin/env bash
set -euo pipefail
exec "$REPO_ROOT/bin/orchard-shell" \\
  --inner-socket "\$1" \\
  --session "\$2" \\
  --outer-socket "\${OUTER_SOCKET:-orchard-shell}" \\
  --detach
SHIM
chmod +x "$LAUNCH"

cleanup() {
  [[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
  [[ -n "$CHILD_ATTACH_PID" ]] && kill "$CHILD_ATTACH_PID" 2>/dev/null
  tmux -L "$OUTER" kill-server 2>/dev/null
  tmux -L "$OUTER2" kill-server 2>/dev/null
  tmux -L "$INNER" kill-server 2>/dev/null
  [[ -n "$HB_OUTER" ]] && tmux -L "$HB_OUTER" kill-server 2>/dev/null
  [[ -n "$HB_INNER" ]] && tmux -L "$HB_INNER" kill-server 2>/dev/null
  # A display-popup client blocks for as long as its popup is open; if the
  # scroll/popup check below died mid-popup, that client is still sitting
  # there holding a pane on a socket we are about to kill. The pattern is
  # scoped to this run's own inner socket name, so it can never match the
  # user's real sidebar (very likely running while this executes).
  [[ -n "$FK_POPUP_PATTERN" ]] && pkill -f "$FK_POPUP_PATTERN" 2>/dev/null
  [[ -n "$FK_OUTER" ]] && tmux -L "$FK_OUTER" kill-server 2>/dev/null
  [[ -n "$FK_INNER" ]] && tmux -L "$FK_INNER" kill-server 2>/dev/null
  [[ -n "$ST_OUTER" ]] && tmux -L "$ST_OUTER" kill-server 2>/dev/null
  [[ -n "$ST_INNER" ]] && tmux -L "$ST_INNER" kill-server 2>/dev/null
  [[ -n "$SH_OUTER" ]] && tmux -L "$SH_OUTER" kill-server 2>/dev/null
  [[ -n "$SH_INNER" ]] && tmux -L "$SH_INNER" kill-server 2>/dev/null
  # Guaranteed restore point for the hand-back check's binary swap (see
  # that block): overwriting bin/orchard-sidebar IN PLACE while the just
  # -launched sidebar process is still executing from it gets the process
  # SIGKILLed by the kernel (observed on macOS -- a running Mach-O's
  # backing file being truncated/rewritten invalidates its mapped pages).
  # Restoring only here, on any exit path, means the swapped-in test
  # binary stays live for the whole hand-back check and nothing can leave
  # the repo's real binary replaced.
  if [[ -n "$HB_REPO_BIN" ]]; then
    if [[ -n "$HB_REPO_BIN_BACKUP" ]]; then
      mv -f "$HB_REPO_BIN_BACKUP" "$HB_REPO_BIN" 2>/dev/null
    else
      rm -f "$HB_REPO_BIN" 2>/dev/null
    fi
  fi
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
# identical to the original 5-arg popup-only helper. Optional argv[7]
# replaces that 0.5s delay, for checks that must drive the pane first and
# then key into whatever that opened.
PYHELPER="$SCRATCH/attach_client.py"
cat > "$PYHELPER" <<'PY_EOF'
#!/usr/bin/env python3
import base64, fcntl, os, pty, struct, sys, termios, time
socket, session, cols, rows, logpath = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
send_keys = base64.b64decode(sys.argv[6]) if len(sys.argv) > 6 else None
send_delay = float(sys.argv[7]) if len(sys.argv) > 7 else 0.5
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
        # default: let the client finish attaching + set raw mode. A longer
        # delay lets a caller drive the pane first and key into what it opened.
        time.sleep(send_delay)
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

# --- boot outer via the REAL bin/orchard-shell (through $LAUNCH) -----------
# $LAUNCH passes --detach, so this returns 0 without needing a tty to attach
# to -- this lets verify.sh drive the actual production binary
# non-interactively.
BOOT_LOG="$SCRATCH/boot.log"
OUTER_SOCKET="$OUTER" "$LAUNCH" "$INNER" "$INNER_SESSION" </dev/null >"$BOOT_LOG" 2>&1

if ! tmux -L "$OUTER" has-session -t "$OUTER_SESSION" 2>/dev/null; then
  record FAIL "outer session boots" "session '$OUTER_SESSION' not found after orchard-shell boot; see $BOOT_LOG"
  cat "$BOOT_LOG" >&2
  echo "cannot continue without a booted outer session" >&2
  exit 1
fi
record PASS "outer session boots" "orchard-shell created session '$OUTER_SESSION' on socket '$OUTER'"

# --- recovery hook + M-r bind are loaded (issue #802) -----------------------
# The pane-died hook and the M-r bind only self-heal a dead pane if the conf
# actually loaded onto this outer server carries them, with a real binary
# path substituted in for @ORCHARD_SHELL@ -- this is orchard-shell's own boot
# path (resolveConfFor), not the render_conf helper above, so a PASS here
# proves the shipped materialise-on-boot behavior, not just this script's
# own rendering.
HOOKS_OUT="$(tmux -L "$OUTER" show-hooks -gw 2>/dev/null)"
if printf '%s\n' "$HOOKS_OUT" | grep -q 'pane-died' && \
   printf '%s\n' "$HOOKS_OUT" | grep -q 'recover-pane' && \
   ! printf '%s\n' "$HOOKS_OUT" | grep -q '@ORCHARD_SHELL@'; then
  record PASS "pane-died recovery hook is registered" "show-hooks -gw rendered pane-died recover-pane hook"
else
  record FAIL "pane-died recovery hook is registered" "show-hooks -gw missing rendered pane-died recover-pane hook"
fi

ROOT_KEYS_HOOK="$(tmux -L "$OUTER" list-keys -T root 2>/dev/null)"
if printf '%s\n' "$ROOT_KEYS_HOOK" | grep -q 'M-r'; then
  record PASS "M-r recovery bind is registered" "list-keys -T root contains M-r"
else
  record FAIL "M-r recovery bind is registered" "list-keys -T root missing M-r"
fi

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
# 5s (seq 1 50), not 2s: measured ~3.1s under normal load for a send-keys
# -triggered subprocess (shell dispatch -> tmux attach -> handshake) to
# register -- the same wait the "switch-client only moves the wrapper's
# own client" check further down already learned needs 5s, and every
# other "wait for a client/subprocess to attach" poll in this file (see
# below) already budgets 5s. This block was the one place still on 2s.
CMD01=""
for _ in $(seq 1 50); do
  CMD01="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_current_command}' 2>/dev/null)"
  [[ "$CMD01" == "tmux" ]] && break
  sleep 0.1
done
# 0.0 gets its own poll budget rather than a single-shot read right after
# 0.1's loop breaks: the sidebar is a Go binary (process fork+exec,
# runtime init, a GraphQL round-trip) that can settle slower than the
# inner tmux client's handshake above, and a single-shot read here raced
# false-negative under load (0.0 still reading "zsh" a moment before the
# sidebar replaced it).
CMD00=""
for _ in $(seq 1 50); do
  CMD00="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_current_command}' 2>/dev/null)"
  [[ "$CMD00" == "watch" || "$CMD00" == "orchard-sidebar" ]] && break
  sleep 0.1
done
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

# --- focus lands on the inner pane, not the sidebar (#747 live defect) -----
# Before the orchard-shell fix, tmux leaves the newly-split pane (0.0) active by
# default, and with `mouse off`/`prefix None` (the pre-fix outer.conf) there
# was no way to move focus at all -- everything typed went to the sidebar
# and never reached the inner shell. No select-pane has run anywhere above
# this point, so a PASS here is entirely orchard-shell's own doing.
BOOT_ACTIVE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{pane_index}' 2>/dev/null)"
if [[ "$BOOT_ACTIVE" == "1" ]]; then
  record PASS "inner pane (0.1) has focus after boot" "active pane index=1, no select-pane run by verify.sh"
else
  record FAIL "inner pane (0.1) has focus after boot" "active pane index=${BOOT_ACTIVE:-<none>}, want 1 -- #747 live defect: user's typing goes to the sidebar with no way to move focus"
fi

# --- outer server forwards pane OSC 52 (#797) ------------------------------
# tmux defaults set-clipboard to `external`, which drops OSC 52 from pane
# programs (Claude Code copy, inner tmux relay). outer.conf turns it on.
SET_CLIP="$(tmux -L "$OUTER" show -sv set-clipboard 2>/dev/null)"
if [[ "$SET_CLIP" == "on" ]]; then
  record PASS "outer server has set-clipboard on (#797)" "show -sv set-clipboard=on"
else
  record FAIL "outer server has set-clipboard on (#797)" "set-clipboard=${SET_CLIP:-<none>}, want on -- pane OSC 52 copies are dropped"
fi

# --- keystrokes typed right after boot land in the inner shell -------------
# Stronger than the pane_index check above: a real attached pty client,
# typing immediately, with zero select-pane call anywhere in this script so
# far (the one later in the swallow-check is below this point) -- proves the
# boot-time focus fix is enough on its own for a real client to type into
# the inner shell the instant it attaches, not just that tmux's internal
# "active pane" bookkeeping says 0.1.
BOOT_TYPE_SIZE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
BOOT_TYPE_COLS="${BOOT_TYPE_SIZE%x*}"
BOOT_TYPE_ROWS="${BOOT_TYPE_SIZE#*x}"
BOOT_TYPE_KEYS_B64="$(printf 'echo boottype747\r' | base64 | tr -d '\n')"
BOOT_TYPE_LOG="$SCRATCH/boottype.log"
BOOT_TYPE_OUT="$SCRATCH/boottypeattach.out"
: >"$BOOT_TYPE_OUT"

python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$BOOT_TYPE_COLS" "$BOOT_TYPE_ROWS" "$BOOT_TYPE_LOG" "$BOOT_TYPE_KEYS_B64" >"$BOOT_TYPE_OUT" 2>&1 &
PYWRAP_PID=$!

ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$BOOT_TYPE_OUT" 2>/dev/null; then
    ATTACHED=1
    break
  fi
  sleep 0.1
done

if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$BOOT_TYPE_OUT" | head -1 | cut -d= -f2)"
  sleep 1.2  # settle: attach handshake + injected keys (written ~0.5s in) + shell round-trip
  BOOT_TYPE_CAP="$(tmux -L "$INNER" capture-pane -p -t "$INNER_SESSION" -S -200 2>/dev/null)"
  if printf '%s' "$BOOT_TYPE_CAP" | grep -qF "boottype747"; then
    record PASS "keystrokes reach inner on boot, no prior select-pane" "'echo boottype747' landed in inner pane on first attach"
  else
    record FAIL "keystrokes reach inner on boot, no prior select-pane" "'boottype747' missing from inner capture -- typed input did not reach the inner shell"
  fi
  kill "$CHILD_ATTACH_PID" 2>/dev/null
else
  record FAIL "keystrokes reach inner on boot, no prior select-pane" "real pty client never attached (see $BOOT_TYPE_OUT)"
fi
[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# Pin to an exact, deterministic baseline size regardless of the calling
# environment's own tty (verify.sh has none, so orchard-shell's tput fallback
# already targets 160x45, but resize-window here makes the baseline explicit
# and exercises the window-resized hook at the same time).
tmux -L "$OUTER" resize-window -t "$OUTER_SESSION" -x 160 -y 45
check_width40 "initial layout (160x45)"

# --- idempotent-boot check: re-running orchard-shell must not error or duplicate
OUTER_SOCKET="$OUTER" "$LAUNCH" "$INNER" "$INNER_SESSION" </dev/null >"$SCRATCH/boot2.log" 2>&1
WINCOUNT="$(tmux -L "$OUTER" list-windows -t "$OUTER_SESSION" 2>/dev/null | wc -l | tr -d ' ')"
if [[ "$WINCOUNT" == "1" ]]; then
  record PASS "idempotent re-run" "still exactly 1 outer window after second orchard-shell invocation"
else
  record FAIL "idempotent re-run" "expected 1 outer window, found $WINCOUNT"
fi

REBOOT_ACTIVE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{pane_index}' 2>/dev/null)"
if [[ "$REBOOT_ACTIVE" == "1" ]]; then
  record PASS "inner pane (0.1) still focused after idempotent re-run" "active pane index=1"
else
  record FAIL "inner pane (0.1) still focused after idempotent re-run" "active pane index=${REBOOT_ACTIVE:-<none>}, want 1"
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
# Belt-and-suspenders, not load-bearing: orchard-shell's own boot-time fix
# (see "inner pane (0.1) has focus after boot" above) already leaves 0.1
# active by the time execution reaches here, and nothing between boot and
# this point calls select-pane on $OUTER. Kept explicit anyway so this
# check's precondition never silently depends on an invariant proven
# upstream by a DIFFERENT check -- key-table dispatch happens client-side
# before a byte is routed to any pane, so forcing focus here only sets up
# the precondition -- it doesn't touch the mechanism under test.
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

# --- M-s collapses the sidebar to the strip, and back ----------------------
# The keyboard half of the sidebar's collapse button. Like the prefix check
# above this needs a REAL attached pty client: M-s lives in the root key
# table, and key-table dispatch only runs for a real client's input --
# send-keys injects straight into a pane and would prove nothing.
#
# Two facts per press, not one: the pane width AND @sidebar_collapsed. The
# option is what outer.conf's resize hooks read to decide which width to
# re-pin to, so a press that resized without setting it would come undone on
# the next terminal resize. The pair also has to round-trip -- this check
# leaves $OUTER expanded at 40 for every check below it.
MS_SIZE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
MS_COLS="${MS_SIZE%x*}"
MS_ROWS="${MS_SIZE#*x}"
MS_KEYS_B64="$(printf '\x1bs' | base64 | tr -d '\n')"

# press_ms N: attach a real client at the window's current size, let the
# helper write M-s into it, then detach. Attaching at the CURRENT size means
# no window resize is involved, so what the width does afterwards is the
# binding's doing and nothing else's.
press_ms() {
  local out="$SCRATCH/ms$1.out"
  : >"$out"
  python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$MS_COLS" "$MS_ROWS" "$SCRATCH/ms$1.log" "$MS_KEYS_B64" >"$out" 2>&1 &
  PYWRAP_PID=$!
  for _ in $(seq 1 50); do
    grep -q '^PTY_PID=' "$out" 2>/dev/null && break
    sleep 0.1
  done
  grep -q '^PTY_PID=' "$out" 2>/dev/null || return 1
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$out" | head -1 | cut -d= -f2)"
  sleep 1.2  # attach handshake + the key written ~0.5s in + tmux dispatch
  kill "$CHILD_ATTACH_PID" 2>/dev/null
  kill "$PYWRAP_PID" 2>/dev/null
  wait "$PYWRAP_PID" 2>/dev/null
  CHILD_ATTACH_PID=""
  PYWRAP_PID=""
  return 0
}

ms_state() {
  printf '%s/%s' \
    "$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_width}' 2>/dev/null)" \
    "$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0" '#{@sidebar_collapsed}' 2>/dev/null)"
}

if press_ms 1; then
  MS_COLLAPSED="$(ms_state)"
  if [[ "$MS_COLLAPSED" == "3/1" ]]; then
    record PASS "M-s collapses the sidebar" "width/@sidebar_collapsed = $MS_COLLAPSED"
  else
    record FAIL "M-s collapses the sidebar" "width/@sidebar_collapsed = $MS_COLLAPSED, want 3/1"
  fi

  # a terminal resize while collapsed must not pop the sidebar back open:
  # the hooks re-pin to the collapsed width, not unconditionally to 40
  tmux -L "$OUTER" resize-window -t "$OUTER_SESSION:0" -x 200 -y 50 2>/dev/null
  MS_AFTER_RESIZE="$(ms_state)"
  if [[ "$MS_AFTER_RESIZE" == "3/1" ]]; then
    record PASS "collapsed sidebar survives an outer resize" "width/@sidebar_collapsed = $MS_AFTER_RESIZE"
  else
    record FAIL "collapsed sidebar survives an outer resize" "width/@sidebar_collapsed = $MS_AFTER_RESIZE, want 3/1"
  fi
  tmux -L "$OUTER" resize-window -t "$OUTER_SESSION:0" -x "$MS_COLS" -y "$MS_ROWS" 2>/dev/null

  if press_ms 2; then
    MS_EXPANDED="$(ms_state)"
    if [[ "$MS_EXPANDED" == "40/0" ]]; then
      record PASS "M-s expands the sidebar again" "width/@sidebar_collapsed = $MS_EXPANDED"
    else
      record FAIL "M-s expands the sidebar again" "width/@sidebar_collapsed = $MS_EXPANDED, want 40/0"
    fi
  else
    record FAIL "M-s expands the sidebar again" "real pty client never attached (see $SCRATCH/ms2.out)"
  fi
else
  record FAIL "M-s collapses the sidebar" "real pty client never attached (see $SCRATCH/ms1.out)"
  record FAIL "collapsed sidebar survives an outer resize" "skipped: no attached client"
  record FAIL "M-s expands the sidebar again" "skipped: no attached client"
fi
check_width40 "sidebar back at full width after the M-s round trip"

# --- a dragged width survives an outer resize -------------------------------
# The OUTER server owns the sidebar's width: the sidebar publishes what the
# user dragged the border to as main-pane-width, and outer.conf's two resize
# hooks re-pin the pane to THAT rather than to a hard-coded 40. Before
# this, a terminal resize pinned 40 back over the dragged width, the sidebar
# read the 40 as a fresh drag, and republished it -- so a drag survived
# exactly until the next resize.
#
# `resize-window` against a detached session fires window-resized (not
# client-resized), which is the hook path a scripted check can reach.
WIDTH_CHECK="dragged width survives an outer resize"
tmux -L "$OUTER" set -w -t "$OUTER_SESSION:0" main-pane-width 52 2>/dev/null
tmux -L "$OUTER" resize-window -t "$OUTER_SESSION:0" -x 150 -y 45 2>/dev/null
sleep 0.8
WIDTH_AFTER="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_width}' 2>/dev/null)"
if [[ "$WIDTH_AFTER" == "52" ]]; then
  record PASS "$WIDTH_CHECK" "main-pane-width=52 held through resize-window; pane_width=$WIDTH_AFTER"
else
  record FAIL "$WIDTH_CHECK" "pane_width=${WIDTH_AFTER:-<no pane>} after resize-window, want 52"
fi

# ...and with nothing published for this window, the hooks fall back to the
# server-wide default outer.conf sets -- a fresh wrapper that has never been
# dragged, not an error.
tmux -L "$OUTER" set -wu -t "$OUTER_SESSION:0" main-pane-width 2>/dev/null
tmux -L "$OUTER" resize-window -t "$OUTER_SESSION:0" -x 160 -y 45 2>/dev/null
sleep 0.8
check_width40 "unset width option falls back to 40"

# --- outer.conf re-sources cleanly on an already-configured server ---------
# Regression guard: `unbind -a -T prefix` (after `set -g prefix None`) used
# to error "table prefix doesn't exist" on any RE-source of an
# already-configured server -- `unbind -a` drops a key table to zero
# bindings, tmux then garbage-collects a table with nothing left in it, so
# the table genuinely didn't exist by the second load. This matters
# because outer.conf gets hot-applied via `source-file` against the live
# wrapper after every later change, not just loaded once at boot -- $OUTER
# already loaded it once when it booted above, so sourcing it here twice
# more reproduces exactly that repeated hot-apply.
tmux -L "$OUTER" source-file "$RENDERED_CONF" 2>"$SCRATCH/resource1.err"
RESOURCE1_RC=$?
tmux -L "$OUTER" source-file "$RENDERED_CONF" 2>"$SCRATCH/resource2.err"
RESOURCE2_RC=$?
if [[ "$RESOURCE1_RC" -eq 0 && "$RESOURCE2_RC" -eq 0 ]]; then
  record PASS "outer.conf re-sources idempotently" "two repeated source-file calls against the already-booted \$OUTER session both exit 0"
else
  record FAIL "outer.conf re-sources idempotently" "exit codes: first=$RESOURCE1_RC second=$RESOURCE2_RC stderr: $(cat "$SCRATCH/resource1.err" "$SCRATCH/resource2.err" 2>/dev/null | tr '\n' ' ')"
fi

# --- outer root table has no dangerous default mouse bindings --------------
# Stock tmux binds MouseDown3Pane/M-MouseDown3Pane to a `display-menu`
# popup (Split/Swap/Kill/Respawn/Zoom against the OUTER pane/window/
# session) whenever the pane isn't already in a mode or a mouse-tracking
# app, and MouseDown3Status(Left)/M- variants to window/session menus with
# the same Kill/Respawn/Swap actions. A right-click meant to focus/forward
# to the inner session could instead kill or respawn a pane at the OUTER
# layer. outer.conf must strip every path to one of these menus and
# rebind right-click to forward like every other mouse event.
ROOT_KEYS="$(tmux -L "$OUTER" list-keys -T root 2>/dev/null)"
DANGEROUS_HIT="$(printf '%s\n' "$ROOT_KEYS" | grep -E 'display-menu|kill-|split-window|respawn|swap-')"
if [[ -z "$DANGEROUS_HIT" ]]; then
  record PASS "outer root table has no dangerous mouse bindings" "list-keys -T root contains no display-menu/kill-/split-window/respawn/swap- binding"
else
  record FAIL "outer root table has no dangerous mouse bindings" "found: $(printf '%s' "$DANGEROUS_HIT" | tr '\n' ' ')"
fi

# --- outer forwards right-click INTO the pane -------------------------------
# The sidebar's row menu (rename/close a session) is drawn by orchard-sidebar
# itself, so the right-click has to reach it: outer.conf rebinds
# MouseDown3Pane to the same unconditional forward MouseDown1Pane does by
# default. The check above proves tmux's own display-menu is gone; this one
# proves something took its place, rather than right-click landing nowhere.
RIGHT_FWD="$(printf '%s\n' "$ROOT_KEYS" | grep 'MouseDown3Pane' | head -1)"
if printf '%s\n' "$RIGHT_FWD" | grep -q 'send-keys -M'; then
  record PASS "outer forwards right-click into the pane" "${RIGHT_FWD## }"
else
  record FAIL "outer forwards right-click into the pane" "MouseDown3Pane is: ${RIGHT_FWD:-<unbound>}"
fi

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

# --- mouse click focuses the right pane, and typed keys land there ---------
# Hard gate (issue #747, coordinator follow-up): a mouse click must focus
# whichever pane it lands in, and a subsequent keystroke must reach that
# pane immediately -- this is the entire point of `mouse on` in outer.conf.
# Geometry is queried live (not hardcoded) so this stays correct regardless
# of whatever size an earlier check left the window at.
MOUSE_SIZE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
MOUSE_COLS="${MOUSE_SIZE%x*}"
MOUSE_ROWS="${MOUSE_SIZE#*x}"
P0_LEFT="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_left}' 2>/dev/null)"
P0_WIDTH="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_width}' 2>/dev/null)"
P1_LEFT="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_left}' 2>/dev/null)"
P1_WIDTH="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_width}' 2>/dev/null)"
CLICK_ROW=5
# SGR mouse columns/rows are 1-based; pane_left/width are 0-based -- a
# mid-pane column keeps every click well clear of the border itself.
P0_CLICK_COL=$(( P0_LEFT + P0_WIDTH / 2 + 1 ))
P1_CLICK_COL=$(( P1_LEFT + P1_WIDTH / 2 + 1 ))

# -- c1: click into 0.0 (the sidebar) focuses it --
C1_KEYS_B64="$(python3 -c "
import base64
seq = b'\x1b[<0;${P0_CLICK_COL};${CLICK_ROW}M\x1b[<0;${P0_CLICK_COL};${CLICK_ROW}m'
print(base64.b64encode(seq).decode())
")"
C1_OUT="$SCRATCH/mouse_c1.out"
: >"$C1_OUT"
python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$MOUSE_COLS" "$MOUSE_ROWS" "$SCRATCH/mouse_c1.log" "$C1_KEYS_B64" >"$C1_OUT" 2>&1 &
PYWRAP_PID=$!
ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$C1_OUT" 2>/dev/null; then ATTACHED=1; break; fi
  sleep 0.1
done
if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$C1_OUT" | head -1 | cut -d= -f2)"
  sleep 1.0
  C1_ACTIVE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{pane_index}' 2>/dev/null)"
  C1_MODE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_in_mode}' 2>/dev/null)"
  if [[ "$C1_ACTIVE" == "0" && "$C1_MODE" == "0" ]]; then
    record PASS "mouse click focuses 0.0 (sidebar)" "active pane index=0, pane_in_mode=0 (no outer copy-mode)"
  else
    record FAIL "mouse click focuses 0.0 (sidebar)" "active pane index=${C1_ACTIVE:-<none>} (want 0), pane_in_mode=${C1_MODE:-<none>} (want 0)"
  fi
  kill "$CHILD_ATTACH_PID" 2>/dev/null
else
  record FAIL "mouse click focuses 0.0 (sidebar)" "real pty client never attached (see $C1_OUT)"
fi
[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# -- c2 + c3: click into 0.1 (inner) focuses it, then a typed keystroke
# lands in the inner shell -- one gesture proving both halves the
# coordinator asked to be a hard gate: click-to-focus AND
# keys-land-immediately-after.
C2_KEYS_B64="$(python3 -c "
import base64
seq = b'\x1b[<0;${P1_CLICK_COL};${CLICK_ROW}M\x1b[<0;${P1_CLICK_COL};${CLICK_ROW}m' + b'echo click747\r'
print(base64.b64encode(seq).decode())
")"
C2_OUT="$SCRATCH/mouse_c2.out"
: >"$C2_OUT"
python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$MOUSE_COLS" "$MOUSE_ROWS" "$SCRATCH/mouse_c2.log" "$C2_KEYS_B64" >"$C2_OUT" 2>&1 &
PYWRAP_PID=$!
ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$C2_OUT" 2>/dev/null; then ATTACHED=1; break; fi
  sleep 0.1
done
if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$C2_OUT" | head -1 | cut -d= -f2)"
  sleep 1.2
  C2_ACTIVE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{pane_index}' 2>/dev/null)"
  C2_MODE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_in_mode}' 2>/dev/null)"
  if [[ "$C2_ACTIVE" == "1" && "$C2_MODE" == "0" ]]; then
    record PASS "mouse click focuses 0.1 (inner)" "active pane index=1, pane_in_mode=0 (no outer copy-mode)"
  else
    record FAIL "mouse click focuses 0.1 (inner)" "active pane index=${C2_ACTIVE:-<none>} (want 1), pane_in_mode=${C2_MODE:-<none>} (want 0)"
  fi
  C2_CAP="$(tmux -L "$INNER" capture-pane -p -t "$INNER_SESSION" -S -200 2>/dev/null)"
  if printf '%s' "$C2_CAP" | grep -qF "click747"; then
    record PASS "keystrokes after mouse click land in inner shell" "'echo click747' landed in inner pane right after the click"
  else
    record FAIL "keystrokes after mouse click land in inner shell" "'click747' missing from inner capture"
  fi
  kill "$CHILD_ATTACH_PID" 2>/dev/null
else
  record FAIL "mouse click focuses 0.1 (inner)" "real pty client never attached (see $C2_OUT)"
  record FAIL "keystrokes after mouse click land in inner shell" "real pty client never attached (see $C2_OUT)"
fi
[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# --- nested mouse: wheel scroll and inner sub-pane selection ---------------
# Coordinator follow-up: does forwarding break wheel scroll or the INNER
# tmux's own pane selection once it has its own multi-pane layout? Answered
# empirically here (not assumed) -- each assertion is what was actually
# observed driving this exact pipeline by hand before writing this check.
# Layout is forced explicitly rather than trusted from earlier checks, so
# this stays correct on its own regardless of what ran before it.
tmux -L "$INNER" set -g mouse on
tmux -L "$INNER" select-layout -t "$INNER_SESSION" even-horizontal >/dev/null 2>&1
if [[ "$(tmux -L "$INNER" list-panes -t "$INNER_SESSION" 2>/dev/null | wc -l | tr -d ' ')" -lt 2 ]]; then
  tmux -L "$INNER" split-window -h -t "$INNER_SESSION"
  tmux -L "$INNER" select-layout -t "$INNER_SESSION" even-horizontal >/dev/null 2>&1
fi

INNER_PANES_SORTED="$(tmux -L "$INNER" list-panes -t "$INNER_SESSION" -F '#{pane_index} #{pane_left} #{pane_width}' 2>/dev/null | sort -k2,2n)"
IDX_LEFT="$(printf '%s\n' "$INNER_PANES_SORTED" | head -1 | awk '{print $1}')"
LEFT_L="$(printf '%s\n' "$INNER_PANES_SORTED" | head -1 | awk '{print $2}')"
LEFT_W="$(printf '%s\n' "$INNER_PANES_SORTED" | head -1 | awk '{print $3}')"
IDX_RIGHT="$(printf '%s\n' "$INNER_PANES_SORTED" | tail -1 | awk '{print $1}')"
RIGHT_L="$(printf '%s\n' "$INNER_PANES_SORTED" | tail -1 | awk '{print $2}')"
RIGHT_W="$(printf '%s\n' "$INNER_PANES_SORTED" | tail -1 | awk '{print $3}')"

# Start on the right pane so the click test below (targeting the left pane)
# actually changes the active pane instead of trivially confirming a
# no-op.
tmux -L "$INNER" select-pane -t "$INNER_SESSION.$IDX_RIGHT"

MOUSE2_SIZE="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
MOUSE2_COLS="${MOUSE2_SIZE%x*}"
MOUSE2_ROWS="${MOUSE2_SIZE#*x}"
OUTER_P1_LEFT="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_left}' 2>/dev/null)"
OUTER_P1_TOP="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_top}' 2>/dev/null)"
IROW=5
# Inner-relative column 1 renders at outer-absolute column
# OUTER_P1_LEFT+1 -- the inner client's own screen starts exactly where
# outer pane 0.1 does. Same reasoning for rows via OUTER_P1_TOP.
ILEFT_COL=$(( OUTER_P1_LEFT + LEFT_L + LEFT_W / 2 + 1 ))
IRIGHT_COL=$(( OUTER_P1_LEFT + RIGHT_L + RIGHT_W / 2 + 1 ))
OROW=$(( OUTER_P1_TOP + IROW + 1 ))

# -- forwarded click selects the correct INNER sub-pane (not just outer's
# own 0.0 vs 0.1) -- proves SGR coordinates survive translation across
# BOTH nesting boundaries with nothing beyond `mouse on` at each level.
IP0_KEYS_B64="$(python3 -c "
import base64
seq = b'\x1b[<0;${ILEFT_COL};${OROW}M\x1b[<0;${ILEFT_COL};${OROW}m'
print(base64.b64encode(seq).decode())
")"
IP0_OUT="$SCRATCH/mouse_innerpane.out"
: >"$IP0_OUT"
python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$MOUSE2_COLS" "$MOUSE2_ROWS" "$SCRATCH/mouse_innerpane.log" "$IP0_KEYS_B64" >"$IP0_OUT" 2>&1 &
PYWRAP_PID=$!
ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$IP0_OUT" 2>/dev/null; then ATTACHED=1; break; fi
  sleep 0.1
done
if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$IP0_OUT" | head -1 | cut -d= -f2)"
  sleep 1.0
  IP_ACTIVE="$(tmux -L "$INNER" display -p -t "$INNER_SESSION" '#{pane_index}' 2>/dev/null)"
  OUTER_STAYED="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{pane_index}' 2>/dev/null)"
  if [[ "$IP_ACTIVE" == "$IDX_LEFT" && "$OUTER_STAYED" == "1" ]]; then
    record PASS "forwarded click selects correct INNER sub-pane" "clicked inner pane $IDX_LEFT (left); inner active=$IP_ACTIVE, outer stayed on 0.1"
  else
    record FAIL "forwarded click selects correct INNER sub-pane" "clicked inner pane $IDX_LEFT (left); inner active=${IP_ACTIVE:-<none>} (want $IDX_LEFT), outer active=${OUTER_STAYED:-<none>} (want 1)"
  fi
  kill "$CHILD_ATTACH_PID" 2>/dev/null
else
  record FAIL "forwarded click selects correct INNER sub-pane" "real pty client never attached (see $IP0_OUT)"
fi
[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# -- true wheel-up (SGR button 64) over the inner pane enters ITS OWN
# copy-mode -- nested scrollback, exactly like non-nested tmux -- without
# ever opening outer's copy-mode.
tmux -L "$INNER" select-pane -t "$INNER_SESSION.$IDX_RIGHT"
WHEELUP_KEYS_B64="$(python3 -c "
import base64
seq = b'\x1b[<64;${IRIGHT_COL};${OROW}M' * 3
print(base64.b64encode(seq).decode())
")"
WHEELUP_OUT="$SCRATCH/wheelup.out"
: >"$WHEELUP_OUT"
python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$MOUSE2_COLS" "$MOUSE2_ROWS" "$SCRATCH/wheelup.log" "$WHEELUP_KEYS_B64" >"$WHEELUP_OUT" 2>&1 &
PYWRAP_PID=$!
ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$WHEELUP_OUT" 2>/dev/null; then ATTACHED=1; break; fi
  sleep 0.1
done
if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$WHEELUP_OUT" | head -1 | cut -d= -f2)"
  sleep 1.0
  WHEELUP_MODE="$(tmux -L "$INNER" display -p -t "$INNER_SESSION.$IDX_RIGHT" '#{pane_in_mode}' 2>/dev/null)"
  OUTER_MODE_AFTER_WHEEL="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_in_mode}' 2>/dev/null)"
  if [[ "$WHEELUP_MODE" == "1" && "$OUTER_MODE_AFTER_WHEEL" == "0" ]]; then
    record PASS "wheel-up over inner pane enters inner copy-mode" "inner pane_in_mode=1 (scrollback), outer stayed out of its own copy-mode"
  else
    record FAIL "wheel-up over inner pane enters inner copy-mode" "inner pane_in_mode=${WHEELUP_MODE:-<none>} (want 1), outer pane_in_mode=${OUTER_MODE_AFTER_WHEEL:-<none>} (want 0)"
  fi
  # 'q' is an ordinary forwarded keystroke, not mouse plumbing -- exits
  # inner copy-mode the normal tmux way and leaves state clean for
  # whatever runs next.
  tmux -L "$INNER" send-keys -t "$INNER_SESSION.$IDX_RIGHT" q
  kill "$CHILD_ATTACH_PID" 2>/dev/null
else
  record FAIL "wheel-up over inner pane enters inner copy-mode" "real pty client never attached (see $WHEELUP_OUT)"
fi
[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# -- wheel-down (SGR button 65) is a pre-existing upstream tmux gap, not a
# regression: stock tmux 3.6a ships NO default root-table WheelDownPane
# binding at all (verified via `list-keys -T root` on a plain -f /dev/null
# server -- the same gap exists with zero nesting involved). outer.conf's
# explicit `bind -n WheelDownPane send-keys -M` means the OUTER layer no
# longer swallows it -- forwarded one level deeper, where it lands on the
# same gap vanilla tmux already has. Gated on the one thing that WOULD be
# a wrapper-introduced regression: corruption, a wrong pane, or an
# accidental outer copy-mode.
tmux -L "$INNER" select-pane -t "$INNER_SESSION.$IDX_RIGHT"
WHEELDOWN_PRECAP="$(tmux -L "$INNER" capture-pane -p -t "$INNER_SESSION.$IDX_RIGHT" 2>/dev/null)"
WHEELDOWN_KEYS_B64="$(python3 -c "
import base64
seq = b'\x1b[<65;${IRIGHT_COL};${OROW}M' * 3
print(base64.b64encode(seq).decode())
")"
WHEELDOWN_OUT="$SCRATCH/wheeldown.out"
: >"$WHEELDOWN_OUT"
python3 "$PYHELPER" "$OUTER" "$OUTER_SESSION" "$MOUSE2_COLS" "$MOUSE2_ROWS" "$SCRATCH/wheeldown.log" "$WHEELDOWN_KEYS_B64" >"$WHEELDOWN_OUT" 2>&1 &
PYWRAP_PID=$!
ATTACHED=0
for _ in $(seq 1 50); do
  if grep -q '^PTY_PID=' "$WHEELDOWN_OUT" 2>/dev/null; then ATTACHED=1; break; fi
  sleep 0.1
done
if [[ "$ATTACHED" -eq 1 ]]; then
  CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$WHEELDOWN_OUT" | head -1 | cut -d= -f2)"
  sleep 1.0
  WHEELDOWN_MODE="$(tmux -L "$INNER" display -p -t "$INNER_SESSION.$IDX_RIGHT" '#{pane_in_mode}' 2>/dev/null)"
  OUTER_MODE_AFTER_WHEELDOWN="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_in_mode}' 2>/dev/null)"
  OUTER_ACTIVE_AFTER_WHEELDOWN="$(tmux -L "$OUTER" display -p -t "$OUTER_SESSION" '#{pane_index}' 2>/dev/null)"
  WHEELDOWN_POSTCAP="$(tmux -L "$INNER" capture-pane -p -t "$INNER_SESSION.$IDX_RIGHT" 2>/dev/null)"
  if [[ "$WHEELDOWN_MODE" == "0" && "$OUTER_MODE_AFTER_WHEELDOWN" == "0" && "$OUTER_ACTIVE_AFTER_WHEELDOWN" == "1" && "$WHEELDOWN_PRECAP" == "$WHEELDOWN_POSTCAP" ]]; then
    record PASS "wheel-down over inner pane is inert, not corrupting" "no outer/inner copy-mode entered, pane content unchanged (stock tmux has no default WheelDownPane binding at any nesting depth)"
  else
    CAPCHANGED="no"; [[ "$WHEELDOWN_PRECAP" != "$WHEELDOWN_POSTCAP" ]] && CAPCHANGED="YES"
    record FAIL "wheel-down over inner pane is inert, not corrupting" "inner pane_in_mode=${WHEELDOWN_MODE:-<none>} outer pane_in_mode=${OUTER_MODE_AFTER_WHEELDOWN:-<none>} outer active=${OUTER_ACTIVE_AFTER_WHEELDOWN:-<none>} capture-changed=$CAPCHANGED"
  fi
  kill "$CHILD_ATTACH_PID" 2>/dev/null
else
  record FAIL "wheel-down over inner pane is inert, not corrupting" "real pty client never attached (see $WHEELDOWN_OUT)"
fi
[[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
wait "$PYWRAP_PID" 2>/dev/null
PYWRAP_PID=""
CHILD_ATTACH_PID=""

# Restore outer's own baseline window size (checks above may have resized
# it) and inner's layout for whatever runs after this block.
tmux -L "$OUTER" resize-window -t "$OUTER_SESSION" -x 160 -y 45 2>/dev/null

# --- sidebar row click -> type -> lands in inner shell (hand-back path) ----
# Coordinator follow-up: prove the FULL production pipeline end to end,
# not just its parts -- a real orchard-sidebar binary (not a stub), a real
# mouse click on a real rendered row, driving the real
# switchClient -> handBackFocus chain, with a subsequent keystroke landing
# in the inner shell right after. Dedicated sockets/session/binary names
# throughout so this can't disturb, or be disturbed by, any check above.
HB_OUTER="orchard-shell-test3"
HB_INNER="orchard-inner-test3"
# orchard-shell hardcodes its own outer session name to "shell" regardless
# of caller (see cmd/orchard-shell/outer.go: outerSessionName) -- sockets,
# not session
# names, are what keep this instance isolated from $OUTER/$OUTER2 above.
HB_OUTER_SESSION="shell"
HB_BOOT_SESSION="boot3"
HB_TARGET_SESSION="hbtest747"
HB_REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." >/dev/null 2>&1 && pwd)"

HB_CHECK1="sidebar renders row from daemon"
HB_CHECK2="sidebar row click switches inner session"
HB_CHECK3="sidebar row click hands focus back to inner (0.1)"
HB_CHECK4="typed keys after row click land in inner shell"

tmux -L "$HB_OUTER" kill-server 2>/dev/null
tmux -L "$HB_INNER" kill-server 2>/dev/null

tmux -L "$HB_INNER" -f /dev/null new-session -d -s "$HB_BOOT_SESSION" -x 200 -y 50
tmux -L "$HB_INNER" -f /dev/null new-session -d -s "$HB_TARGET_SESSION" -x 200 -y 50

# Fake local GraphQL daemon: unconditionally returns one tmuxSessions row
# named $HB_TARGET_SESSION for any POST -- real enough to satisfy both
# fetchFast's and fetchSlow's Go structs (encoding/json silently ignores
# whichever half of the payload a given struct doesn't ask for) without a
# real orchard daemon.
HB_DAEMON_PY="$SCRATCH/hb_daemon.py"
cat > "$HB_DAEMON_PY" <<'HBPY_EOF'
import http.server, json, sys

RESPONSE = json.dumps({
    "data": {
        "workView": {
            "claudeInstances": [],
            "tmuxSessions": [{
                "name": sys.argv[1],
                "attached": False,
                "createdAt": "2026-01-01T00:00:00Z",
                "windows": [{"panes": [{"paneId": "%1"}]}],
            }],
            "meta": {"failureReason": None},
            "repos": [],
        }
    }
}).encode()

class Handler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(RESPONSE)))
        self.end_headers()
        self.wfile.write(RESPONSE)

    def log_message(self, *args):
        pass  # keep verify.sh's own stdout clean

srv = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
print(f'PORT={srv.server_address[1]}', flush=True)
srv.serve_forever()
HBPY_EOF

HB_DAEMON_LOG="$SCRATCH/hb_daemon.log"
python3 "$HB_DAEMON_PY" "$HB_TARGET_SESSION" >"$HB_DAEMON_LOG" 2>&1 &
HB_DAEMON_PID=$!
HB_PORT=""
for _ in $(seq 1 50); do
  if grep -q '^PORT=' "$HB_DAEMON_LOG" 2>/dev/null; then
    HB_PORT="$(grep '^PORT=' "$HB_DAEMON_LOG" | head -1 | cut -d= -f2)"
    break
  fi
  sleep 0.1
done

if [[ -z "$HB_PORT" ]]; then
  record FAIL "$HB_CHECK1" "fake daemon never reported a port; see $HB_DAEMON_LOG"
  record FAIL "$HB_CHECK2" "no daemon -- see $HB_CHECK1"
  record FAIL "$HB_CHECK3" "no daemon -- see $HB_CHECK1"
  record FAIL "$HB_CHECK4" "no daemon -- see $HB_CHECK1"
else
  # -ldflags -X overrides the package-level graphqlURL (main.go) / wsURL
  # (subscribe.go) vars those files already declare as vars for exactly
  # this (hermetic tests). wsURL points at a closed port: the push lane is
  # expected to dial-fail and degrade to the 2s poll, which is all this
  # check needs -- the row comes from the fast lane alone.
  HB_BIN="$SCRATCH/orchard-sidebar-hbtest"
  HB_BUILD_LOG="$SCRATCH/hb_build.log"
  if ! (cd "$HB_REPO_ROOT" && go build -o "$HB_BIN" \
      -ldflags "-X main.graphqlURL=http://127.0.0.1:${HB_PORT}/graphql -X main.wsURL=ws://127.0.0.1:1/graphql" \
      ./cmd/orchard-sidebar) >"$HB_BUILD_LOG" 2>&1; then
    record FAIL "$HB_CHECK1" "go build (fake-daemon binary) failed; see $HB_BUILD_LOG"
    record FAIL "$HB_CHECK2" "build failed -- see $HB_CHECK1"
    record FAIL "$HB_CHECK3" "build failed -- see $HB_CHECK1"
    record FAIL "$HB_CHECK4" "build failed -- see $HB_CHECK1"
  else
    # bin/orchard-sidebar is what orchard-shell execs when present (see
    # cmd/orchard-shell/discover.go's sidebar resolution) -- swap in the fake-daemon build
    # for this boot. Backup-and-swap uses `mv`, never `cp`, onto the live
    # path: `cp` writes into the EXISTING inode in place, and overwriting
    # a Mach-O that a just-launched process is still executing from gets
    # that process SIGKILLed by the kernel (observed on macOS) -- `mv` only
    # repoints the directory entry, so a process already running from the
    # old inode (this wrapper's own pane 0.0, or the primary $OUTER
    # session's still-live sidebar from the very first boot check above)
    # is untouched either way. Restore is the cleanup() trap's job (see
    # top of file) so the swapped-in binary survives for this whole check
    # regardless of how it exits, and nothing can leave the repo's real
    # binary replaced.
    HB_REPO_BIN="$HB_REPO_ROOT/bin/orchard-sidebar"
    HB_REPO_BIN_BACKUP=""
    mkdir -p "$HB_REPO_ROOT/bin"
    if [[ -e "$HB_REPO_BIN" ]]; then
      HB_REPO_BIN_BACKUP="$SCRATCH/orchard-sidebar.orig-backup"
      mv "$HB_REPO_BIN" "$HB_REPO_BIN_BACKUP"
    fi
    cp "$HB_BIN" "$HB_REPO_BIN"
    chmod +x "$HB_REPO_BIN"

    HB_BOOT_LOG="$SCRATCH/hb_boot.log"
    OUTER_SOCKET="$HB_OUTER" "$LAUNCH" "$HB_INNER" "$HB_BOOT_SESSION" </dev/null >"$HB_BOOT_LOG" 2>&1

    if ! tmux -L "$HB_OUTER" has-session -t "$HB_OUTER_SESSION" 2>/dev/null; then
      record FAIL "$HB_CHECK1" "outer session never booted; see $HB_BOOT_LOG"
      record FAIL "$HB_CHECK2" "outer never booted -- see $HB_CHECK1"
      record FAIL "$HB_CHECK3" "outer never booted -- see $HB_CHECK1"
      record FAIL "$HB_CHECK4" "outer never booted -- see $HB_CHECK1"
    else
      HB_INNER_TTY="$(tmux -L "$HB_OUTER" display -p -t "$HB_OUTER_SESSION:0.1" '#{pane_tty}' 2>/dev/null)"

      HB_ROW_LINE=""
      for _ in $(seq 1 50); do
        HB_CAP="$(tmux -L "$HB_OUTER" capture-pane -p -t "$HB_OUTER_SESSION:0.0" 2>/dev/null)"
        if printf '%s\n' "$HB_CAP" | grep -qF "$HB_TARGET_SESSION"; then
          HB_ROW_LINE="$(printf '%s\n' "$HB_CAP" | grep -nF "$HB_TARGET_SESSION" | head -1 | cut -d: -f1)"
          break
        fi
        sleep 0.2
      done

      if [[ -z "$HB_ROW_LINE" ]]; then
        record FAIL "$HB_CHECK1" "'$HB_TARGET_SESSION' never appeared in pane 0.0; see $HB_BOOT_LOG"
        record FAIL "$HB_CHECK2" "no row rendered -- see $HB_CHECK1"
        record FAIL "$HB_CHECK3" "no row rendered -- see $HB_CHECK1"
        record FAIL "$HB_CHECK4" "no row rendered -- see $HB_CHECK1"
      else
        record PASS "$HB_CHECK1" "'$HB_TARGET_SESSION' row visible at capture line $HB_ROW_LINE"

        HB_P0_LEFT="$(tmux -L "$HB_OUTER" display -p -t "$HB_OUTER_SESSION:0.0" '#{pane_left}' 2>/dev/null)"
        HB_P0_TOP="$(tmux -L "$HB_OUTER" display -p -t "$HB_OUTER_SESSION:0.0" '#{pane_top}' 2>/dev/null)"
        HB_P0_WIDTH="$(tmux -L "$HB_OUTER" display -p -t "$HB_OUTER_SESSION:0.0" '#{pane_width}' 2>/dev/null)"
        HB_CLICK_COL=$(( HB_P0_LEFT + HB_P0_WIDTH / 2 + 1 ))
        # capture-pane -p emits one line per screen row starting at the
        # pane's own first row, so grep's 1-based line number already IS
        # the pane-relative screen row -- add pane_top and it's an
        # outer-absolute, 1-based SGR row with no further adjustment.
        HB_CLICK_ROW=$(( HB_P0_TOP + HB_ROW_LINE ))

        HB_SIZE="$(tmux -L "$HB_OUTER" display -p -t "$HB_OUTER_SESSION" '#{window_width}x#{window_height}' 2>/dev/null)"
        HB_COLS="${HB_SIZE%x*}"
        HB_ROWS="${HB_SIZE#*x}"

        HB_CLICK_KEYS_B64="$(python3 -c "
import base64
seq = b'\x1b[<0;${HB_CLICK_COL};${HB_CLICK_ROW}M\x1b[<0;${HB_CLICK_COL};${HB_CLICK_ROW}m'
print(base64.b64encode(seq).decode())
")"
        HB_CLICK_OUT="$SCRATCH/hb_click.out"
        : >"$HB_CLICK_OUT"
        python3 "$PYHELPER" "$HB_OUTER" "$HB_OUTER_SESSION" "$HB_COLS" "$HB_ROWS" "$SCRATCH/hb_click.log" "$HB_CLICK_KEYS_B64" >"$HB_CLICK_OUT" 2>&1 &
        PYWRAP_PID=$!
        ATTACHED=0
        for _ in $(seq 1 50); do
          if grep -q '^PTY_PID=' "$HB_CLICK_OUT" 2>/dev/null; then ATTACHED=1; break; fi
          sleep 0.1
        done

        if [[ "$ATTACHED" -ne 1 ]]; then
          record FAIL "$HB_CHECK2" "real pty client never attached (see $HB_CLICK_OUT)"
          record FAIL "$HB_CHECK3" "real pty client never attached (see $HB_CLICK_OUT)"
          record FAIL "$HB_CHECK4" "real pty client never attached (see $HB_CLICK_OUT)"
        else
          CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$HB_CLICK_OUT" | head -1 | cut -d= -f2)"

          # Poll rather than a fixed sleep: the click has to travel through
          # a REAL subprocess chain inside the sidebar binary itself
          # (switchClient's `tmux switch-client`, then handBackFocus's
          # `tmux select-pane`), not tmux's own in-process key-table
          # dispatch -- give it a real deadline instead of guessing a delay.
          HB_SWITCHED=0
          HB_HANDED_BACK=0
          HB_CLIENT_SESS=""
          HB_OUTER_ACTIVE=""
          for _ in $(seq 1 40); do
            HB_CLIENT_SESS="$(tmux -L "$HB_INNER" list-clients -F '#{client_tty} #{client_session}' 2>/dev/null | awk -v t="$HB_INNER_TTY" '$1==t{print $2}')"
            HB_OUTER_ACTIVE="$(tmux -L "$HB_OUTER" display -p -t "$HB_OUTER_SESSION" '#{pane_index}' 2>/dev/null)"
            [[ "$HB_CLIENT_SESS" == "$HB_TARGET_SESSION" ]] && HB_SWITCHED=1
            [[ "$HB_OUTER_ACTIVE" == "1" ]] && HB_HANDED_BACK=1
            if [[ "$HB_SWITCHED" -eq 1 && "$HB_HANDED_BACK" -eq 1 ]]; then break; fi
            sleep 0.1
          done

          if [[ "$HB_SWITCHED" -eq 1 ]]; then
            record PASS "$HB_CHECK2" "wrapper's inner client (tty=$HB_INNER_TTY) now attached to $HB_TARGET_SESSION"
          else
            record FAIL "$HB_CHECK2" "wrapper's inner client session=${HB_CLIENT_SESS:-<none>}, want $HB_TARGET_SESSION"
          fi
          if [[ "$HB_HANDED_BACK" -eq 1 ]]; then
            record PASS "$HB_CHECK3" "outer active pane index=1 after the click, no manual select-pane"
          else
            record FAIL "$HB_CHECK3" "outer active pane index=${HB_OUTER_ACTIVE:-<none>}, want 1"
          fi

          kill "$CHILD_ATTACH_PID" 2>/dev/null
          [[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
          wait "$PYWRAP_PID" 2>/dev/null
          PYWRAP_PID=""
          CHILD_ATTACH_PID=""

          # Second, independent attach: proves a REAL client typing right
          # after the click lands in the inner shell -- the actual
          # end-user gesture (click a row, immediately type), not just an
          # internal tmux-state assertion.
          HB_TYPE_KEYS_B64="$(printf 'echo handback747\r' | base64 | tr -d '\n')"
          HB_TYPE_OUT="$SCRATCH/hb_type.out"
          : >"$HB_TYPE_OUT"
          python3 "$PYHELPER" "$HB_OUTER" "$HB_OUTER_SESSION" "$HB_COLS" "$HB_ROWS" "$SCRATCH/hb_type.log" "$HB_TYPE_KEYS_B64" >"$HB_TYPE_OUT" 2>&1 &
          PYWRAP_PID=$!
          ATTACHED=0
          for _ in $(seq 1 50); do
            if grep -q '^PTY_PID=' "$HB_TYPE_OUT" 2>/dev/null; then ATTACHED=1; break; fi
            sleep 0.1
          done
          if [[ "$ATTACHED" -eq 1 ]]; then
            CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$HB_TYPE_OUT" | head -1 | cut -d= -f2)"
            sleep 1.2
            HB_TYPE_CAP="$(tmux -L "$HB_INNER" capture-pane -p -t "$HB_TARGET_SESSION" -S -200 2>/dev/null)"
            if printf '%s' "$HB_TYPE_CAP" | grep -qF "handback747"; then
              record PASS "$HB_CHECK4" "'echo handback747' landed in $HB_TARGET_SESSION right after the click"
            else
              record FAIL "$HB_CHECK4" "'handback747' missing from $HB_TARGET_SESSION capture"
            fi
            kill "$CHILD_ATTACH_PID" 2>/dev/null
          else
            record FAIL "$HB_CHECK4" "real pty client never re-attached (see $HB_TYPE_OUT)"
          fi
        fi
        [[ -n "$PYWRAP_PID" ]] && kill "$PYWRAP_PID" 2>/dev/null
        wait "$PYWRAP_PID" 2>/dev/null
        PYWRAP_PID=""
        CHILD_ATTACH_PID=""
      fi
    fi
  fi
fi

kill "$HB_DAEMON_PID" 2>/dev/null
tmux -L "$HB_OUTER" kill-server 2>/dev/null
tmux -L "$HB_INNER" kill-server 2>/dev/null


# --- switch-client only moves the wrapper's own client ----------------------
# Regression guard for the live-pass defect (#747 defect 2): on a SHARED
# inner server, a plain `switch-client -t <session>` (no -c) lets tmux pick
# an ARBITRARY attached client to move -- in the wild this hijacked the
# user's unrelated terminal instead of the wrapper's own pane. The fix
# scopes every switch to -c $ORCHARD_TMUX_CLIENT, which orchard-shell resolves
# from outer pane 0.1's #{pane_tty}. This proves
# BOTH halves: the scoped command moves only the wrapper's own client, and
# -- informational only, never a gate -- that the OLD unscoped form really
# is capable of moving a bystander, evidence the -c fix is load-bearing.
tmux -L "$INNER" new-session -d -s A
tmux -L "$INNER" new-session -d -s B

# A second, independent outer wrapper attached to inner session A via the
# REAL orchard-shell (through $LAUNCH), so the wrapper's own inner client below is produced
# exactly the way production does it -- not synthesized.
OUTER_SOCKET="$OUTER2" "$LAUNCH" "$INNER" A </dev/null >"$SCRATCH/boot_switchcheck.log" 2>&1

SWITCH_INNER_TTY=""
# 5s, not 2s: this waits on send-keys inside a freshly-split pane 0.1 to
# reach a running shell, dispatch, and have the resulting `tmux attach`
# register as a client -- measured at ~3.1s under normal load, so a 2s
# ceiling here was a false-negative risk with zero margin. Matches the
# 5s (seq 1 50) poll every other "wait for a client to attach" check in
# this file already uses.
for _ in $(seq 1 50); do
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

# --- scrolling list, three sections, and the + launch modal ----------------
# The redesign round: a list long enough to scroll, grouped under three
# section headers, with fixed chrome above and below it, and a + button in
# the header that opens the launch modal in a tmux popup.
#
# Hermetic by construction: the binary is built with graphqlURL and wsURL
# pointed at closed ports, so the daemon on this machine (which is normally
# up, and whose rows would differ run to run) contributes nothing and every
# row comes from ORCHARD_SIDEBAR_FAKE. That also means no row here names a
# real tmux session, so nothing this check clicks or scrolls can attach the
# user's terminal to anything.
FK_OUTER="orchard-shell-test4"
FK_INNER="orchard-inner-test4"
FK_SESSION="shell"
FK_ROWS=30
FK_REPO_ROOT="$(cd -- "$SCRIPT_DIR/../.." >/dev/null 2>&1 && pwd)"

FK_CHECK1="fake rows render as one flat last-attached list"
FK_CHECK2="wheel scrolls the list under fixed chrome"
FK_CHECK3="clicking + opens the launch modal in a popup"
FK_CHECK4="esc closes the launch modal"
FK_CHECK5="real arrow keys move the selection"
FK_CHECK6="M-Down drives the sidebar from the inner pane"
FK_CHECK7="a click selects without moving the list"
FK_CHECK8="right-click opens the row menu; esc closes it"
FK_CHECK9="rename from the menu renames the inner session"
FK_CHECK10="the Needs-attention badge is in the header"
FK_CHECK11="/ filter narrows the cards; esc restores them"
FK_CHECK12="M-3 jumps the selection to the third card"

fk_fail_all() {
  record FAIL "$FK_CHECK1" "$1"
  record FAIL "$FK_CHECK2" "$1"
  record FAIL "$FK_CHECK3" "$1"
  record FAIL "$FK_CHECK4" "$1"
  record FAIL "$FK_CHECK5" "$1"
  record FAIL "$FK_CHECK6" "$1"
  record FAIL "$FK_CHECK7" "$1"
  record FAIL "$FK_CHECK8" "$1"
  record FAIL "$FK_CHECK9" "$1"
  record FAIL "$FK_CHECK10" "$1"
  record FAIL "$FK_CHECK11" "$1"
  record FAIL "$FK_CHECK12" "$1"
}

tmux -L "$FK_OUTER" kill-server 2>/dev/null
tmux -L "$FK_INNER" kill-server 2>/dev/null

FK_BIN="$SCRATCH/orchard-sidebar-fktest"
FK_BUILD_LOG="$SCRATCH/fk_build.log"
if ! (cd "$FK_REPO_ROOT" && go build -o "$FK_BIN" \
    -ldflags "-X main.graphqlURL=http://127.0.0.1:1/graphql -X main.wsURL=ws://127.0.0.1:1/graphql" \
    ./cmd/orchard-sidebar) >"$FK_BUILD_LOG" 2>&1; then
  fk_fail_all "go build (offline-daemon binary) failed; see $FK_BUILD_LOG"
else
  tmux -L "$FK_INNER" -f /dev/null new-session -d -s work -x 200 -y 50

  # One REAL row beside the synthetic ones, so the row menu has a session it
  # can actually rename (a synthetic row names no tmux session and its menu
  # actions decline by design). The sidebar derives real rows from the state
  # dir (hooks.go), pointed here at a scratch directory: left at its default
  # this check would read the USER's live session state, and a real state
  # file whose pane id happens to collide with one on $FK_INNER would put a
  # live session's card in a pane this check right-clicks and renames.
  FK_STATE_DIR="$SCRATCH/fkstate"
  mkdir -p "$FK_STATE_DIR"
  FK_INNER_PANE="$(tmux -L "$FK_INNER" display -p -t work '#{pane_id}' 2>/dev/null)"
  cat >"$FK_STATE_DIR/verify.json" <<JSON
{"sid":"verify-0001","cwd":"$SCRATCH","state":"input","pid":$$,
 "pane":"$FK_INNER_PANE","ts":"$(date -u +%Y-%m-%dT%H:%M:%SZ)",
 "first_prompt":"row menu rename target"}
JSON

  # Same shape as the real wrapper: a 40-column sidebar pane beside a wide
  # inner pane, in a window tall enough that 30 rows cannot possibly fit.
  tmux -L "$FK_OUTER" -f /dev/null new-session -d -s "$FK_SESSION" -x 130 -y 40 \
    "ORCHARD_SIDEBAR_FAKE=$FK_ROWS ORCHARD_TMUX_SOCKET=$FK_INNER CLAUDE_SESSION_STATE_DIR='$FK_STATE_DIR' $FK_BIN"
  tmux -L "$FK_OUTER" split-window -h -t "$FK_SESSION:0.0" -l 90 2>/dev/null
  tmux -L "$FK_OUTER" set -t "$FK_SESSION" -g mouse on 2>/dev/null

  FK_PANE="$FK_SESSION:0.0"
  FK_READY=""
  for _ in $(seq 1 60); do
    if tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null | grep -q 'fake-'; then
      FK_READY=1
      break
    fi
    sleep 0.2
  done

  if [[ -z "$FK_READY" ]]; then
    fk_fail_all "sidebar never rendered a fake row; capture: $(tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null | head -3 | tr '\n' '|')"
  else
    FK_CAP0="$(tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null)"

    # 1. one flat list ordered by attach recency. $FK_ROWS four-line cards are
    #    far taller than the pane, which is the point of the exercise, so the
    #    list is collected across a full scroll rather than from the one
    #    screenful that happens to be on top.
    FK_SEEN="$FK_CAP0"
    for _ in $(seq 1 12); do
      for _ in 1 2 3 4 5 6; do
        tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<65;10;10M')" 2>/dev/null
      done
      sleep 0.3
      FK_SEEN="$FK_SEEN
$(tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null)"
    done
    # the section headers are gone by request: the list orders by last_attached,
    # not by activity bucket, so there is nothing to head. The claim is that
    # many synthetic rows render AND none of the old headers appear anywhere.
    FK_STRAY=""
    for label in "Needs attention" "Done" "Sessions"; do
      printf '%s\n' "$FK_SEEN" | grep -qF "$label" && FK_STRAY="$FK_STRAY $label"
    done
    FK_NAMES="$(printf '%s\n' "$FK_SEEN" | grep -cE 'fake-[0-9]')"
    if [[ -n "$FK_STRAY" ]]; then
      record FAIL "$FK_CHECK1" "a removed section header still renders:$FK_STRAY"
    elif [[ "$FK_NAMES" -lt 3 ]]; then
      record FAIL "$FK_CHECK1" "expected many fake rows, saw $FK_NAMES card lines"
    else
      record PASS "$FK_CHECK1" "$FK_ROWS synthetic rows rendered flat, no section headers"
    fi

    # back to the top of the list so the scroll check below starts from a
    # known position (and can tell "did not move" from "already at the end")
    for _ in $(seq 1 80); do
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<64;10;10M')" 2>/dev/null
    done
    sleep 0.5
    FK_CAP0="$(tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null)"

    # 2. wheel-down at the pane (SGR button 65) must move the list and leave
    #    the header and footer exactly where they were -- the whole point of
    #    the three-band layout is that only the middle band moves.
    FK_HEAD0="$(printf '%s\n' "$FK_CAP0" | sed -n '1p')"
    FK_FOOT0="$(printf '%s\n' "$FK_CAP0" | grep -c 'M-1-9')"
    FK_BODY0="$(printf '%s\n' "$FK_CAP0" | sed -n '2,12p')"
    for _ in 1 2 3; do
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<65;10;10M')" 2>/dev/null
      sleep 0.15
    done
    sleep 0.5
    FK_CAP1="$(tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null)"
    FK_HEAD1="$(printf '%s\n' "$FK_CAP1" | sed -n '1p')"
    FK_BODY1="$(printf '%s\n' "$FK_CAP1" | sed -n '2,12p')"
    FK_FOOT1="$(printf '%s\n' "$FK_CAP1" | grep -c 'M-1-9')"
    if [[ "$FK_BODY0" == "$FK_BODY1" ]]; then
      record FAIL "$FK_CHECK2" "the list did not move after 3 wheel-down notches"
    elif [[ "$FK_HEAD0" != "$FK_HEAD1" ]]; then
      record FAIL "$FK_CHECK2" "the header scrolled with the list: '$FK_HEAD0' -> '$FK_HEAD1'"
    elif [[ "$FK_FOOT1" != "$FK_FOOT0" || "$FK_FOOT1" == "0" ]]; then
      record FAIL "$FK_CHECK2" "the footer hint line did not stay pinned (before=$FK_FOOT0 after=$FK_FOOT1)"
    else
      record PASS "$FK_CHECK2" "list scrolled; header and footer stayed put"
    fi

    # 3+4. The + button. A popup composites only into an ATTACHED client's
    #      stream and appears in no pane's grid, in list-panes -a, or in any
    #      client format -- so the proof is the display-popup client process
    #      itself, which blocks for exactly as long as the popup is open.
    #      The pattern carries this run's own inner socket name (the popup
    #      inherits it via -e), so it cannot match the user's real sidebar.
    FK_POPUP_PATTERN="display-popup.*$FK_INNER"
    FK_WIDTH="$(tmux -L "$FK_OUTER" display -p -t "$FK_PANE" '#{pane_width}' 2>/dev/null)"
    # the + sits 6 cells in from the right edge (0-based), and SGR mouse
    # coordinates are 1-based -- clicking w-6 lands one cell to its left and
    # opens nothing, which is exactly how this check first failed
    FK_CLICK_COL=$((FK_WIDTH - 5))
    FK_ESC_B64="$(printf '\033' | base64 | tr -d '\n')"
    FK_OUT="$SCRATCH/fk_attach.out"
    : >"$FK_OUT"
    # attach a real client, and have it write Esc at t=6s -- after the click
    # below has had time to open the popup and be observed
    python3 "$PYHELPER" "$FK_OUTER" "$FK_SESSION" 130 40 "$SCRATCH/fk_attach.log" "$FK_ESC_B64" 6 >"$FK_OUT" 2>&1 &
    PYWRAP_PID=$!
    for _ in $(seq 1 50); do
      grep -q '^PTY_PID=' "$FK_OUT" 2>/dev/null && break
      sleep 0.1
    done
    if ! grep -q '^PTY_PID=' "$FK_OUT" 2>/dev/null; then
      record FAIL "$FK_CHECK3" "real pty client never attached (see $FK_OUT)"
      record FAIL "$FK_CHECK4" "skipped: no attached client"
    else
      CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$FK_OUT" | head -1 | cut -d= -f2)"
      sleep 1.5   # attach handshake, well before the client writes Esc
      # a real SGR left press+release on the + cell, injected into the
      # sidebar pane the same way the row-click check above does it
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<0;%d;1M' "$FK_CLICK_COL")" 2>/dev/null
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<0;%d;1m' "$FK_CLICK_COL")" 2>/dev/null
      FK_OPEN=""
      for _ in $(seq 1 25); do
        if pgrep -f "$FK_POPUP_PATTERN" >/dev/null 2>&1; then
          FK_OPEN=1
          break
        fi
        sleep 0.1
      done
      if [[ -n "$FK_OPEN" ]]; then
        record PASS "$FK_CHECK3" "click at column $FK_CLICK_COL opened a display-popup on $FK_INNER"
      else
        record FAIL "$FK_CHECK3" "no display-popup client after clicking column $FK_CLICK_COL of $FK_WIDTH"
      fi

      # the client writes Esc at t=6s; the modal quits, the popup closes,
      # and the blocking display-popup client exits with it
      FK_CLOSED=""
      for _ in $(seq 1 80); do
        if ! pgrep -f "$FK_POPUP_PATTERN" >/dev/null 2>&1; then
          FK_CLOSED=1
          break
        fi
        sleep 0.2
      done
      if [[ -z "$FK_OPEN" ]]; then
        record FAIL "$FK_CHECK4" "skipped: the modal never opened"
      elif [[ -n "$FK_CLOSED" ]]; then
        record PASS "$FK_CHECK4" "esc at the attached client closed the popup"
      else
        record FAIL "$FK_CHECK4" "popup still open after esc (see $SCRATCH/fk_attach.log)"
      fi

      kill "$CHILD_ATTACH_PID" 2>/dev/null
      kill "$PYWRAP_PID" 2>/dev/null
      wait "$PYWRAP_PID" 2>/dev/null
      CHILD_ATTACH_PID=""
      PYWRAP_PID=""
    fi
    pkill -f "$FK_POPUP_PATTERN" 2>/dev/null
    FK_POPUP_PATTERN=""

    # 5. Arrow keys, driven as real escape sequences through an attached
    #    pty -- NOT via send-keys, which hands tmux a pre-parsed key name and
    #    would skip the whole terminal->tmux->app path this check exists to
    #    exercise. The selection rail (▌) is the assertion: it is drawn by
    #    the app from its own cursor, so if it moved, the key arrived, was
    #    parsed, moved the cursor, and the viewport followed.
    fk_rail() { tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null | grep -n '^▌' | head -1 | cut -d: -f1; }
    tmux -L "$FK_OUTER" select-pane -t "$FK_SESSION:0.0" 2>/dev/null
    # the wheel checks above left the viewport parked away from the cursor
    # (scrolling deliberately does not move the selection), so the rail is off
    # screen and there would be no baseline to compare against. Wheel back to
    # the top, where the still-unmoved cursor's card is.
    for _ in $(seq 1 60); do
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<64;10;10M')" 2>/dev/null
    done
    for _ in $(seq 1 20); do
      [[ -n "$(fk_rail)" ]] && break
      sleep 0.1
    done
    FK_DOWN_B64="$(printf '\033[B\033[B\033[B' | base64 | tr -d '\n')"
    FK_OUT5="$SCRATCH/fk_arrows.out"
    : >"$FK_OUT5"
    python3 "$PYHELPER" "$FK_OUTER" "$FK_SESSION" 130 40 "$SCRATCH/fk_arrows.log" "$FK_DOWN_B64" 3 >"$FK_OUT5" 2>&1 &
    PYWRAP_PID=$!
    for _ in $(seq 1 50); do
      grep -q '^PTY_PID=' "$FK_OUT5" 2>/dev/null && break
      sleep 0.1
    done
    if ! grep -q '^PTY_PID=' "$FK_OUT5" 2>/dev/null; then
      record FAIL "$FK_CHECK5" "real pty client never attached (see $FK_OUT5)"
      record FAIL "$FK_CHECK6" "skipped: no attached client"
    else
      CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$FK_OUT5" | head -1 | cut -d= -f2)"
      sleep 2            # attached, and the client has not written yet
      FK_RAIL0="$(fk_rail)"
      FK_HEADA="$(tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null | sed -n '1p')"
      sleep 2.5          # the client writes the three Downs at t=3s
      FK_RAIL1="$(fk_rail)"
      FK_HEADB="$(tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null | sed -n '1p')"
      if [[ -z "$FK_RAIL0" || -z "$FK_RAIL1" ]]; then
        record FAIL "$FK_CHECK5" "no selection rail on screen (before='$FK_RAIL0' after='$FK_RAIL1')"
      elif [[ "$FK_RAIL1" -le "$FK_RAIL0" ]]; then
        record FAIL "$FK_CHECK5" "3 Down keys did not move the rail (line $FK_RAIL0 -> $FK_RAIL1)"
      elif [[ "$FK_HEADA" != "$FK_HEADB" ]]; then
        record FAIL "$FK_CHECK5" "the header moved with the selection: '$FK_HEADA' -> '$FK_HEADB'"
      else
        record PASS "$FK_CHECK5" "Down x3 moved the rail from line $FK_RAIL0 to $FK_RAIL1"
      fi
      kill "$CHILD_ATTACH_PID" 2>/dev/null
      kill "$PYWRAP_PID" 2>/dev/null
      wait "$PYWRAP_PID" 2>/dev/null

      # 6. The same navigation from where the user actually is. A row click
      #    hands focus back to the inner pane, so bare arrows go to the inner
      #    client from then on; M-Up/M-Down are outer.conf bindings that
      #    forward a plain arrow to pane 0.0 regardless of focus. The CSI
      #    modifier form is used deliberately: three ESC-prefixed alt keys in
      #    one write have an ambiguous boundary and tmux drops one.
      tmux -L "$FK_OUTER" source-file "$(render_conf "$FK_OUTER" "$FK_INNER")" 2>/dev/null
      tmux -L "$FK_OUTER" select-pane -t "$FK_SESSION:0.1" 2>/dev/null
      FK_ALT_B64="$(printf '\033[1;3B\033[1;3B' | base64 | tr -d '\n')"
      FK_OUT6="$SCRATCH/fk_altarrows.out"
      : >"$FK_OUT6"
      python3 "$PYHELPER" "$FK_OUTER" "$FK_SESSION" 130 40 "$SCRATCH/fk_altarrows.log" "$FK_ALT_B64" 3 >"$FK_OUT6" 2>&1 &
      PYWRAP_PID=$!
      for _ in $(seq 1 50); do
        grep -q '^PTY_PID=' "$FK_OUT6" 2>/dev/null && break
        sleep 0.1
      done
      if ! grep -q '^PTY_PID=' "$FK_OUT6" 2>/dev/null; then
        record FAIL "$FK_CHECK6" "real pty client never attached (see $FK_OUT6)"
      else
        CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$FK_OUT6" | head -1 | cut -d= -f2)"
        sleep 2
        FK_RAIL2="$(fk_rail)"
        sleep 2.5        # M-Down x2 at t=3s
        FK_RAIL3="$(fk_rail)"
        FK_ACTIVE="$(tmux -L "$FK_OUTER" display -p -t "$FK_SESSION" '#{pane_index}' 2>/dev/null)"
        if [[ -z "$FK_RAIL2" || -z "$FK_RAIL3" ]]; then
          record FAIL "$FK_CHECK6" "no selection rail on screen (before='$FK_RAIL2' after='$FK_RAIL3')"
        elif [[ "$FK_RAIL3" -le "$FK_RAIL2" ]]; then
          record FAIL "$FK_CHECK6" "M-Down x2 from pane 0.1 did not move the rail (line $FK_RAIL2 -> $FK_RAIL3)"
        elif [[ "$FK_ACTIVE" != "1" ]]; then
          record FAIL "$FK_CHECK6" "M-Down stole focus: active pane is now 0.$FK_ACTIVE, want 0.1"
        else
          record PASS "$FK_CHECK6" "rail $FK_RAIL2 -> $FK_RAIL3 while focus stayed on 0.1"
        fi
        kill "$CHILD_ATTACH_PID" 2>/dev/null
        kill "$PYWRAP_PID" 2>/dev/null
        wait "$PYWRAP_PID" 2>/dev/null
      fi
      CHILD_ATTACH_PID=""
      PYWRAP_PID=""
    fi

    # --- 7. a click selects, and moves nothing else -------------------------
    # The live complaint: clicking a card threw the scroll position away. The
    # rule now is that the viewport moves only for a selection that is OFF
    # screen, and a click can only land on a card that is already drawn.
    #
    # The assertion is over the LIST BAND only, with the cells that legitimately
    # change neutralised: the footer's git box is a projection OF the selection
    # and is supposed to follow it, the working spinner turns four times a
    # second, and the age column rounds to the minute underneath the check.
    # What is left -- which cards, in which order, at which lines -- must be
    # byte-identical, which is also what lets bubbletea diff the frame instead
    # of repainting the pane.
    fk_band() {
      python3 -c '
import re, sys
out = []
for ln in sys.stdin.buffer.read().decode("utf-8", "replace").split("\n"):
    if ln.startswith("─" * 10):
        break
    for ch in "▌◐◓◑◒¹²³⁴⁵⁶⁷⁸⁹":
        ln = ln.replace(ch, " ")
    out.append(re.sub(r"(\d+[mhd]|now)\s*$", "", ln.rstrip()).rstrip())
sys.stdout.buffer.write(("\n".join(out) + "\n").encode("utf-8"))
'
    }
    fk_cap() { tmux -L "$FK_OUTER" capture-pane -p -t "$FK_PANE" 2>/dev/null; }

    # twelve notches: far enough that the SELECTED card (checks 5/6 left it
    # near the top) is off screen, so the click lands on a card that is not
    # the selection and the snap rule is actually being asked a question
    for _ in $(seq 1 12); do
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<65;10;10M')" 2>/dev/null
      sleep 0.1
    done
    sleep 0.8
    FK_CAPC0="$(fk_cap)"
    # a card's NAME line (the dir line also carries "fake-", and clicking it
    # would put the rail two lines above the row this check computed)
    FK_CLICK_ROW="$(printf '%s\n' "$FK_CAPC0" | grep -n 'fake-' | grep -v '📁' | sed -n '3p' | cut -d: -f1)"
    if [[ -z "$FK_CLICK_ROW" ]]; then
      record FAIL "$FK_CHECK7" "no card name line to click: $(printf '%s\n' "$FK_CAPC0" | head -3 | tr '\n' '|')"
    else
      # a real SGR left press+release on that card, raw bytes into the pane
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<0;6;%dM' "$FK_CLICK_ROW")" 2>/dev/null
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<0;6;%dm' "$FK_CLICK_ROW")" 2>/dev/null
      sleep 0.9
      FK_CAPC1="$(fk_cap)"
      sleep 4.5   # two full 2s poll cycles, each of which re-sorts the rows
      FK_CAPC2="$(fk_cap)"
      FK_BAND0="$(printf '%s\n' "$FK_CAPC0" | fk_band)"
      FK_BAND1="$(printf '%s\n' "$FK_CAPC1" | fk_band)"
      FK_BAND2="$(printf '%s\n' "$FK_CAPC2" | fk_band)"
      FK_RAIL_AT="$(printf '%s\n' "$FK_CAPC1" | grep -n '^▌' | head -1 | cut -d: -f1)"
      if [[ "$FK_BAND0" != "$FK_BAND1" ]]; then
        record FAIL "$FK_CHECK7" "the click moved the list: $(diff <(printf '%s\n' "$FK_BAND0") <(printf '%s\n' "$FK_BAND1") | head -4 | tr '\n' '|')"
      elif [[ -z "$FK_RAIL_AT" ]]; then
        record FAIL "$FK_CHECK7" "the click drew no selection rail, so it selected nothing"
      elif [[ "$FK_RAIL_AT" != "$FK_CLICK_ROW" ]]; then
        record FAIL "$FK_CHECK7" "clicked line $FK_CLICK_ROW but the rail starts at line $FK_RAIL_AT"
      elif [[ "$FK_BAND1" != "$FK_BAND2" ]]; then
        record FAIL "$FK_CHECK7" "two poll cycles after the click moved the list: $(diff <(printf '%s\n' "$FK_BAND1") <(printf '%s\n' "$FK_BAND2") | head -4 | tr '\n' '|')"
      else
        record PASS "$FK_CHECK7" "click on line $FK_CLICK_ROW selected that card and the list held through 2 polls"
      fi
    fi

    # --- 8+9. the row menu --------------------------------------------------
    # Right-click a card for the two things you do TO a session. The rename
    # half is driven all the way through against the one REAL session on
    # $FK_INNER: "a menu appeared" is not the claim, "the session got renamed"
    # is. The synthetic rows cannot serve here -- they name no tmux session.
    #
    # Back to the top of the list first: the real session was just created, so
    # among the never-attached rows it has the newest created time and sorts
    # ahead of every (older) synthetic row.
    for _ in $(seq 1 80); do
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<64;10;10M')" 2>/dev/null
    done
    FK_WORK_ROW=""
    for _ in $(seq 1 30); do
      FK_WORK_ROW="$(fk_cap | grep -n ' work ' | grep -v '📁' | head -1 | cut -d: -f1)"
      [[ -n "$FK_WORK_ROW" ]] && break
      sleep 0.3
    done
    if [[ -z "$FK_WORK_ROW" ]]; then
      record FAIL "$FK_CHECK8" "the state-dir session never rendered a row: $(fk_cap | head -6 | tr '\n' '|')"
      record FAIL "$FK_CHECK9" "skipped: no real row to rename"
    else
      # SGR button 2 is the right button; M presses, m releases
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<2;6;%dM' "$FK_WORK_ROW")" 2>/dev/null
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<2;6;%dm' "$FK_WORK_ROW")" 2>/dev/null
      sleep 1.0
      FK_MENU_OPEN="$(fk_cap)"
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033')" 2>/dev/null
      sleep 1.0
      FK_MENU_SHUT="$(fk_cap)"
      if ! printf '%s\n' "$FK_MENU_OPEN" | grep -q 'Rename'; then
        record FAIL "$FK_CHECK8" "right-click on line $FK_WORK_ROW drew no menu: $(printf '%s\n' "$FK_MENU_OPEN" | sed -n "${FK_WORK_ROW},$((FK_WORK_ROW + 5))p" | tr '\n' '|')"
      elif ! printf '%s\n' "$FK_MENU_OPEN" | grep -q 'Close'; then
        record FAIL "$FK_CHECK8" "the menu is missing its Close item"
      elif printf '%s\n' "$FK_MENU_SHUT" | grep -q 'Rename'; then
        record FAIL "$FK_CHECK8" "esc left the menu on screen"
      else
        record PASS "$FK_CHECK8" "right-click on line $FK_WORK_ROW opened Rename/Close; esc closed it"
      fi

      # and now the whole gesture: right-click, Enter on Rename, ^U, type,
      # Enter to commit -- then ask the INNER server what its sessions are
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<2;6;%dM' "$FK_WORK_ROW")" 2>/dev/null
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<2;6;%dm' "$FK_WORK_ROW")" 2>/dev/null
      sleep 0.8
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\r')" 2>/dev/null      # Rename
      sleep 0.5
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\025')" 2>/dev/null    # ^U clears
      sleep 0.4
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l 'renamed-by-verify' 2>/dev/null
      sleep 0.5
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\r')" 2>/dev/null      # commit
      FK_RENAMED=""
      for _ in $(seq 1 30); do
        if tmux -L "$FK_INNER" list-sessions -F '#{session_name}' 2>/dev/null | grep -qx 'renamed-by-verify'; then
          FK_RENAMED=1
          break
        fi
        sleep 0.2
      done
      if [[ -n "$FK_RENAMED" ]]; then
        record PASS "$FK_CHECK9" "'work' is now 'renamed-by-verify' on $FK_INNER"
      else
        record FAIL "$FK_CHECK9" "sessions on $FK_INNER: $(tmux -L "$FK_INNER" list-sessions -F '#{session_name}' 2>/dev/null | tr '\n' ' ')"
      fi
    fi

    # --- 10. the Needs-attention badge --------------------------------------
    # The sidebar's stated single question is "what needs me?", and the badge
    # is its one-glance answer: the size of the bucket the list's first section
    # holds. Read off the header line only -- the same ● is the state dot on
    # every attention card, so a whole-pane grep would pass on the cards alone.
    for _ in $(seq 1 80); do
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<64;10;10M')" 2>/dev/null
    done
    sleep 0.6
    FK_HEAD_BADGE="$(fk_cap | sed -n '1p')"
    if printf '%s\n' "$FK_HEAD_BADGE" | grep -qE '●[0-9]+'; then
      record PASS "$FK_CHECK10" "header: $(printf '%s\n' "$FK_HEAD_BADGE" | tr -s ' ')"
    else
      record FAIL "$FK_CHECK10" "no ●N badge on the header line: '$FK_HEAD_BADGE'"
    fi

    # --- 11. the / filter ---------------------------------------------------
    # Real keystrokes into the pane's pty, the same path the rename check types
    # through: "/" opens the field in the header row, the query narrows the
    # cards live, and esc puts the whole list back.
    #
    # "payments" is one of twelve names cycled across $FK_ROWS synthetic rows,
    # so exactly three cards survive it -- a number the header itself prints,
    # which is what makes "(3)" an assertion rather than a coincidence.
    tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l '/' 2>/dev/null
    sleep 0.4
    tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l 'payments' 2>/dev/null
    sleep 0.9
    FK_FILTERED="$(fk_cap)"
    FK_FHEAD="$(printf '%s\n' "$FK_FILTERED" | sed -n '1p')"
    # card NAME lines only: the 📁 line carries the session name as its
    # directory, and the branch spells it "fake/NN-name"
    fk_names() { grep 'fake-' | grep -v '📁'; }
    FK_FMATCH="$(printf '%s\n' "$FK_FILTERED" | fk_names | wc -l | tr -d ' ')"
    FK_FOTHER="$(printf '%s\n' "$FK_FILTERED" | fk_names | grep -cv 'payments')"
    tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033')" 2>/dev/null
    sleep 0.9
    FK_RESTORED="$(fk_cap)"
    # "restored" is asked as "cards the filter had hidden are back on screen",
    # not "one named card is back": only a screenful of a 30-row list is ever
    # drawn, so naming a specific card asks whether it happens to be above the
    # fold -- which is a question about the viewport, not about the filter.
    FK_RBACK="$(printf '%s\n' "$FK_RESTORED" | fk_names | grep -cv 'payments')"
    if ! printf '%s\n' "$FK_FHEAD" | grep -q '/payments'; then
      record FAIL "$FK_CHECK11" "the header never showed the filter: '$FK_FHEAD'"
    elif ! printf '%s\n' "$FK_FHEAD" | grep -qF '(3)'; then
      record FAIL "$FK_CHECK11" "the header count is not (3): '$FK_FHEAD'"
    elif [[ "$FK_FMATCH" != "3" ]]; then
      record FAIL "$FK_CHECK11" "$FK_FMATCH cards on screen under the filter, want 3: $(printf '%s\n' "$FK_FILTERED" | fk_names | tr '\n' '|')"
    elif [[ "$FK_FOTHER" != "0" ]]; then
      record FAIL "$FK_CHECK11" "$FK_FOTHER non-matching card(s) still drawn: $(printf '%s\n' "$FK_FILTERED" | fk_names | grep -v 'payments' | tr '\n' '|')"
    elif [[ "$FK_RBACK" == "0" ]]; then
      record FAIL "$FK_CHECK11" "esc left the list filtered: $(printf '%s\n' "$FK_RESTORED" | fk_names | head -4 | tr '\n' '|')"
    else
      record PASS "$FK_CHECK11" "/payments left exactly 3 payments cards and the (3) count; esc brought $FK_RBACK other cards back"
    fi

    # --- 12. M-3 ------------------------------------------------------------
    # All the way through: a REAL terminal writes the chord, outer.conf's
    # `bind -n M-3` catches it at the outer layer and forwards it to pane 0.0,
    # and the sidebar selects the third VISIBLE card. The assertion is the
    # sidebar's own two readings of "third card" agreeing -- the ³ marker it
    # printed before the chord, and the selection rail after it.
    #
    # M-1 first, injected straight into the pane, so the rail is parked on card
    # one and card three is therefore still wearing its marker to compare to.
    for _ in $(seq 1 80); do
      tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\033[<64;10;10M')" 2>/dev/null
    done
    tmux -L "$FK_OUTER" send-keys -t "$FK_PANE" -l "$(printf '\0331')" 2>/dev/null
    sleep 0.9
    FK_ORD3="$(fk_cap | grep -n '^³' | head -1 | cut -d: -f1)"
    FK_M3_B64="$(printf '\0333' | base64 | tr -d '\n')"
    FK_OUT12="$SCRATCH/fk_jump.out"
    : >"$FK_OUT12"
    python3 "$PYHELPER" "$FK_OUTER" "$FK_SESSION" 130 40 "$SCRATCH/fk_jump.log" "$FK_M3_B64" 3 >"$FK_OUT12" 2>&1 &
    PYWRAP_PID=$!
    for _ in $(seq 1 50); do
      grep -q '^PTY_PID=' "$FK_OUT12" 2>/dev/null && break
      sleep 0.1
    done
    if [[ -z "$FK_ORD3" ]]; then
      record FAIL "$FK_CHECK12" "no ³ marker on screen to jump to: $(fk_cap | head -8 | tr '\n' '|')"
    elif ! grep -q '^PTY_PID=' "$FK_OUT12" 2>/dev/null; then
      record FAIL "$FK_CHECK12" "real pty client never attached (see $FK_OUT12)"
    else
      CHILD_ATTACH_PID="$(grep '^PTY_PID=' "$FK_OUT12" | head -1 | cut -d= -f2)"
      sleep 2            # attached; the client writes M-3 at t=3s
      FK_RAIL_M3=""
      for _ in $(seq 1 30); do
        FK_RAIL_M3="$(fk_rail)"
        [[ "$FK_RAIL_M3" == "$FK_ORD3" ]] && break
        sleep 0.2
      done
      FK_ACTIVE_M3="$(tmux -L "$FK_OUTER" display -p -t "$FK_SESSION" '#{pane_index}' 2>/dev/null)"
      if [[ "$FK_RAIL_M3" != "$FK_ORD3" ]]; then
        record FAIL "$FK_CHECK12" "M-3 put the rail on line ${FK_RAIL_M3:-<none>}, want line $FK_ORD3 where ³ was"
      elif [[ "$FK_ACTIVE_M3" != "1" ]]; then
        record FAIL "$FK_CHECK12" "M-3 left focus on pane 0.$FK_ACTIVE_M3, want it handed back to 0.1"
      else
        record PASS "$FK_CHECK12" "M-3 selected the ³ card (line $FK_ORD3) and handed focus back to 0.1"
      fi
      kill "$CHILD_ATTACH_PID" 2>/dev/null
    fi
    kill "$PYWRAP_PID" 2>/dev/null
    wait "$PYWRAP_PID" 2>/dev/null
    CHILD_ATTACH_PID=""
    PYWRAP_PID=""
  fi
fi

tmux -L "$FK_OUTER" kill-server 2>/dev/null
tmux -L "$FK_INNER" kill-server 2>/dev/null
FK_OUTER=""
FK_INNER=""

# --- the remembered layout is restored on startup ---------------------------
# The width the user dragged to (and whether they left the sidebar collapsed)
# is written to $XDG_STATE_HOME/orchard/sidebar-state.json and restored at
# startup: a sidebar that reopens at 40 columns every morning is a sidebar the
# user re-drags every morning.
#
# End to end, because every step of it is a place this has broken: the sidebar
# has to read the file, resize its own pane on the OUTER server, and publish
# main-pane-width so the wrapper's resize hooks re-pin to the restored width
# rather than to their default.
ST_CHECK1="remembered width is restored at startup"
ST_CHECK2="restored width is published to the outer server"
ST_OUTER="orchard-shell-test5"
ST_INNER="orchard-inner-test5"
if [[ ! -x "$FK_BIN" ]]; then
  record FAIL "$ST_CHECK1" "no sidebar binary was built; see $FK_BUILD_LOG"
  record FAIL "$ST_CHECK2" "no sidebar binary was built; see $FK_BUILD_LOG"
else
  tmux -L "$ST_OUTER" kill-server 2>/dev/null
  tmux -L "$ST_INNER" kill-server 2>/dev/null
  ST_STATE_HOME="$SCRATCH/ststate"
  mkdir -p "$ST_STATE_HOME/orchard"
  printf '{"width":52,"collapsed":false}' >"$ST_STATE_HOME/orchard/sidebar-state.json"

  tmux -L "$ST_INNER" -f /dev/null new-session -d -s work -x 200 -y 50
  # the real outer.conf, so the restore lands in front of the same resize
  # hooks the wrapper runs with
  tmux -L "$ST_OUTER" -f "$(render_conf "$ST_OUTER" "$ST_INNER")" new-session -d -s "$OUTER_SESSION" -x 130 -y 40 \
    "XDG_STATE_HOME='$ST_STATE_HOME' ORCHARD_TMUX_SOCKET=$ST_INNER CLAUDE_SESSION_STATE_DIR='$SCRATCH/nostate' $FK_BIN"
  tmux -L "$ST_OUTER" split-window -h -t "$OUTER_SESSION:0.0" -l 89 2>/dev/null

  ST_WIDTH=""
  for _ in $(seq 1 40); do
    ST_WIDTH="$(tmux -L "$ST_OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_width}' 2>/dev/null)"
    [[ "$ST_WIDTH" == "52" ]] && break
    sleep 0.25
  done
  if [[ "$ST_WIDTH" == "52" ]]; then
    record PASS "$ST_CHECK1" "sidebar-state.json width=52 restored; pane_width=$ST_WIDTH"
  else
    record FAIL "$ST_CHECK1" "pane_width=${ST_WIDTH:-<no pane>} after startup, want the remembered 52"
  fi

  ST_OPT="$(tmux -L "$ST_OUTER" show -wv -t "$OUTER_SESSION:0" main-pane-width 2>/dev/null)"
  if [[ "$ST_OPT" == "52" ]]; then
    record PASS "$ST_CHECK2" "main-pane-width=$ST_OPT on the outer server"
  else
    record FAIL "$ST_CHECK2" "main-pane-width=${ST_OPT:-<unset>}, want 52 — the resize hooks would re-pin to their default"
  fi

  tmux -L "$ST_OUTER" kill-server 2>/dev/null
  tmux -L "$ST_INNER" kill-server 2>/dev/null
fi

# --- reattach self-heal: a dead sidebar pane is respawned, not left broken -
# Issue #747 follow-up (Bug 1). Pane 0.0's own process (the interactive
# shell send-keys types the sidebar/placeholder command into) can die out
# from under a live wrapper -- a crash, an OOM kill, the user Ctrl-D'ing
# past a dead sidebar back to a bare prompt then exiting that too. Before
# remain-on-exit (outer.conf) that closed the pane outright and renumbered
# 0.1 down to 0.0, so a rerun's hardcoded "0.1" target missed the survivor
# and orchard-shell left a broken single-pane window (`select-pane -t
# shell:0.1: can't find pane: 1`, the exact live-drive failure this check
# guards against).
#
# Kills pane 0.0's own #{pane_pid} directly, not a grep'd child pid:
# send-keys types the sidebar command into an already-running interactive
# shell, so killing only that child would just return the pane to a fresh
# prompt with the shell (and so the pane) still alive. #{pane_pid} is the
# pane's own tracked process -- the one remain-on-exit/#{pane_dead}
# actually watches, and the one that has to die to reproduce the bug.
#
# Runs on its own throwaway sockets (never $OUTER/$INNER): this check
# kills a real process out from under a live wrapper session, which the
# checks above and below this block do not expect to happen to theirs.
SH_OUTER="orchard-shell-test6"
SH_INNER="orchard-inner-test6"
SH_SESSION="sh747"
SH_CHECK1="pane 0.0 goes dead, not closed, when its process is killed"
SH_CHECK2="rerun after a dead sidebar restores exactly 2 panes"
SH_CHECK3="0.0 is respawned as the sidebar after a dead-pane rerun"
SH_CHECK4="0.1 is still the inner attach after a dead-pane rerun"
SH_CHECK5="sidebar width is restored after a dead-pane rerun"

tmux -L "$SH_OUTER" kill-server 2>/dev/null
tmux -L "$SH_INNER" kill-server 2>/dev/null

tmux -L "$SH_INNER" -f /dev/null new-session -d -s "$SH_SESSION" -x 200 -y 50

SH_BOOT1_LOG="$SCRATCH/sh_boot1.log"
OUTER_SOCKET="$SH_OUTER" "$LAUNCH" "$SH_INNER" "$SH_SESSION" </dev/null >"$SH_BOOT1_LOG" 2>&1

if ! tmux -L "$SH_OUTER" has-session -t "$OUTER_SESSION" 2>/dev/null; then
  for c in "$SH_CHECK1" "$SH_CHECK2" "$SH_CHECK3" "$SH_CHECK4" "$SH_CHECK5"; do
    record FAIL "$c" "initial boot on socket '$SH_OUTER' never came up; see $SH_BOOT1_LOG"
  done
else
  SH_PID="$(tmux -L "$SH_OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_pid}' 2>/dev/null)"
  if [[ -z "$SH_PID" ]]; then
    for c in "$SH_CHECK1" "$SH_CHECK2" "$SH_CHECK3" "$SH_CHECK4" "$SH_CHECK5"; do
      record FAIL "$c" "could not resolve #{pane_pid} for 0.0"
    done
  else
    kill -9 "$SH_PID" 2>/dev/null

    SH_DEAD=""
    for _ in $(seq 1 30); do
      SH_DEAD="$(tmux -L "$SH_OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_dead}' 2>/dev/null)"
      [[ "$SH_DEAD" == "1" ]] && break
      sleep 0.1
    done
    if [[ "$SH_DEAD" == "1" ]]; then
      record PASS "$SH_CHECK1" "remain-on-exit kept 0.0 addressable as a dead pane after kill -9 $SH_PID"
    else
      record FAIL "$SH_CHECK1" "pane_dead=${SH_DEAD:-<no pane>} after kill -9 $SH_PID -- remain-on-exit did not keep the pane"
    fi

    SH_BOOT2_LOG="$SCRATCH/sh_boot2.log"
    OUTER_SOCKET="$SH_OUTER" "$LAUNCH" "$SH_INNER" "$SH_SESSION" </dev/null >"$SH_BOOT2_LOG" 2>&1

    SH_PANECOUNT="$(tmux -L "$SH_OUTER" list-panes -t "$OUTER_SESSION:0" 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "$SH_PANECOUNT" == "2" ]]; then
      record PASS "$SH_CHECK2" "rerun restored exactly 2 panes"
    else
      record FAIL "$SH_CHECK2" "found ${SH_PANECOUNT:-<none>} panes after rerun, want 2; see $SH_BOOT2_LOG"
    fi

    SH_CMD00=""
    for _ in $(seq 1 50); do
      SH_CMD00="$(tmux -L "$SH_OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_current_command}' 2>/dev/null)"
      [[ "$SH_CMD00" == "watch" || "$SH_CMD00" == "orchard-sidebar" ]] && break
      sleep 0.1
    done
    if [[ "$SH_CMD00" == "watch" || "$SH_CMD00" == "orchard-sidebar" ]]; then
      record PASS "$SH_CHECK3" "0.0=$SH_CMD00"
    else
      record FAIL "$SH_CHECK3" "0.0=${SH_CMD00:-<none>}, want watch or orchard-sidebar"
    fi

    SH_CMD01="$(tmux -L "$SH_OUTER" display -p -t "$OUTER_SESSION:0.1" '#{pane_current_command}' 2>/dev/null)"
    if [[ "$SH_CMD01" == "tmux" ]]; then
      record PASS "$SH_CHECK4" "0.1=$SH_CMD01"
    else
      record FAIL "$SH_CHECK4" "0.1=${SH_CMD01:-<none>}, want tmux"
    fi

    SH_WIDTH="$(tmux -L "$SH_OUTER" display -p -t "$OUTER_SESSION:0.0" '#{pane_width}' 2>/dev/null)"
    if [[ "$SH_WIDTH" == "40" ]]; then
      record PASS "$SH_CHECK5" "expected=40 actual=$SH_WIDTH"
    else
      record FAIL "$SH_CHECK5" "expected=40 actual=${SH_WIDTH:-<no pane>}"
    fi
  fi
fi

tmux -L "$SH_OUTER" kill-server 2>/dev/null
tmux -L "$SH_INNER" kill-server 2>/dev/null

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
