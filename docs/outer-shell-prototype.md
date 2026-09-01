# Outer-tmux-wrapper prototype (issue #747)

Spike proving an outer tmux server can wrap a nested inner tmux server —
sidebar pane pinned to a fixed width, inner session churning freely inside
it — using shell + tmux, plus one small Go-side routing shim in
`cmd/orchard-sidebar` so the sidebar's own tmux execs reach the right
server (see "Routing orchard-sidebar's own tmux execs" below). Lives at
`scripts/outer-shell/`.

## How it works

```
outer server  (-L orchard-shell, its own outer.conf, no prefix — see below)
└── session "shell", 1 window, 2 panes
    ┌────────────────────┬──────────────────────────────────────┐
    │ pane 0.0 (40 cols)  │ pane 0.1                              │
    │ orchard-sidebar, or │ TMUX= tmux -L <inner> attach -t <sess>│
    │ watch -n1 tmux -L   │  → nested inner client, full tmux     │
    │   <inner> list-     │    prefix/keybindings/panes/windows   │
    │   windows -a if not │    all working normally               │
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

`launch.sh`'s right pane runs:

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

`launch.sh` sets `ORCHARD_TMUX_SOCKET=<inner-socket>` when it starts the real
sidebar in pane 0.0 (falling back to the `watch` placeholder when the binary
isn't on PATH). `cmd/orchard-sidebar/main.go`'s `tmuxCmd`/`tmuxCmdContext`
helpers check that var on every tmux exec: set, they prepend `-L <socket>`
and strip `TMUX` from the child env; unset — the sidebar's normal, unwrapped
mode — they exec `tmux` exactly as before, byte-for-byte. `switchClient`
also logs (rather than silently drops) a non-zero exit from `switch-client`.

This is an explicit interim shim, not the destination: the real fix is the
daemon `switchClient` mutation from #726, at which point the client stops
exec'ing tmux for this at all, per ADR-016.

## Keeping the sidebar pinned

Two hooks in `outer.conf`, not one:

- `client-resized` — fires when an **attached client's** terminal actually
  changes size (real SIGWINCH).
- `window-resized` — fires on any window resize, **including** a scripted
  `resize-window -x/-y` command run against a session with **no attached
  client**. `client-resized` does not fire for that path at all.

Both re-run `resize-pane -t 0.0 -x 40`. Neither hook fires for anything
happening inside the *inner* server — the two servers share nothing, so
inner pane/window churn (split, kill, zoom, layout, resize) is structurally
invisible to the outer session. That's the whole trick; the hooks only
exist to correct outer-level resizes, not to defend against the inner one.

## Trade-offs

- **No outer prefix.** A prefix key at the outer layer swallows itself AND
  the next keystroke before either ever reaches pane 0.1 — `send-keys`
  bypasses a client's key-table dispatch entirely, so this only reproduces
  through a real attached client, not `capture-pane` probing. With `C-a` as
  outer's prefix this broke Ctrl-A (readline start-of-line) inside the
  inner shell: the second keystroke was silently consumed by the (mostly
  empty) prefix table before ever falling through. Fix: `set -g prefix
  None` plus `unbind -a -T prefix` — outer has no prefix at all. Its three
  actions live in the root key table on Alt bindings instead, which no
  shell or inner tmux config reaches for: `M-s` toggles the sidebar
  (`resize-pane -Z -t 0.1`), `M-p` opens a placeholder command palette
  (`display-popup -E -w 80% -h 80% "$SHELL"`), `M-d` detaches. Nothing else
  is bound outer-side, so every other keystroke — including the inner
  session's own prefix — falls straight through untouched.
  `scripts/outer-shell/verify.sh`'s "outer prefix does not swallow
  keystrokes" check drives a real attached pty client (send-keys can't
  exercise key-table dispatch) to prove this; confirmed it fails against
  the old `C-a`-prefix config and passes against the current one.
- **Second server, second config file.** A `-L` socket does **not**
  suppress loading the user's real `~/.tmux.conf`; omitting `-f <outer.conf>`
  on every invocation would silently pull in the user's actual config
  (their prefix, their plugins, their status line) into what's supposed to
  be a minimal wrapper. `launch.sh` and `verify.sh` both pass `-f` on every
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

- Write a `specs/features/` BDD feature for the wrapper behavior this
  spike proved (sidebar width invariant, `TMUX=` clearing, popup-over-inner
  rendering) so it graduates from spike to spec'd behavior.
- Wire `launch.sh`'s boot path into the real session-launch flow instead
  of being invoked standalone.
- Per ADR-016/017/018 (GraphQL is the protocol, no client-side shellouts):
  the outer server's mutating operations here — attaching a client,
  opening a popup, resizing the sidebar — are exactly the shape of
  operations that should eventually be daemon GraphQL mutations
  (`switchClient`, `openPopup`, ...) rather than scripts a client shells
  out to directly. This prototype deliberately stays shell-only per the
  issue's scope; promoting it past spike stage should route through the
  daemon per RULES.md M2 (client shellouts map to daemon mutations).
