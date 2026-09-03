# Outer tmux wrapper (issue #747)

An outer tmux server wraps a nested inner tmux server — sidebar pane pinned
to a fixed width, inner session churning freely inside it — using shell +
tmux, plus three Go-side additions in `cmd/orchard-sidebar`: a routing shim
so the sidebar's own tmux execs reach the right server (see "Routing
orchard-sidebar's own tmux execs" below), a focus hand-back that returns
keyboard focus to the inner pane after a switch (see "Sidebar focus
hand-back" below), and the sidebar's own fixed-header/fixed-footer layout
and collapse button (see "Sidebar layout and collapse" below). The wrapper
itself ships as `cmd/orchard-shell` (dispatched as `orchard shell` per
ADR-013); `scripts/outer-shell/` retains `outer.conf` (embedded in the
binary; the file here is what a human edits — see `cmd/orchard-shell/conf_test.go`
for the drift guard) and `verify.sh`, the automated proof battery.

## How it works

```
outer server  (-L orchard-shell, its own outer.conf, no prefix — see below)
└── session "shell", 1 window, 2 panes
    ┌────────────────────┬──────────────────────────────────────┐
    │ pane 0.0 (40 cols,  │ pane 0.1                              │
    │  3 when collapsed)  │ TMUX= tmux -L <inner> attach -t <sess>│
    │ orchard-sidebar, or │  → nested inner client, full tmux     │
    │ watch -n1 tmux -L   │    prefix/keybindings/panes/windows   │
    │   <inner> list-     │    all working normally               │
    │   windows -a if not │                                        │
    │   on PATH           │                                        │
    └────────────────────┴──────────────────────────────────────┘
```

pane 0.0 is a real tmux pane in the OUTER session — it never enters the
inner server. pane 0.1 runs a normal `tmux attach` client INTO the inner
server; from the user's perspective that pane becomes the inner session,
prefix and all. Two independent tmux servers, one composited terminal —
but only one prefix in the traditional sense: outer has none (see
"No outer prefix" below), so the inner session's own prefix is the only
one the user ever needs to reach for.

## The one place `TMUX=` is needed

`orchard-shell`'s right pane runs:

```sh
TMUX= tmux -L "$INNER_SOCKET" attach -t "$SESSION"
```

Every process tmux spawns inside a pane inherits `$TMUX` from that outer
session. Attaching a second tmux client without clearing it first is not
a cosmetic warning — tmux hard-refuses to connect:

```
sessions should be nested with care, unset $TMUX to force
```

The attach never happens; the pane is left sitting at a dead shell prompt.
`TMUX=` before the inner `tmux attach` is the only place this matters,
because it's the only point where a second tmux client is created inside
a pane already owned by the first.

## Routing orchard-sidebar's own tmux execs

The sidebar binary itself execs `tmux` for a few reads/writes ADR-016/017/018
haven't grown a daemon mutation for yet (`switch-client`, `list-clients`,
`list-panes`, pane-width sync — tracked in orchardist#726). Those execs are
normally bare `tmux ...`, which resolves against whichever server owns the
pane the process happens to be running in.

Inside this wrapper that's wrong by default: `orchard-sidebar` runs as pane
0.0's command on the **outer** server, but every session it needs to read or
switch lives on the **inner** one. A bare exec would silently talk to the
wrong server — `switch-client` no-ops, the "which session is current"
highlight never anchors, and nothing on screen says why.

`orchard-shell` sets `ORCHARD_TMUX_SOCKET=<inner-socket>` when it starts the real
sidebar in pane 0.0 (falling back to the `watch` placeholder when the binary
isn't on PATH). `cmd/orchard-sidebar/env.go` resolves that variable ONCE at
startup into a `tmuxEnv`, and `env.innerCmd`/`innerCmdContext` route every
tmux exec through it: set, they prepend `-L <socket>`
and strip `TMUX` from the child env; unset — the sidebar's normal, unwrapped
mode — they exec `tmux` exactly as before, byte-for-byte. `switchClient`
also logs (rather than silently drops) a non-zero exit from `switch-client`.

Socket routing alone isn't enough: the inner server can be **shared** by
other, unrelated clients (a plain terminal attached to the same inner
session for other reasons). `switch-client -t <session>` with no `-c` lets
tmux pick *any* attached client to move — on a shared server that picked an
unrelated client instead of the wrapper's own pane (#747 defect 2, seen
live). `orchard-shell` also sets `ORCHARD_TMUX_CLIENT=<tty>`, resolved from
outer pane 0.1's `#{pane_tty}` right after sending the inner attach (that
pane's pty *is* the inner client's `client_tty`, once attached).
`switchClient` and `fetchClientSession`/`pickClient` scope every
client-targeting exec to `-c $ORCHARD_TMUX_CLIENT` when it's set, so a
switch only ever moves the wrapper's own client.

The two servers' identifiers are DISTINCT GO TYPES (`innerSocket`,
`clientTTY`, `outerPane`), so a pane id belonging to one cannot be handed to a
command aimed at the other — which is defect 1 in one line of compiler error
rather than a pane resizing on the wrong server. An environment that is
half-configured (wrapped with no client tty, or no `TMUX_PANE` at all) is
named in the sidebar's own header by `tmuxEnv.problem()`, because the symptom
otherwise is silence: switches refused, collapse doing nothing.

If `ORCHARD_TMUX_SOCKET` is set but `ORCHARD_TMUX_CLIENT` is not — a
configuration the shim shouldn't produce, but could reach via a stale
launch script or manual invocation — the sidebar refuses to run
`switch-client` at all and logs why, rather than falling back to an
unscoped switch that risks hijacking a foreign client on that socket.

This is an explicit interim shim, not the destination: the real fix is the
daemon `switchClient` mutation from #726, at which point the client stops
exec'ing tmux for this at all, per ADR-016.

## Sidebar focus hand-back

Selecting a row switches the INNER server's active session (`switchClient`,
above) — but with `mouse on` and `prefix None` (see "Keybindings and
mouse"), the OUTER server's own active pane doesn't move just because the
inner one did. Landing on pane 0.0 after a switch and finding every
keystroke still going to the sidebar, not the shell just switched to, was
issue #747's defect 3.

`orchard-shell` resolves outer pane 0.1's `#{pane_id}` once at boot and hands it
to the sidebar as `ORCHARD_OUTER_PANE`. `cmd/orchard-sidebar/env.go`'s
`env.outerCmd` execs `tmux` against the OUTER server with no modification —
deliberately bypassing `innerCmd`'s inner-socket routing, since
`ORCHARD_OUTER_PANE` names a pane on the outer server, not the inner one
every other exec in this file targets. `handBackFocus` runs it as
`select-pane -t $ORCHARD_OUTER_PANE`, synchronously, on `switchClient`'s own
goroutine, and only after `cmd.Wait()` confirms the inner switch itself
succeeded — never handing back focus onto a switch that failed. Failure to
hand back is logged, never fatal: a stuck outer pane is recoverable by hand
(`M-Left`/`M-Right`), a crashed sidebar is not.

`selectRow` threads a `handBack bool` through to `switchClient` so only a
click or Enter triggers the hand-back — `j`/`k` pass `false`. Handing back
focus on every cursor move would fight the user off the sidebar mid-browse,
before they've decided which row they actually want; selection (click or
Enter) is the point where the user is done navigating and the keyboard
should follow them to the shell.

Unset (legacy unwrapped mode, or wrapped but not yet given an outer pane),
`handBackFocus` is a no-op — the sidebar's switch-client behavior outside
this wrapper is unchanged.

## Sidebar layout and collapse

The sidebar pane is three bands, and only the middle one moves:

```
 orchard              ●3  +  « ← header: FIXED to the top
                                     title, the Needs-attention badge,
                Needs attention       launch/collapse buttons, banners
 ▌ ● fake-01-payments [opus]  11m ← list: takes what the other two leave,
 ▌  “wire the retry budget into…”     grouped under section headers,
 ▌  📁 fake-01-payments  issue#900    scrolls (wheel, or j/k) without ever
 ▌                                    pushing the other two bands
 ² ✖ fake-05-inbox [opus]     39m     the gutter carries the selection
   ...                                rail, or the M-1..M-9 ordinal
                           Done
 ⁵ ✓ fake-03-search [haiku]   25m
   ...
 ─────────────────────────────── ← footer: FIXED to the bottom
 ╭─ Git ───────────────────────╮    a rule, so the end of the scrolling
 │ 🌿 issue747/outer-wrapper ⧉ │    list is unambiguous, then the card the
 │ 📁 git-orchard-rs         ⧉ │    rail is on — its git facts, every line
 ╰─────────────────────────────╯    click-to-copy — then the hints
 / filter · M-1-9 jump · b bell·off
```

With the filter on, the title's place in the header row is taken by
`/query (n)`: the query as typed, and how many cards survived it. The badge
sits beside the buttons and counts the *bucket*, not the visible cards — the
filter decides what is drawn and nothing else.

`layoutPane(height, headerH, footerH)` in `cmd/orchard-sidebar/layout.go` is the
whole rule, and it is pure: given a height it returns the three rects, header
at row 0, footer ending on the last row, list filling the gap. A pane too
short for all three gives up footer rows first — the header carries the
collapse button, which is the only way back out of a mis-sized pane.
`layout_test.go` drives it at 24, 40 and 60 rows, and drives `View()` through
`tea.WindowSizeMsg` at the same heights to prove the rendered pane is exactly
that many lines with the title first and the hints last.

Clicking the `«` button (or pressing `M-s`, below) collapses the pane to a
3-column strip showing `»` and one state glyph per session; clicking anywhere
in the strip expands it again, back to the width the user last dragged to. A
collapse click hands keyboard focus back to the inner pane, exactly like a row
click does — a collapsed strip has nothing left to type at.

### Who owns the width

**The outer server does.** It holds two window options, and `outer.conf`'s
`M-s` binding and both of its resize hooks read them:

| Option | Meaning |
| --- | --- |
| `main-pane-width` | the width the sidebar reopens and re-pins to |
| `@sidebar_collapsed` | `1`/`0` — collapsed pins a literal 3 instead |

The sidebar's job is to notice the one event the outer server cannot see — the
user dragging the pane border — and publish it (`width.go`). Everything else
that changes the pane's width (a terminal resize, `M-s`, the resize hooks) is
the outer server re-pinning to what it already knows, and the sidebar reads
those back as its own machinery echoing rather than as a gesture.

Two owners was the bug: the sidebar published a width *inwards* while the
outer hooks re-pinned to a hard-coded 40, so a drag to 60 survived exactly
until the next terminal resize — at which point the hook pinned 40, the
sidebar read the 40 as a fresh drag, and republished it over the 60 the user
had asked for.

`main-pane-width` is tmux's own "how wide is the left column" option rather
than an `@orchard_sidebar_width` of our own, for one measured reason: a user
option cannot be read back out synchronously. `resize-pane -x` does not expand
formats in its argument (`-x "#{@orchard_sidebar_width}"` fails with "width
invalid" — measured on tmux 3.6a), and the `run-shell` workaround that *does*
expand them returns before its shell has resized anything, so the pane sits at
the wrong width for a beat after every resize and anything reading the width
straight afterwards reads the un-pinned one. `select-layout main-vertical`
reads `main-pane-width` inside tmux, with no shell and no delay, and is a
no-op on a window that still has only one pane (during boot, before
`orchard-shell` splits).

### What survives a restart

The width, the collapsed flag and the bell setting are also written to
`$XDG_STATE_HOME/orchard/sidebar-state.json` and restored at startup
(`state.go`): a sidebar that reopens at 40 columns every morning is a sidebar
the user re-drags every morning. The restore runs **synchronously in `main`,
before bubbletea reads the pane size** — applied asynchronously it would race
the first `WindowSizeMsg`, and the sidebar would take the pre-restore width
for the wrapper's own split and publish it back over the width it was
restoring. A missing or corrupt file means "nothing remembered", never a
refusal to start.

Both facts move together for a collapse to survive: `setCollapsed` writes
`@sidebar_collapsed` *before* the resize, so the hooks and `M-s` read the
width the sidebar just chose rather than the one it just left. Both go through
`outerCmd`: the sidebar's own pane lives on the OUTER server, so routing them
inwards would send an outer pane id to the inner server, where the same id
names a different pane — and resize *that* one.

The sidebar never reads either option back. It learns it was collapsed from
the width it is handed, which is the one signal every path shares: its own
button, `M-s`, and the resize hooks all end in a `WindowSizeMsg`. A collapsed
width is also explicitly *not* treated as a pane drag: publishing 3 as the
remembered width would collapse the sidebar for good, and enforcing the
readable floor (34) back over it would fight `M-s` open again on the next
tick.

## Grouping, scrolling, and card shape

The list answers one question — *what needs me?* — so it has three sections
and no more:

| Section | Rule (`rowBucket`, `model.go`) |
| --- | --- |
| **Needs attention** | state `input` or `stalled` — the agent is blocked on a human |
| **Done** | `idle`, hooked, and nobody attached — it finished and nobody has looked |
| **Sessions** | everything else: running agents, attached sessions, plain shells |

There is deliberately no "working" vs "idle" split. A running agent already
announces itself with a spinning marker, so a header saying the same thing is
noise; and an idle session someone is sitting in is not a result waiting to be
collected, which is why `attached` disqualifies a row from **Done**.

Within a section, rows are ordered by **most recent activity first** (a row
with no activity timestamp sorts last, then by name), so the thing that just
changed is the thing at the top of its section. `sortRows` is stable across
refreshes because the whole ordering is derived from row data, not from
arrival order.

Markers carry the same three-way meaning in colour: amber `●`/`✖` for
attention, green `✓` for done, a cyan spinner for a working agent, dim `·`/`○`
otherwise (`marker` in `format.go`, `lipgloss.AdaptiveColor` so all three stay
legible on light and dark terminals).

**Every card is exactly `cardRows` (4) lines**, whatever the session has to
say: name+model+age, mission (blank if there is none), directory + a tag
(issue ref, `pr#N`, or branch), blank. A card whose height depended on its
content made the list impossible to scan and made scrolling jump by
unpredictable amounts. Below `minWidth` the pane switches to
`compactCardRows` (2) — name and a blank — since the detail lines have no room
to say anything true at that width.

The state glyph, model tag, and last-message text on a Claude row all come
from the `claude-session-state` Claude Code plugin (this repo's marketplace,
`plugin-sources/claude-session-state`), whose hooks write
`~/.local/state/claude-sessions/state/<sid>.json`. Without it installed a
Claude row still draws — it just has nothing to say. `orchard shell doctor`'s
`plugin` check (issue #772) warns when the plugin is missing or its hooks have
never fired, with the install remedy.

Scrolling is a property of the middle band alone:

- **The wheel scrolls, and never selects.** Selection attaches a session, so a
  wheel notch that also moved the cursor would yank the terminal into whatever
  it scrolled past. `wheelStep` is 3 lines per notch.
- **The viewport moves for exactly one reason: the selection is OFF SCREEN.**
  That is `j`/`k` walking past an edge, or an attach that landed somewhere the
  user is not looking (someone switching this client from another terminal).
  When it fires, `scrollOffset` scrolls the *minimum* that brings the whole
  card back, measured from where the viewport already is — walking down by one
  card scrolls by one card, in either direction. It used to ignore the current
  offset and re-derive one from the top of the list, so a single `k` onto the
  card just above the viewport jumped the list back to line 0.
- **A click therefore never moves the viewport at all.** A click can only land
  on a line that is already drawn, so the card it selects is on screen by
  construction and the rule above declines to fire. Snapping on any selection
  *change* — which is what this did before — meant every click re-derived the
  offset and threw away wherever the user had scrolled to ("clicking something
  shouldn't reset the position of everything", reported live). `rowOnScreen`
  counts a single visible line as on screen, precisely so that a click on a
  card clipped by the viewport edge is still a no-move.
- **Selection is compared by SESSION, never by cursor index.** `render()`
  compares the session under the cursor against the one it last drew
  (`drawnSess`). The index is not the selection: rows re-sort on every 2s
  refresh as activity changes, so comparing indexes made every poll look like
  the user had moved the cursor and threw the scroll position away every two
  seconds (observed live).
- **A refresh never moves the viewport.** The scroll position is user state.
  Between paints the offset is re-derived from the card that was on top
  (`anchorSess` plus the line offset into that card), so rows appearing,
  disappearing or re-sorting slide *under* a viewport that stays where it was
  pointed. A deliberate scroll retires the anchor; a shrinking list clamps the
  offset to the new end rather than resetting it to the top; and if the
  anchored card is gone entirely the raw offset is kept and clamped.
- **...except at the very top, which is a position rather than a card.**
  Parked at offset 0 the user is looking at *the start of the list*, so a row
  arriving above the anchored card has to appear there. Anchoring to the card
  instead pushed the viewport down by exactly the new card's height to keep the
  old one pinned — which silently hid every newly-arrived **Needs attention**
  session, and its section header, from anyone sitting at the top of the list,
  which is where this sidebar is normally left.

The footer is a **constant height**, for the same reason cards are: the git
box always draws `gitBoxRows` (4) body rows, padding with empty ones and
drawing even when the selection has nothing to put in it. Four is the most
`gitBoxItems` can produce (branch, directory, issue, PR), so a fixed height
never drops a fact — a 3-row box would hide the PR line for exactly the
sessions with the most going on. Before this, the box grew and shrank with
whatever the selected session happened to know, so the list band above it
changed height and every card jumped as the cursor moved.

`ORCHARD_SIDEBAR_FAKE=N` appends N synthetic rows (`fake.go`) — stable,
index-derived, obviously named `fake-NN-*`, and spread across all three
sections. They exist to make the scrolling and grouping testable at sizes the
machine running the tests doesn't happen to have. A synthetic row is inert:
`selectRow` returns before `switchClient` for any row flagged `fake`, so
scrolling through test data can never attach a client to anything. `applyHooks`
skips them too — they carry their own state and have no hook file, so the
overlay would otherwise strip their hooked flag on the next rebuild and paint
every synthetic card as state-unverified. `bellCheck` skips them for a third
reason: a row that exists to be scrolled past must not make a sound.

### Finding a session: the `/` filter

At thirty sessions the list is longer than the pane, and scrolling to a name
you already know is the wrong gesture. `/` opens a field in the header row —
`textField` over `bubbles/textinput`, the same input the rename box and the
launch modal use, so a paste or a fast repeat lands whole. Typing narrows the
cards by a case-insensitive substring over every fact a card shows: name,
mission, directory, branch, repo slug, and the issue or PR ref
(`rowMatches`, `filter.go`).

The filter is a **view**, and the constraints follow from that one word:

- It never runs a `switch-client`. What you are attached to is not decided by
  what you type into a search box.
- It never changes the selection's *identity*, so `Esc` gives you back the
  session you started from. When the query hides that card the **rail** moves
  to the first visible one — `railIndex` — so a narrowed list still has a "you
  are here" to navigate from, and `j`/`k` and `M-1`..`M-9` all walk the visible
  list from there.
- The footer's git box and the `+` modal's starting directory both follow the
  rail (`railRow`), never the hidden selection: everything the pane describes
  is a card the pane is drawing.
- The Needs-attention badge counts the bucket, not the matches. The filter
  decides what is *drawn*; it does not get to change how much needs you.
- Nothing matching draws an explicit no-match line, never an empty band — a
  blank list reads as "the sidebar broke", which is the one thing it must not
  say while working correctly.

`Enter` closes the field and keeps the query, so the list is navigable without
leaving a mode; `/` reopens it with the query intact and the cursor at the end.

### The attention badge and the bell

The header carries `●N` in the attention amber whenever the Needs-attention
bucket is non-empty, and nothing at all when it is — a `0` badge teaches you to
stop reading badges. Collapsed, the 3-column strip carries the same number on
the line under its `»`, which is the one thing three columns can usefully say.

`b` toggles a bell, off by default and remembered in `sidebar-state.json`. It
rings on a **transition**, not on a state: one BEL when a session *enters* the
bucket, and silence when the count merely falls or holds. Three cases it
deliberately stays quiet for, each of which is a way a bell becomes noise:

| Case | Why |
| --- | --- |
| The first list | What was already waiting when the sidebar started is a snapshot, not an arrival |
| A synthetic row | `ORCHARD_SIDEBAR_FAKE` rows are scroll furniture |
| A row that vanished and came back | Rows disappear wholesale when a lane fails (`applyFast`'s wipe, `applySessions` dropping a session tmux stopped reporting). A session is forgotten only while it is still on screen and has genuinely left the bucket — otherwise the next good poll re-rings the whole list |

`bellCheck` hangs off `rebuild()` rather than off each lane, because every path
that can put a session into the bucket ends there: a transition detected in
three places is a transition detected three times. The BEL goes to `/dev/tty`
as a single byte — a C0 control, which a terminal executes wherever it lands,
so it cannot corrupt an escape sequence bubbletea is midway through writing.
tmux forwards it from the pane per its own `bell-action`.

### The update indicator

The header carries a dim `⇡vX.Y.Z` whenever `internal/release`'s cached
check file names a version newer than this binary's own — read on startup
and every 10 minutes (`update.go`), never fetched directly: `orchard shell`
writes the cache on its own schedule (a detached child process at startup,
see `cmd/orchard-shell/updatecheck.go`), and the sidebar only ever reads it
(RULES T1). A missing, corrupt, or stale-shape file reads as "nothing
cached" and shows nothing, logged at most once rather than on every 10-minute
tick; a `dev` build always reads as older than any real release, matching
`internal/release/semver.go`'s own comparison. Collapsed, the 3-column strip
carries the bare glyph with no version — the same three columns the
attention count uses above it.

The glyph sits ahead of the `●N` badge in the header's right-hand strip
and is the first to go on a narrowing pane: its floor is 24 inner columns,
above the badge's own 18, so it disappears well before the badge does and
can never crowd either button. Clicking it opens the same kind of in-app
overlay as the row menu — one line, `boxRender`, dismissed by any keypress —
naming the two versions and the command that closes the gap:
`orchard upgrade`.

## The modal rule

Two modals, drawn two different ways, and the choice is not taste:

> **In-app overlay for anything that acts on a row and must be
> capture-pane-testable; `display-popup` only when the modal needs a full-window
> canvas and a separate program.**

A `display-popup` (and `display-menu`) composites into an *attached client's*
rendered stream and appears in no pane's grid buffer, so `capture-pane` cannot
see it — a popup is only testable with a real attached pty client (see "Popups
need a real attached client"). It also lives on the OUTER server, while every
session a row menu acts on lives on the inner one.

So the row menu (rename/close a session) is drawn into the sidebar's own pane,
and the launch modal — which needs a directory browser, three fields and a
whole window to put them in, and which runs as its own program — is a popup.
`verify.sh` covers each accordingly: the row menu through `capture-pane`, the
launch modal through a real pty client.

## The + launch modal

The header's `+` opens a **new session** in a `display-popup` over the whole
window: a directory picker, the command to run, the session name, and a launch
button.

```
 ┌ New session ────────────────────────┐
 │ /Users/you/workspace/git-orchard-rs │  ← where you are
 │  ..                                 │
 │  cmd                                │  ← j/k moves, ⏎ descends,
 │  docs                               │    ⌫ goes up, typing filters
 │  internal                           │
 │ filter: doc                         │
 │ command: claude                     │  ← prefilled with the LAST command
 │ name:    git-orchard-rs             │    launched, dir name otherwise
 │ [ Launch ]                          │
 └─────────────────────────────────────┘
```

The flow, end to end:

1. The click lands in `launchZone` (the header's right-hand buttons publish
   their hit rects with every composed frame), and focus is handed back to the
   inner pane FIRST, *then* the popup opens over it — the other order raced,
   with a `select-pane` arriving underneath a popup that already had the
   keyboard. Either way the end state is the same whether the popup opens,
   fails, or is cancelled: the keyboard is on the shell, where it was.
2. `openLaunchPopup` runs `display-popup -E` **in a goroutine** — the command
   blocks for as long as the popup is open, so calling it inline would freeze
   the sidebar's update loop behind its own modal. A popup rather than an
   in-pane overlay per the modal rule above: it needs a full-window canvas and
   runs a separate program.
3. The popup runs this same binary as `orchard-sidebar launch`. A popup does
   not inherit the sidebar pane's environment, so the socket and client it
   must talk to ride along as explicit `-e` flags (`popupArgs`); without them
   the modal would build the session on whichever tmux server it found first.
4. The picker starts in the selected session's directory (`-d`, and only when
   that directory still exists — a removed worktree is a normal thing to
   inherit, and `-d` on a missing path fails the whole popup rather than
   opening it on a fallback).
5. Launch runs `new-session -d -s NAME -c DIR CMD`, then switches this
   wrapper's own client to it (`switchClientArgs`, `-c` scoped as ever), then
   records the command in
   `${XDG_STATE_HOME:-~/.local/state}/orchard/sidebar-last-launch.json` so the
   next `+` prefills it.

Details that are easy to get wrong and are pinned by tests:

- **The command is one argument, never split on spaces** — `claude --resume x`
  has to reach tmux whole (`newSessionArgs`).
- **Names are deduped and sanitised** — tmux refuses a duplicate name outright
  (which reads as "the button did nothing"), and `.`/`:` in a session name
  make every later `-t` target ambiguous, so `uniqueName` yields `api-2`,
  `api-3`, and `v1.2` becomes `v1-2`.
- **A failed launch keeps the modal open** with the error on it; closing on
  failure is indistinguishable from success.
- **Typing is the filter**, so the picker's own controls move off plain
  letters: `⏎` descends, `⌫` deletes a filter character or goes up when the
  filter is empty, `.` picks the directory you are standing in, `⌥h` toggles
  hidden entries, `^u` clears. Typing a filter also parks the cursor on the
  first match rather than on `..`, or typing a directory name and pressing
  enter would walk *up*.
- **A space is `KeySpace`, not `KeyRunes`** in bubbletea, and a burst of typed
  characters arrives as one `KeyRunes` message with several runes in it.
  Reading only single-rune `KeyRunes` messages silently ate both — `sleep 300`
  became `sleep300` — so both fields go through `typedRunes`.

Per ADR-016 this is a **client-side shellout that should not survive the
spike**. The gap is specific and worth naming: the daemon already exposes a
`launchSession(input: LaunchSessionInput!)` mutation, but its input carries
only `cwd`, `name`, `model` and `prompt` — there is no field for an arbitrary
command, so it cannot express "run the last command in this directory", which
is exactly what this button does. Promoting this past spike stage means adding
that field (or a sibling mutation) and having the modal call it instead of
running `new-session` itself.

## The right-click row menu

Right-click a card for the two things you do *to* a session rather than with
it:

```
   ● fake-01-payments [opus]  11m
 ╭─ fake-01-payments ─────────╮   j/k or ↑↓ moves, ⏎ opens,
 │ ▸ Rename                   │   esc — or a click outside the
 │   Close                    │   box — dismisses
 ╰────────────────────────────╯
```

`Rename` swaps the body for a text field prefilled with the current name (the
usual edit is a suffix, not a retype); `Close` swaps it for
`Close <name>? y/N`, and anything that is not a `y` is a no.

What is load-bearing rather than cosmetic:

- **It is drawn into the pane, not opened as a tmux menu** — the modal rule
  above. It acts on a row and has to be `capture-pane`-testable, which a
  `display-menu`/`display-popup` can never be, and it would live on the OUTER
  server while every session it acts on lives on the inner one.
- **Right-click does not select, and does not hand back focus.** Selection
  attaches the terminal to a session (see "Sidebar focus hand-back"), so "tell
  me about this one" must not move the user; and the menu is the one thing in
  this sidebar that needs the keyboard, so unlike a left-click it keeps it.
- **The menu holds its row by session NAME, not by index.** Rows re-sort under
  an open menu on every 2s refresh; an index would quietly come to point at a
  different session, and this menu kills things.
- **An open menu owns the keyboard and the mouse.** `q` closes it rather than
  quitting the sidebar, `j`/`k` move the highlight rather than walking the
  selection (which would attach a session out from under the menu the user is
  reading), and a click that dismisses the menu stops there rather than falling
  through to the card the box was covering. `Ctrl-C` is the exception and still
  quits.
- **Never kill the session this wrapper's own client is sitting in.** tmux
  drops a client whose session dies, which here means the user's terminal goes
  with it. `commitClose` switches the client to another session *first*
  (`switchClient`, `-c` scoped as ever) and kills second; with nowhere to move
  to, the close is refused outright and says why.
- **Synthetic rows open a menu and decline both actions,** with a notice. They
  name no tmux session (`fake.go`), so either action would target something
  that does not exist.
- **A failed action keeps the box open with tmux's own message on it** — same
  judgment as the launch modal: closing on failure is indistinguishable from
  having worked. A successful rename also carries every identity that pointed
  at the old name (the selection, the scroll anchor, the hook/attach/pane maps)
  across to the new one, so the row does not vanish and reappear as a ghost
  card during the two seconds before the next poll re-reads tmux.

Per ADR-016 these two mutations are client-side tmux execs, exactly like
`switchClient` and the launch modal above. There is nothing to call instead:
the daemon's `Mutation` type is `sendTextToPane`, `launchSession`,
`worktreeRemove` and `worktreesCleanup` and nothing else — no session rename,
no session kill (introspected against the live daemon, 2026-09-02). Promoting
this past spike stage means adding those mutations; tracked with the rest of
the client-side tmux execs in orchardist#726.

`verify.sh` drives the whole gesture through raw SGR bytes on the hermetic
sockets: right-click opens the menu, `esc` closes it, and a rename driven
through the box actually renames a session on the inner test server. That last
check needs a row that is not synthetic, so the check writes its own
`claude-session-state` file into a scratch `CLAUDE_SESSION_STATE_DIR` naming a
pane on the inner test socket. Pointing the state dir at a scratch directory is
also what makes the check hermetic at all: left at its default it reads the
user's live session state, and a real state file whose pane id happened to
collide with one on the test socket would put a live session's card in a pane
this check right-clicks and renames.

## Diagnostics never go to the pane

Every tmux exec in the sidebar can fail, and the sidebar holds the alt screen
for its whole life — so a diagnostic written to stderr lands *in* its pane, on
top of the UI, and survives until something repaints that exact cell. Observed
live on 2026-09-02: a wrapper whose pane 0.1 had been left as a plain shell
rather than an inner `tmux attach` had no inner client at all, so every
selection failed `switch-client -c <tty>` with `can't find client`, and a few
of those shredded the sidebar into unreadable strips of half-drawn cards.

`logf` (`log.go`) writes those lines to
`${XDG_STATE_HOME:-~/.local/state}/orchard/sidebar.log` instead, and is
best-effort: a sidebar that cannot open its log still has a job to do. It holds
the file open for the process's life and caps it at 1 MiB, starting over at the
cap: a wedged inner server fails an exec on every tick, so an uncapped
append-only log is a slow disk leak on the one machine least able to notice it,
and the newest failures are the ones worth keeping.

Every error channel ends either in the UI or in that file — none is dropped.
The ones that reach the UI are the ones a user can act on: the row menu's
notice line, the daemon-offline banner, a `↯` beside the title when the push
lane has dropped and the sidebar is polling, and a `⚠` line naming a
half-configured wrapper environment (`tmuxEnv.problem()` — wrapped with no
client tty, or running outside a tmux pane, both of which are otherwise
silent). The rest — a failed slow-lane poll, an unreadable state dir, a
`pbcopy` that did not run — go to the log.

The environment that produced it is worth naming, because the sidebar cannot
fix it from the inside: `ORCHARD_TMUX_CLIENT` names the tty of the wrapper's
own inner client, resolved from pane 0.1 at boot (see "Routing
orchard-sidebar's own tmux execs"). If pane 0.1 is not running
`TMUX= tmux -L <inner> attach` — someone exited the attach and got a shell back
— then that tty is a client of nothing and no value is correct: the wrapper has
no inner client to switch. Pointing the variable at some *other* client on the
inner socket is the one thing that must not happen; that is #747 defect 2, and
it hijacks a terminal the user never pointed at this sidebar. The fix is to
restore the inner attach in pane 0.1, not to re-aim the variable.

## Keeping the sidebar pinned

Two hooks in `outer.conf`, not one:

- `client-resized` — fires when an **attached client's** terminal actually
  changes size (real SIGWINCH).
- `window-resized` — fires on any window resize, **including** a scripted
  `resize-window -x/-y` command run against a session with **no attached
  client**. `client-resized` does not fire for that path at all.

Both re-pin the sidebar to the width `@sidebar_collapsed` says it should have
— `if-shell -F '#{@sidebar_collapsed}'`, so 3 while collapsed and 40
otherwise, rather than unconditionally 40, which would pop a collapsed sidebar
back open on the next terminal resize. Neither hook fires for anything
happening inside the *inner* server — the two servers share nothing, so
inner pane/window churn (split, kill, zoom, layout, resize) is structurally
invisible to the outer session. That's the whole trick; the hooks only
exist to correct outer-level resizes, not to defend against the inner one.

## Keybindings and mouse

All outer-level bindings live in the root key table — there is no prefix
table at all (see "No outer prefix" below) — on Alt keys and mouse events
chosen so nothing a shell or the inner tmux config reaches for collides:

| Binding | Action |
| --- | --- |
| `M-s` | Collapse/expand the sidebar — the keyboard half of its `«`/`»` button: `resize-pane -t 0.0 -x 3\|40` plus the matching `@sidebar_collapsed` window option (see "Sidebar layout and collapse") |
| `M-p` | Open a placeholder command palette popup (`$SHELL` in `display-popup`) |
| `M-d` | Detach the outer client |
| `M-Left` | Focus pane 0.0 (sidebar) |
| `M-Right` | Focus pane 0.1 (inner client) |
| `M-Up` / `M-Down` | Move the sidebar's selection from either pane — `send-keys -t 0.0 Up\|Down` (see "Driving the list from the keyboard") |
| `M-1` … `M-9` | Jump the sidebar's selection to the nth *visible* card from either pane — `send-keys -t 0.0 M-N`, forwarded for the same reason `M-Up`/`M-Down` are. Hands focus back like a click; past the end of the list it does nothing |
| `↑`/`↓`, `j`/`k` (sidebar focused) | Previous/next *visible* session. Selection *is* the switch, so these attach as they move |
| `/` (sidebar focused) | Open the filter in the header row. Typing narrows the cards by any fact a card shows — name, mission, directory, branch, issue or PR ref — case-insensitively. `Enter` keeps the query and gives the keys back to the list; `Esc` clears it |
| `b` (sidebar focused) | Toggle the attention bell, and remember it in `sidebar-state.json` |
| Mouse click | Focus + forward the click to whichever pane is under the cursor (tmux's default `MouseDown1Pane`; `mouse on` is the only config this needs) |
| Click `+` (sidebar header) | Open the new-session modal in a popup, on the selected session's directory (see "The + launch modal") |
| Wheel over the sidebar list | Scroll the list only — never moves the selection, so it can never attach a session (see "Grouping, scrolling, and card shape") |
| Right-click a card (sidebar) | Open the row menu — rename or close that session (see "The right-click row menu"). `outer.conf` rebinds `MouseDown3Pane` to forward into the pane; stock tmux would open its own Split/Kill/Respawn menu instead |
| Wheel up/down | Forward the raw scroll event to whichever pane is under the cursor (`WheelUpPane`/`WheelDownPane` rebound to `send-keys -M` — see "Mouse on, but forwarding-only" below) |

### Driving the list from the keyboard

Bare `↑`/`↓` and `j`/`k` are the same call into `selectRow` — the arrow keys
are matched on `tea.KeyDown`/`tea.KeyUp` rather than as extra string cases, so
the two spellings cannot drift apart. They work whenever pane 0.0 holds the
keyboard.

That last clause is the catch, and it is why `M-Up`/`M-Down` exist. Clicking a
card hands focus straight back to the inner pane (see "Sidebar focus
hand-back") — so the very gesture that puts a user in front of the sidebar
also takes the arrow keys away from it, and the list appears frozen. Binding
bare `Up`/`Down` at the outer level would fix that by stealing the arrow keys
from the inner shell and from Claude, which is far worse than the problem.
`M-Up`/`M-Down` forward a plain arrow to pane 0.0 explicitly: sidebar
navigation works from either pane, unmodified arrows stay the inner client's,
and focus does not move.

`M-1`..`M-9` are the same trick for going somewhere directly instead of one
card at a time, and they forward the chord itself rather than a translation of
it. The first nine visible cards wear matching superscript markers in the
one-cell selection gutter, so the chord is *read off the list* rather than
counted — and because the marker shares the gutter with the selection rail,
neither costs the list a column. The rail wins that cell on the card it lands
on: you are already there, so the chord that would take you there is the one
marker worth giving up. `M-0` is deliberately unbound — it has no ordinal to
mark it with, and it stays the inner session's.

The nth card is the nth *visible* card, which is what makes the chords work
under the `/` filter: `M-2` over three matches selects the second match, and
`M-9` over three matches does nothing rather than reaching into the rows the
filter is hiding.

One subtlety worth keeping: bubbletea coalesces a burst of runes arriving in a
single read into **one** `KeyRunes` message, so holding `j` down produces
`msg.String() == "jjj"`. The handler therefore walks `msg.Runes` one at a time
instead of switching on the joined string — switching on it moved nothing at
all under a held key, which is the input that most means "move". Escape
sequences are not coalesced this way, so arrows were unaffected; the two paths
are tested against each other (`TestArrowKeysMatchJK`,
`TestKeyBurstMovesOncePerRune`) and end to end through a real pty in
`verify.sh`, which injects `\e[B` and `\e[1;3B` rather than using `send-keys`
— `send-keys` hands tmux a pre-parsed key name and would skip the entire
terminal→tmux→app path these checks exist to exercise.

`M-Left`/`M-Right` exist because mouse-only focus has no keyboard
equivalent otherwise — with `prefix None` there is no prefix-table binding
to fall back to, so a keyboard-only session (no terminal mouse reporting,
an SSH client that eats mouse escapes, a screen reader, ...) would have no
way to move focus between panes at all without them. This was #747's
original live defect: boot-time focus landed on 0.0 with no bound way off
it.

## Trade-offs

- **No outer prefix.** A prefix key at the outer layer swallows itself AND
  the next keystroke before either ever reaches pane 0.1 — `send-keys`
  bypasses a client's key-table dispatch entirely, so this only reproduces
  through a real attached client, not `capture-pane` probing. With `C-a` as
  outer's prefix this broke Ctrl-A (readline start-of-line) inside the
  inner shell: the second keystroke was silently consumed by the (mostly
  empty) prefix table before ever falling through. Fix: `set -g prefix
  None` plus `unbind -a -T prefix` — outer has no prefix at all. Its
  actions live in the root key table instead (see "Keybindings and mouse"
  above), on bindings no shell or inner tmux config reaches for. Nothing
  else is bound outer-side, so every other keystroke — including the inner
  session's own prefix — falls straight through untouched.
  `scripts/outer-shell/verify.sh`'s "outer prefix does not swallow
  keystrokes" check drives a real attached pty client (send-keys can't
  exercise key-table dispatch) to prove this; confirmed it fails against
  the old `C-a`-prefix config and passes against the current one.
- **Mouse on, but forwarding-only.** Turning `mouse on` at the outer layer
  risks the same class of bug as a prefix key: tmux's default mouse
  bindings don't just report clicks, several of them ACT on the outer
  server directly instead of forwarding to the pane under the cursor.
  Click-to-focus needs no explicit bind — `MouseDown1Pane`'s default
  (`select-pane -t = ; send-keys -M`) already does exactly "focus the pane
  under the cursor, then forward the click", so `mouse on` alone is enough
  there. Two other defaults fight that goal and are unbound:
  `MouseDrag1Pane` (drags default into outer's OWN copy-mode rather than
  forwarding) and `MouseDrag1Border` (`resize-pane -M`, which would let a
  drag shrink the pinned 40-col sidebar — the resize hooks below only
  correct WINDOW-level resizes, not a live border drag within an unchanged
  window). Wheel scroll has no such rescue: on this tmux build both
  `MouseDrag1Pane` and `WheelUpPane` fall back to outer's own copy-mode
  whenever `pane_in_mode`/`mouse_any_flag`/`alternate_on` are all false —
  exactly pane 0.1's normal state — and `WheelDownPane` has NO default
  root-table binding at all (verified via `tmux list-keys -T root`):
  wheel-down over any pane was silently swallowed, forwarded nowhere.
  Both wheel directions are rebound to `send-keys -M`, which
  unconditionally forwards the raw scroll event to whichever pane is under
  the cursor and never opens outer's own copy-mode. `verify.sh` confirms
  wheel-up lands in the INNER session's own copy-mode/scrollback (not
  outer's) and wheel-down is forwarded as raw input rather than swallowed.
  The missing `WheelDownPane` default is a pre-existing tmux gap at any
  nesting depth — reproduces the same way unwrapped, one server deep, no
  outer layer involved — not something this wrapper regresses; rebinding
  it here happens to close that gap too, incidentally.
- **Second server, second config file.** A `-L` socket does **not**
  suppress loading the user's real `~/.tmux.conf`; omitting `-f <outer.conf>`
  on every invocation would silently pull in the user's actual config
  (their prefix, their plugins, their status line) into what's supposed to
  be a minimal wrapper. `orchard-shell` and `verify.sh` both pass `-f` on every
  tmux invocation against the outer socket for this reason.
- **Popups need a real attached client.** `display-popup` requires at
  least one attached client to render into (`no current client` otherwise)
  and its content is composited only into that client's output stream —
  it never appears in any pane's grid buffer, so `capture-pane` cannot see
  it. Proving it renders required attaching a real pty client and reading
  the raw bytes tmux sent it, not polling pane content.
- **`window-size manual` is unusable on this build.** Tried as an
  alternative to the hook-pair above (let tmux itself refuse to track
  client size); combined with `-x/-y` on `new-session -d` it crashes the
  tmux 3.6a server outright. Abandoned; the hook-pair approach above has
  no such failure mode and was proven stable across every check in
  `verify.sh`.
- **`script(1)` is not a reliable stand-in for a real terminal on macOS.**
  Early attempts to fake an attached client for testing via
  `script -q /dev/null tmux ... attach` were intermittently flaky
  (`tcgetattr/ioctl: Operation not supported on socket`). `verify.sh`
  instead forks a small Python helper (`pty.openpty()` + `os.fork()` +
  `TIOCSCTTY`/`TIOCSWINSZ`) that attaches a real client at an exact size
  and logs the raw stream to a file — robust, no flakiness observed across
  repeated runs.

## Next steps

- Write a `specs/features/` BDD feature for the wrapper behavior originally
  proved here as a spike (sidebar width invariant, `TMUX=` clearing,
  popup-over-inner rendering) so it graduates from spike to spec'd behavior.
- `orchard-shell`'s boot path is already wired into `orchard shell` (ADR-013);
  remaining work is #3's reattach hardening and #726's daemon mutations below.
- Per ADR-016/017/018 (GraphQL is the protocol, no client-side shellouts):
  the outer server's mutating operations here — attaching a client,
  opening a popup, resizing the sidebar — are exactly the shape of
  operations that should eventually be daemon GraphQL mutations
  (`switchClient`, `openPopup`, ...) rather than scripts a client shells
  out to directly. This prototype deliberately stays shell-only per the
  issue's scope; promoting it past spike stage should route through the
  daemon per RULES.md M2 (client shellouts map to daemon mutations).
