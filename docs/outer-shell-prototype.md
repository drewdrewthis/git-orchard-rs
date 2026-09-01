# Outer-tmux-wrapper prototype (issue #747)

Spike proving an outer tmux server can wrap a nested inner tmux server —
sidebar pane pinned to a fixed width, inner session churning freely inside
it — using shell + tmux only. No Rust/Go changes. Lives at
`scripts/outer-shell/`.

## How it works

```
outer server  (-L orchard-shell, its own outer.conf, prefix C-a)
└── session "shell", 1 window, 2 panes
    ┌────────────────────┬──────────────────────────────────────┐
    │ pane 0.0 (40 cols)  │ pane 0.1                              │
    │ sidebar placeholder │ TMUX= tmux -L <inner> attach -t <sess>│
    │ watch -n1 tmux -L   │  → nested inner client, full tmux     │
    │   <inner> list-     │    prefix/keybindings/panes/windows   │
    │   windows -a        │    all working normally               │
    └────────────────────┴──────────────────────────────────────┘
```

pane 0.0 is a real tmux pane in the OUTER session — it never enters the
inner server. pane 0.1 runs a normal `tmux attach` client INTO the inner
server; from the user's perspective that pane becomes the inner session,
prefix and all. Two independent tmux servers, two independent prefixes,
one composited terminal.

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

- **Second prefix.** Outer uses `C-a`, inner keeps whatever it's configured
  with (commonly `C-b` or the project's own). Two prefixes to remember;
  outer's should stay minimal (resize hooks only, nothing else bound) so
  it's rarely reached for on purpose.
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
