#!/usr/bin/env bash
# pane-labels.sh — derive a rich label per tmux PANE from orchard daemon
# state plus Claude hook state, and write it to @orchard_pane_label so
# choose-tree (expanded) renders it.
#
# Not invoked directly by users: `orchard.tmux` (the tmux-plugin entry
# script in this directory) binds it to `prefix + s` and passes the
# resolved daemon URL. See scripts/tmux/README.md.
#
# Usage:
#   pane-labels.sh [--daemon-url URL] [--heartbeat-dir DIR]
#                  [--panes-file FILE] [--print]
#
#   --daemon-url    The daemon's GraphQL endpoint. Either shape works — a
#                   base URL ("http://host:7777") has /graphql appended, a
#                   full endpoint is used as given. Defaults to
#                   $ORCHARD_DAEMON_URL, which accepts the same two shapes,
#                   then http://127.0.0.1:7777/graphql.
#   --heartbeat-dir Directory holding orchard-claude-<session>.json hook
#                   state files. Defaults to $ORCHARD_HEARTBEAT_DIR, then
#                   $TMPDIR, then /tmp — mirroring claudeinstance.ResolveDir()
#                   in internal/server/providers/claudeinstance/types.go.
#   --panes-file    Read the pane table from FILE instead of `tmux list-panes`.
#   --print         Emit "<pane_id>\t<label>" on stdout instead of setting
#                   the tmux option. Implies no tmux writes.
#
# Daemon contract: queries `repos { slug worktrees { ... } }` per the v0.8
# schema (ADR-015 rename project→repo). Falls back to empty results when
# the daemon is unreachable so the picker still works.
#
# Claude enrichment: per ADR-007, Claude state is read from hook state
# files, not from tmux. The hook (`orchard-state.sh`) writes exactly
# {state, session_id, tmux_session, cwd, event, timestamp}. Only fields
# actually present in a file are rendered — nothing is placeholdered.
set -euo pipefail

DAEMON="${ORCHARD_DAEMON_URL:-http://127.0.0.1:7777}"
DAEMON_URL=""
HEARTBEAT_DIR=""
PANES_FILE=""
PRINT_MODE=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --daemon-url)    DAEMON_URL="$2"; shift 2 ;;
    --heartbeat-dir) HEARTBEAT_DIR="$2"; shift 2 ;;
    --panes-file)    PANES_FILE="$2"; shift 2 ;;
    --print)         PRINT_MODE=1; shift ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

# One concept, one meaning. `--daemon-url` and `@orchard_daemon_url` are
# documented as FULL endpoints while `$ORCHARD_DAEMON_URL` was historically a
# BASE url with /graphql appended, so mirroring the documented option value
# into the env var produced `.../graphql/graphql` and silently empty results.
# Both spellings now accept either shape.
normalize_daemon_url() {
  local url="$1"
  url="${url%/}"
  case "$url" in
    */graphql) printf '%s' "$url" ;;
    *)         printf '%s/graphql' "$url" ;;
  esac
}

[[ -z "$DAEMON_URL" ]] && DAEMON_URL="$DAEMON"
DAEMON_URL="$(normalize_daemon_url "$DAEMON_URL")"

run_once() {
  local qfile panes
  qfile=$(mktemp)
  panes=$(mktemp)
  # Expansion at trap-definition time is deliberate: the paths are fixed for
  # this call and must survive any later reassignment of the variables.
  # shellcheck disable=SC2064
  trap "rm -f '$qfile' '$panes'" RETURN

  # `claudeInstances` and `tmuxSessions` were selected here and never read.
  # Claude state deliberately comes from the hook sidecars instead (ADR-007):
  # they are local files, so state still renders when the daemon is down —
  # the degradation the README promises and the bats suite pins. Selecting a
  # second copy of the same data and dropping it is the "same data in two
  # shapes" smell of ADR-022, so the selection goes rather than the sidecars.
  #
  # The query is intentionally lean (path+branch only) so the picker boot
  # stays under ~1s even with 30+ worktrees across many repos. Enriching
  # each worktree with PR/issue/labels hits the gh provider per-worktree
  # and can take 30s+ under cold cache. Set ORCHARD_LABEL_ENRICH=1 to
  # opt-in to the heavy query (useful when running outside the prefix-s
  # hot path).
  local query
  if [ "${ORCHARD_LABEL_ENRICH:-0}" = "1" ]; then
    query='{"query":"{ repos { slug worktrees { branch path host pr { number draft mergeStateStatus statusCheckRollup labels { name } reviewDecision } issue { number title } } } }"}'
  else
    query='{"query":"{ repos { slug worktrees { branch path host } } }"}'
  fi
  if ! curl -sf --max-time 15 -X POST "${DAEMON_URL}" \
      -H 'Content-Type: application/json' \
      -d "$query" \
      > "$qfile" 2>/dev/null; then
    printf '{"data":{"repos":[]}}' > "$qfile"
  fi

  local TAB
  TAB=$(printf '\t')
  # Per-pane: target id, session_name, window_index, pane_index, current_path, current_command.
  if [[ -n "$PANES_FILE" ]]; then
    cat "$PANES_FILE" > "$panes"
  else
    tmux list-panes -aF "#{pane_id}${TAB}#{session_name}${TAB}#{window_index}${TAB}#{pane_index}${TAB}#{pane_current_path}${TAB}#{pane_current_command}" > "$panes" 2>/dev/null || return 1
  fi

  ORCHARD_PRINT_MODE=$PRINT_MODE \
  ORCHARD_HEARTBEAT_DIR_ARG="$HEARTBEAT_DIR" \
  python3 - "$qfile" "$panes" <<'PY'
import json, subprocess, sys, datetime, os, glob, stat

PRINT_MODE = os.environ.get("ORCHARD_PRINT_MODE", "0") == "1"

# `curl -sf` only rejects non-2xx, so a proxy error page, a truncated body or
# a JSON document of the wrong shape all arrive here as a "successful"
# response. Fall back to the same empty result set the unreachable-daemon
# branch installs, rather than raising and labelling nothing at all.
try:
    with open(sys.argv[1]) as f:
        resp = json.load(f)
except (OSError, ValueError):
    print("orchard-tmux-labels: daemon response was not valid JSON; "
          "labelling from local state only", file=sys.stderr)
    resp = {}
if not isinstance(resp, dict):
    print("orchard-tmux-labels: daemon response was not a JSON object; "
          "labelling from local state only", file=sys.stderr)
    resp = {}

data = resp.get("data")
if not isinstance(data, dict):
    data = {}
# v0.8 schema renamed `projects` → `repos` (ADR-015). Each repo has `slug`
# (was `name`) and the same nested worktree shape.
repos = data.get("repos") or []

# (worktree_path, worktree_data, repo_slug) sorted longest-prefix first.
# Entries of the wrong shape are skipped, not fatal — see the JSON guard above.
all_worktrees = []
for r in repos if isinstance(repos, list) else []:
    if not isinstance(r, dict):
        continue
    for wt in (r.get("worktrees") or []):
        if not isinstance(wt, dict) or not isinstance(wt.get("path"), str):
            continue
        all_worktrees.append((wt["path"], wt, r.get("slug")))
all_worktrees.sort(key=lambda x: -len(x[0]))

now = datetime.datetime.now(datetime.timezone.utc)


# --- tmux format-string safety ---------------------------------------------
# choose-tree renders this label through `#{E:@orchard_pane_label}`, which
# expands the option's value as a tmux format — and `#(cmd)` in a format runs
# `cmd`. Branch names, issue titles, PR labels, hook-state fields and pane
# paths are all attacker-influenceable, so none may reach the option with a
# live `#`. tmux's only escape is doubling it. Verified on tmux 3.6a: a raw
# `#(touch F)` planted in the option creates F on the first real render, while
# the doubled form renders as inert literal text.

def fmt_escape(value):
    """Neutralise every tmux format directive in untrusted text."""
    return str(value).replace("#", "##")


def path_under(parent, child):
    """True when `child` is `parent` itself or lives beneath it.

    The boundary is a path separator, not a string prefix, so
    `/a/b` never claims `/a/bc`. `/` (and any trailing-slash form of it) is
    the parent of every absolute path: naive `parent + "/"` concatenation
    would build `//`, which no absolute path starts with.
    """
    if not parent or not child:
        return False
    parent = parent.rstrip("/")
    if not parent:
        return child.startswith("/")
    return child == parent or child.startswith(parent + "/")


def cell(style, text):
    """Build one label cell.

    `style` is ours and stays live; `text` is untrusted and is escaped. Cells
    are only ever built here, so no caller can leak an unescaped value into
    the option by hand.
    """
    return f"#[{style}]{fmt_escape(text)}#[default]"


# --- Claude hook state (ADR-007) -------------------------------------------
# The hook writes one sidecar per tmux session, named
# orchard-claude-<tmux_session>.json. Directory resolution mirrors
# claudeinstance.ResolveDir(): $ORCHARD_HEARTBEAT_DIR, then $TMPDIR, then /tmp.

def heartbeat_dir():
    arg = os.environ.get("ORCHARD_HEARTBEAT_DIR_ARG") or ""
    if arg:
        return arg
    for var in ("ORCHARD_HEARTBEAT_DIR", "TMPDIR"):
        v = os.environ.get(var)
        if v:
            return v
    return "/tmp"


def _warn(msg):
    print(f"orchard-tmux-labels: {msg}", file=sys.stderr)


def trusted_dir(dirpath):
    """True when a heartbeat dir is safe to read sidecars from.

    The default resolution ends at /tmp, which is world-writable, so any local
    unprivileged process can drop a sidecar naming a session it does not own.
    World-writable is only acceptable with the sticky bit — the property that
    stops one user deleting or replacing another's files in a shared /tmp.
    """
    try:
        st = os.stat(dirpath)
    except OSError:
        return False
    if st.st_uid not in (0, os.geteuid()):
        return False
    if (st.st_mode & stat.S_IWOTH) and not (st.st_mode & stat.S_ISVTX):
        return False
    return True


def trusted_file(path):
    """True when a sidecar's provenance holds up.

    Owner is the check that closes the attack: a sidecar planted by another
    local user in a shared /tmp is rejected however well-formed it looks. The
    rest is defence in depth — a symlink could redirect the read outside the
    dir, and a world-writable file can be rewritten after it was created.
    """
    try:
        st = os.lstat(path)
    except OSError:
        return False
    if not stat.S_ISREG(st.st_mode):
        return False
    if st.st_uid != os.geteuid():
        return False
    if st.st_mode & stat.S_IWOTH:
        return False
    return True


def load_hook_states(dirpath):
    """Map tmux session name -> parsed hook state dict.

    Unreadable, non-JSON, non-object and untrusted files are skipped so
    neither a partially written sidecar nor a planted one breaks labelling.
    `.inflight.json` companions are not state files and are ignored.
    """
    if not trusted_dir(dirpath):
        _warn(f"heartbeat dir is not trustworthy, ignoring hook state: {dirpath}")
        return {}
    states = {}
    for path in sorted(glob.glob(os.path.join(dirpath, "orchard-claude-*.json"))):
        name = os.path.basename(path)
        if name.endswith(".inflight.json"):
            continue
        if not trusted_file(path):
            _warn(f"ignoring hook state file with untrusted provenance: {path}")
            continue
        try:
            with open(path) as fh:
                st = json.load(fh)
        except (OSError, ValueError):
            continue
        if not isinstance(st, dict):
            continue
        session = st.get("tmux_session")
        if not session:
            session = name[len("orchard-claude-"):-len(".json")]
        states[session] = st
    return states


HOOK_STATES = load_hook_states(heartbeat_dir())

# Glyph + colour per hook `state` value written by orchard-state.sh:
# working (PreToolUse/PostToolUse), idle (Stop/SessionStart), input
# (AskUserQuestion, permission_prompt, elicitation_dialog, idle_prompt).
CLAUDE_STATE_STYLE = {
    "working": ("⏺", "green"),
    "idle":    ("⏸", "brightblack"),
    "input":   ("⌨", "yellow"),
}


def hook_state_for_pane(session, pane_path, is_claude_pane):
    """Pick the sidecar belonging to this pane, or None.

    A sidecar is per-SESSION but a label is per-PANE, so a session-name hit
    alone would stamp Claude state onto every shell and editor in the
    session. Narrow it to the pane whose cwd the hook recorded, or failing
    that to the pane actually running claude.
    """
    st = HOOK_STATES.get(session)
    if not st:
        return None
    if path_under(st.get("cwd") or "", pane_path):
        return st
    if is_claude_pane:
        return st
    return None


def claude_cells(st):
    """Render the hook state as label cells, omitting every absent field."""
    if not st:
        return []
    cells = []
    # Only the values orchard-state.sh actually writes are rendered. Anything
    # else is a sidecar this script does not understand, so it is dropped
    # rather than passed through as free text.
    state = (st.get("state") or "").strip() if isinstance(st.get("state"), str) else ""
    if state in CLAUDE_STATE_STYLE:
        glyph, color = CLAUDE_STATE_STYLE[state]
        cells.append(cell(f"fg={color}", f"{glyph} {state}"))
    elif state:
        _warn(f"ignoring unknown claude state: {state[:40]!r}")
    # `model` and `context_window_pct` are statusline telemetry, not part of
    # the hook payload (ClaudeSessionInfo::from_state_file in
    # crates/orchard/src/session.rs leaves both None). Rendered only when a
    # writer actually supplies them; never placeholdered.
    model = st.get("model")
    if isinstance(model, str) and model.strip():
        cells.append(cell("fg=brightblack", model.strip()[:40]))
    ctx = st.get("context_window_pct")
    if isinstance(ctx, (int, float)) and not isinstance(ctx, bool):
        cells.append(cell("fg=brightblack", f"{ctx:.0f}%"))
    return cells


def status_glyph(pr):
    if not pr: return ("", "default")
    if pr.get("statusCheckRollup") == "FAILURE":            return ("🚫", "red")
    if pr.get("reviewDecision") == "CHANGES_REQUESTED":     return ("🔴", "red")
    if pr.get("mergeStateStatus") in ("DIRTY","BLOCKED"):   return ("⚠", "yellow")
    if pr.get("draft"):                                     return ("📝", "default")
    if pr.get("statusCheckRollup") == "PENDING":            return ("⬆", "blue")
    if (pr.get("reviewDecision") == "APPROVED" and
        pr.get("statusCheckRollup") == "SUCCESS" and
        pr.get("mergeStateStatus") == "CLEAN"):             return ("🟢", "green")
    return ("⬆", "blue")

def head_branch(path):
    try:
        r = subprocess.run(["git","-C",path,"branch","--show-current"],
                           capture_output=True, text=True, timeout=2)
        if r.returncode == 0:
            return (r.stdout or "").strip() or None
    except Exception:
        pass
    return None

def pick_worktree(pane_path):
    """Longest-prefix match, then disambiguate by HEAD branch when path matches multiple."""
    matches = []
    seen = set()
    for wt_path, wt, repo in all_worktrees:
        if wt_path in seen: continue
        if path_under(wt_path, pane_path):
            matches.append((wt_path, wt, repo))
            seen.add(wt_path)
    if not matches:
        return (None, None)
    if len(matches) == 1:
        _, wt, repo = matches[0]
        return (wt, repo)
    head = head_branch(pane_path)
    if head:
        for _, wt, repo in matches:
            if wt.get("branch") == head:
                return (wt, repo)
    _, wt, repo = matches[0]
    return (wt, repo)

count = 0
with open(sys.argv[2]) as f:
    for line in f:
        line = line.rstrip()
        if not line: continue
        cols = line.split("\t")
        if len(cols) < 6: continue
        pane_id, session, window_idx, pane_idx, pane_path, cmd = cols
        wt, repo = pick_worktree(pane_path)

        cells = []

        # Color palette (deterministic per category):
        #   STATUS  : per-state (red/yellow/green/blue/default)
        #   ID      : cyan,bold        (#NNN / PR#NNN)
        #   TITLE   : white             (issue title)
        #   BRANCH  : magenta            (git branch)
        #   LABELS  : yellow             (gh labels)
        #   REPO    : blue,italics       (orchard repo slug)
        #   CLAUDE  : per-state (green/brightblack/yellow) — hook state
        #   CMD     : green,bold         (running process — claude/zsh/vim/etc.)
        #   PATH    : brightblack        (only when no worktree)

        if wt:
            s_g, s_c = status_glyph(wt.get("pr"))
            if s_g:
                cells.append(cell(f"fg={s_c}", s_g))

            ids = []
            iss = wt.get("issue")
            pr = wt.get("pr")
            if iss: ids.append(f"#{iss['number']}")
            if pr:  ids.append(f"PR#{pr['number']}")
            if ids:
                cells.append(cell("fg=cyan,bold", " / ".join(ids)))

            title = (iss or {}).get("title")
            if title:
                cells.append(cell("fg=white", str(title)[:55]))

            b = wt.get("branch") or ""
            if b:
                cells.append(cell("fg=magenta", b))

            # labels is [{name, color, description}, ...] in v0.8 (was [String]
            # in pre-ADR-015 shape). Extract `.name` for the rendered chips.
            label_names = [l.get("name") for l in ((pr or {}).get("labels") or []) if l.get("name")]
            if label_names:
                cells.append(cell("fg=yellow", " ".join(f"[{l}]" for l in label_names[:3])))

            if repo:
                cells.append(cell("fg=blue,italics", repo))
        else:
            # No worktree match — show truncated path; cmd is rendered separately below.
            # HOME can be unset (cron, a stripped `env`, a systemd unit) and can be
            # empty; str.replace("") would splice a `~` between every character.
            home = os.environ.get("HOME") or ""
            short = pane_path.replace(home, "~") if home else pane_path
            if len(short) > 50:
                short = "…" + short[-49:]
            cells.append(cell("fg=brightblack", short))

        # Process indicator (always last) — what's actually running in the pane.
        # Note: tmux's pane_current_command often shows a version string (e.g. "2.1.132")
        # for claude because claude sets its window title; map that to "claude" explicitly.
        cmd_str = (cmd or "").strip()
        # Heuristic: if cmd looks like a version (digits.digits.digits), it's likely claude
        # which prints its semver as the process title.
        is_version_str = bool(cmd_str) and all(part.isdigit() for part in cmd_str.split(".") if part)
        if is_version_str and "." in cmd_str:
            cmd_str = "claude"
        is_claude_pane = cmd_str.startswith("claude")

        # Claude hook state (ADR-007) sits just before the process indicator.
        cells.extend(claude_cells(hook_state_for_pane(session, pane_path, is_claude_pane)))

        if cmd_str:
            interesting = {"claude","node","python","python3","go","cargo","ssh","mosh","vim","nvim","emacs"}
            if cmd_str in interesting or is_claude_pane:
                cells.append(cell("fg=green,bold", f"⏵ {cmd_str}"))
            elif cmd_str in {"zsh","bash","fish","sh","-zsh","-bash"}:
                cells.append(cell("fg=brightblack", f"⏵ {cmd_str}"))
            else:
                cells.append(cell("fg=cyan", f"⏵ {cmd_str}"))


        label = "  ".join(cells)
        if PRINT_MODE:
            print(f"{pane_id}\t{label}")
        else:
            # Set on the pane via target -t ${pane_id}
            subprocess.run(["tmux","set-option","-pt", pane_id, "@orchard_pane_label", label],
                           check=False)
        count += 1

print(f"orchard-tmux-labels: updated {count} panes", file=sys.stderr)

if not PRINT_MODE:
    # Also clear any stale session-level @orchard_label so the new pane labels are
    # what choose-tree renders (sessions fall back to default chrome).
    out = subprocess.run(["tmux","list-sessions","-F","#{session_name}"], capture_output=True, text=True)
    if out.returncode == 0:
        for n in out.stdout.splitlines():
            subprocess.run(["tmux","set-option","-t",n,"-u","@orchard_label"], check=False)
PY
}

run_once
