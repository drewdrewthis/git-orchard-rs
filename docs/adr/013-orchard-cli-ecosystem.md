# ADR-013: Orchard CLI ecosystem & worktree-management surface

**Status:** ACCEPTED — Drew signed off on the three open questions 2026-05-07 (PR #409: rework completed via #414 rename + dispatcher to land on top; bare-verb shortcuts: yes for the worktree primary unit only; third-party plugins: deferred). Promoted to `~/.claude/adrs/` 2026-05-06. Supersedes v1 (third-binary framing) and v2 (ecosystem map). v3 incorporates the parallel `quorum` debate (`013-orchard-cli-ecosystem-quorum.md`) and `RESEARCH.md` (`013-orchard-cli-ecosystem-research.md`, 14 comparable CLIs).

**Implementation status (2026-05-07):**
- Step 1 (extract `crates/worktree-core/`) — IN PR (this PR).
- Steps 2-6 — sequenced PRs to follow.

**Related:** `~/.claude/research/019-orchard-gui-quorum.md` — the chat-first GUI direction. Both ADRs converge on "daemon owns logic; UIs (TUI + GUI) are clients of the same GraphQL surface."

**Companion docs** (live in the codex repo at `~/.claude/adrs/`, not duplicated here):
- `013-orchard-cli-ecosystem-quorum.md` — full 4-position debate
- `013-orchard-cli-ecosystem-research.md` — survey of 14 comparable CLIs

**Date:** 2026-05-05
**Author:** worktrunk-design session
**Decides:** the shape of the entire `orchard` CLI ecosystem (single binary vs multiple, dispatcher vs monolith, namespacing strategy), and where worktree-management sits within it.

---

## TL;DR

- **One user-facing binary: `orchard`.** Single name on PATH. Mode-as-subcommand (`orchard tui`, `orchard daemon start`).
- **Implementation: dispatcher + helper binaries** (git/cargo/docker pattern). `orchard` is a thin Rust dispatcher (~100 lines). It execs `orchard-tui` (Rust), `orchard-daemon` (Go), `orchard-worktree` (Rust library exposed as a binary), etc.
- **Hybrid grammar.** Namespaced subcommands for clarity (`orchard worktree new`, `orchard tmux send`, `orchard chat send`). Bare-verb shortcuts only for the primary unit (`orchard new 412` = `orchard worktree new 412`). No hyphenated compounds.
- **Worktree-core is a Rust library.** Backs `orchard-worktree` (CLI) and the TUI's create/destroy/transfer flows. Single source of truth.
- **PR #409 reworked.** Rust binary becomes `orchard-tui` (correct under the dispatcher model). New thin dispatcher takes over the `orchard` name. Go binary renamed `orchard-daemon`.

---

## Context

### Two existing binaries, both named `orchard`

**Rust** (`crates/orchard/`) — flat-dispatch CLI: `init`, `upgrade`, `setup-remote`, `heal`, `chat`, `watch`, `refresh`, `hook-enrich`, `webhook-serve`, `list-remotes`, `sessions`, `--json`, plus the TUI as default. Worktree mutation lives only inside `tui/dialogs.rs` (628 lines) backed by `git.rs` (325 lines). No scriptable mutation surface.

**Go** (`cmd/orchard/`) — Cobra hierarchy: `daemon {start,stop,status}`, `config {init,add-repo}`, `query {projects,pull-requests,issues,workflow-runs,host,panes,claude-instances,processes,claude-account,host-services,contracts,conversations}`. Read-only via GraphQL. PR #413 lands `config add-peer`.

PR #409 (in flight) renames Rust → `orchard-tui`. Currently under reconsideration because the Rust binary owns far more than the TUI.

### The deeper question

Worktree management is the proximate trigger ("should we ship `worktrunk`?"). The real question is the entrypoint shape *across* both binaries — and across whatever the future adds (webapp, more daemons, third-party plugins, AI extensions).

Drew's stated priorities, exact words:
1. *"AI doesn't have to think about how, just gets to call commands"*
2. Easy creation of tmux + worktree + claude in one shot
3. Easy destroy/prune for one or many
4. Easy move from one machine to the other
5. Worktree+session is the primary unit (memory: `feedback_primary_unit_is_worktree`)

Three messaging layers must NOT be conflated:
- `worktree` — the orchard unit (worktree+tmux+claude+linkage)
- `tmux` — raw session primitives
- `chat` — agent-to-agent

---

## What v2 got right and what it got wrong

**Right:** the ecosystem mapping (today's Rust + Go surface), surfacing the `orchard`/`orchard` collision, calling out missing ADR-011, recommending Model 3 (Rust-stays-orchard, Go-becomes-orchardd) as superior to PR #409's Model 1, identifying the architectural correctness of `crates/worktree-core/` extraction.

**Wrong:** missed the dispatcher pattern entirely. v2 framed it as "monolith vs split binaries" — exactly the dichotomy that `git`, `cargo`, `gh`, `kubectl`, and `docker compose` all *transcend* via the dispatcher model. Once v2 got challenged on "is the TUI really the business-logic owner?" (Drew: "the TUI is just a UI wrapper around the daemon and the cli, no?"), the answer was clearly no — but v2 then proposed lifting business logic into a library *behind* the TUI binary, when the better answer is to break the binary itself apart.

**Result of v3 work:** the quorum's synthesis and the research's "hybrid grammar, daemon hidden" recommendation converge on a single architecture. v3 is that.

---

## Recommendation

### 1. Architecture: one user-facing name, dispatcher under the hood

```
$ orchard                   →  exec orchard-tui (default no-arg)
$ orchard tui               →  exec orchard-tui
$ orchard worktree new 412  →  exec orchard-worktree new 412
$ orchard daemon start      →  exec orchard-daemon start
$ orchard query projects    →  exec orchard-daemon query projects (forwarded)
$ orchard tmux send foo "x" →  exec orchard-tmux send foo "x"   (or built into dispatcher)
$ orchard chat send X "..." →  exec orchard-chat send X "..."   (or built into dispatcher)

User sees: one binary `orchard`. One `--help`. One tab-completion. One install.
On disk:   `orchard`, `orchard-tui`, `orchard-daemon`, `orchard-worktree`, ...
```

### 2. The binary roster

| Binary | Language | Purpose | User-facing name |
|---|---|---|---|
| `orchard` | Rust | Thin dispatcher. ~100 lines. Routes verbs to helper binaries. | YES |
| `orchard-tui` | Rust | The TUI implementation. Renamed under PR #409 — but now correctly. | NO (dispatched) |
| `orchard-daemon` | Go | The GraphQL daemon + read queries + config. Renamed from today's `orchard`. | NO (dispatched) |
| `orchard-worktree` | Rust | Worktree mutation surface. Backed by `crates/worktree-core/` library. | NO (dispatched) |
| `orchard-chat` | Rust | Agent-to-agent messaging. Today this lives in the Rust binary as `chat` subcommand; extracts. | NO (dispatched) |
| `orchard-tmux` | Rust | Raw tmux primitives (send-keys, capture, rename, kill). Could be inline in dispatcher initially; extracts when surface grows. | NO (dispatched) |

**Amendment (2026-09-02, #747):** two more helper binaries joined the roster, both dispatched exactly like the six above (`orchard shell` → exec `orchard-shell`, `orchard upgrade` → exec `orchard-upgrade`):

| Binary | Language | Purpose | User-facing name |
|---|---|---|---|
| `orchard-shell` | Go | Outer tmux wrapper (session + sidebar pane) for `orchard shell`. | NO (dispatched) |
| `orchard-upgrade` | Go | Downloads, verifies, and installs the latest release suite. | NO (dispatched) |

`upgrade` moved out of `orchard-tui`'s flat verb set (it was a URL-printing stub there) to a real helper binary. `orchard-tui upgrade` now exits non-zero, pointing at `orchard upgrade`.

**Discovery rule:** dispatcher looks for `orchard-<verb>` in (1) the directory containing itself (bundled install), (2) `$PATH`. Found-first wins. This matches `git`'s exec-path policy.

**Bundled vs split:** for first-party plugins, `brew install orchard` installs all of them in the same `bin/` directory. The user sees one Homebrew formula and gets the full command surface. Third-party plugins land via `$PATH` (matching `kubectl` + `krew`).

### 3. Subcommand grammar (hybrid)

**Namespaced verbs (the structural backbone):**
```
orchard worktree {new, rm, prune, mv, ls, path}    [--on host] [--json] [--force]
orchard tmux     {send, capture, rename, kill, ls, mv}
orchard chat     {send, broadcast, tail}           [--target | --room]
orchard remote   {setup, ls, rm}
orchard config   {init, add-repo, add-peer, ls}
orchard query    {projects, pull-requests, issues, workflow-runs, host, panes,
                  claude-instances, processes, claude-account, host-services,
                  contracts, conversations}
orchard hook     {ingest}                          (--transcript ...)
orchard webhook  {serve, send}                     (send = stripe-style simulator)
orchard daemon   {start, stop, status, reload, logs}
orchard tui                                        (or no-arg for muscle memory)
orchard heal     [--fix] [--json]
orchard refresh
orchard watch
orchard init
orchard upgrade
orchard --json | --schema                          (legacy live state)
```

**Bare-verb shortcuts for the primary unit:**
```
orchard new <issue>     ≡  orchard worktree new <issue>
orchard rm <id>         ≡  orchard worktree rm <id>
orchard prune [filter]  ≡  orchard worktree prune [filter]
orchard mv <id> <host>  ≡  orchard worktree mv <id> <host>
orchard ls [--json]     ≡  orchard worktree ls [--json]
orchard path <id>       ≡  orchard worktree path <id>
```

**Discipline rule (codified):** *only the primary unit gets bare verbs.* If a second primary unit ever emerges (e.g., remotes become equally first-class), this rule is revisited explicitly in a new ADR. Bare verbs are a reserved namespace for whatever the project's primary unit is.

**No hyphenated compound verbs.** Position C's `tmux-send`/`daemon-start` proposal is rejected. Hyphenated compounds tab-complete worse, read worse in tool-call transcripts, and conflict with the namespaced grammar above.

### 4. Worktree-core library

Extract `crates/worktree-core/` from existing `git.rs` + the relevant parts of `tui/dialogs.rs`:

```
crates/worktree-core/
├── lib.rs
├── create.rs       # create_worktree(issue, host?) → WorktreeId
├── destroy.rs      # destroy_worktree(id, force) → ()
├── prune.rs        # prune(filter: Merged | Stale{days} | All) → Vec<WorktreeId>
├── transfer.rs     # transfer(id, target_host) → ()
├── list.rs         # list() → Vec<Worktree>
├── path.rs         # path(id) → PathBuf
└── events.rs       # emits to events.jsonl
```

**Consumers:**
- `orchard-worktree` (CLI binary) — thin clap wrapper.
- `orchard-tui` (TUI) — dialogs become *thin*: collect input → call `worktree-core` → render result. Delete duplicate logic.
- (Future) `orchard-daemon` mutations — call `worktree-core` directly (same Rust process via `cmd/` subcommand) or via FFI when daemon-mediated mutation is needed.

### 5. AI tool-call ergonomics

This is Drew's load-bearing priority. The recommendation explicitly serves it:

- **JSON everywhere.** Every command supports `--json`. Schema is versioned (`JsonOutput.version` already exists in the codebase — extend it).
- **Idempotency.** `orchard worktree new 412` is safe to re-run (returns existing if present, creates if absent). `orchard worktree rm` is safe to call on already-removed (no-op + clear exit code).
- **Stable exit codes.** 0 = success, 2 = invalid args, 3 = precondition failed (dirty worktree, etc.), 4 = remote unreachable, 5 = conflict (e.g., trying to remove a worktree mid-merge). Borrowed from agentic-CLI conventions.
- **Bare-verb shortcuts.** `orchard new 412` — minimum tokens for the AI's most-frequent intent.
- **Stable `--help` tree.** AI agents bootstrap by reading `--help`. Hierarchical structure means `orchard --help` returns groupings; `orchard worktree --help` returns the worktree surface. Discovery is deterministic and complete.

### 6. Cross-machine semantics (`--on <host>`)

`orchard worktree new 412 --on ubuntu` SSHs and runs `orchard worktree new 412` on the remote. This is *the same dispatcher pattern* applied across the network: the local dispatcher recognizes `--on` and shells out to `ssh ubuntu orchard worktree new 412`. The remote machine has the same orchard install; it executes locally there. This means:

- Federation requires the same `orchard` version on remote (drift detection via version handshake).
- `transfer` (`orchard worktree mv 412 ubuntu`) is *not* a primitive of `worktree-core`; it's a coordinator: snapshot locally, ssh-and-create remotely, sync state, kill local. Lives in `orchard-worktree` binary, not in the library.

This avoids embedding remote-awareness inside `worktree-core`, keeping the library local-first and pure-ish.

---

## Why this beats every pure position

(Detail in `/tmp/worktrunk-design/QUORUM.md`. Headlines:)

- **Pure A (monolith)** — keeps the 14-arm `match` in `main.rs` indefinitely, blocks third-party plugins, requires re-linking the whole binary to ship any change. Dispatcher gives the same single-name UX without paying that cost.
- **Pure B (`orchard` + `orchardd`)** — forces users to remember the partition on every command, requires two tab-completions, two manpages, two install lines. Synthesis hides the partition behind one name while preserving B's honest-named-process virtue (`top` shows `orchard-daemon`, not `orchard`).
- **Pure C (flat verbs everywhere)** — collision risk grows with surface. Bare-verb shortcuts capture C's win for the primary unit *without* paying its cost as the surface grows.
- **Pure D (open ecosystem)** — premature commitment to third-party extensibility as a *product goal*. Synthesis adopts D as *implementation* without committing to D as *ecosystem strategy*. Door stays open, cost is zero today.

---

## What the research said (digest)

(Full report: `/tmp/worktrunk-design/RESEARCH.md`, 14 tools surveyed.)

**Convergent patterns across `git`, `cargo`, `gh`, `kubectl`, `docker`, `flyctl`, `stripe`, `glab`, `wrangler`, `mise`:**
1. **One user-facing binary.** Even tools with daemons (`docker`/`dockerd`, `wrangler`/dev-server) ship as one user CLI; the daemon is hidden or invoked through the CLI.
2. **Hybrid grammar.** Pure noun-verb (`gh pr create`) loses to pure flat (`fly deploy`) on AI ergonomics; pure flat loses to noun-verb on discoverability. Successful CLIs use hybrid: namespaces for clarity, bare-verb shortcuts for the most-frequent unit.
3. **Plugin model: defer.** Even `git` and `kubectl` (the canonical plugin CLIs) keep first-party verbs as built-ins. Third-party plugins are a sidecar, not the main shape. Don't ship plugin support before there's something to install.
4. **JSON contract as a hard requirement.** Every modern CLI surveyed has `--json`. Stripe and `gh` are explicitly designed for AI/CI consumption. Versioned schemas are standard.
5. **The TUI is a client of the same library, not the home of the logic.** `wrangler dev`, `docker compose up`, `flyctl logs --tui` — all rich UIs that wrap the same core the headless commands use.

**Specific lessons that shaped this ADR:**
- `git`'s PATH dispatch is the proven pattern at scale (200+ commands, decades, polyglot). Adopt the *mechanism* without the *ecosystem commitment*.
- `docker compose` v2's transition from standalone `docker-compose` to `docker compose` subcommand confirms users prefer one binary over two-with-the-same-purpose, even at the cost of a transition.
- `gh` retrofitted AI ergonomics onto a noun-verb hierarchy and the AI integrations work fine. Two-level grouping isn't a tax on AI; flat-only isn't a free win.

---

## Sequencing

1. **Settle this ADR (Drew approves or redirects).** Three open questions below block step 2.
2. **Write the missing ADR-011** capturing single-binary-with-helpers pattern. Referenced in `cmd/orchard/main.go` but not on disk; this work delivers it.
3. **Extract `crates/worktree-core/`** from `git.rs` + dialog logic. Behavior-preserving refactor PR. Tests on the library. ~2-4 days.
4. **Build the dispatcher binary** (~100 lines Rust). Routes to `orchard-tui`, `orchard-daemon`, `orchard-worktree`, etc. Resolves bare-verb shortcuts to namespaced equivalents.
5. **Rework PR #409.** Rename today's Rust binary to `orchard-tui` (correct under dispatcher model); rename today's Go binary to `orchard-daemon`; ship the new dispatcher at the `orchard` name. Coordinate via release notes + symlink shim for one minor version.
6. **Add `orchard-worktree` binary** with `{new, rm, prune, mv, ls, path}` subcommands as thin wrappers on `worktree-core`. Wire into dispatcher.
7. **Migrate TUI dialogs** to call `worktree-core` directly. Delete duplicated mutation logic in `tui/dialogs.rs`.
8. **Restructure remaining flat verbs** (`heal`, `refresh`, `watch`, `chat`, `sessions`, `setup-remote`, `webhook-serve`, `hook-enrich`) into namespaced grammar. Backwards-compat aliases for one minor version.
9. **(Later)** Federated mutation through `orchard-daemon` if a forcing function emerges (e.g., consistency requirements for cross-machine transfer).

---

## What this ADR decides

1. **One user-facing binary: `orchard`.** Dispatcher pattern under the hood (git/kubectl precedent).
2. **Worktree-core is a Rust library.** Backs `orchard-worktree` binary + TUI dialogs.
3. **Hybrid grammar.** Namespaced subcommands + bare-verb shortcuts for the primary unit only.
4. **TUI is a client, not a logic owner.** Renamed `orchard-tui`, dispatched.
5. **No new top-level binary named `worktrunk`.** Worktree management is `orchard worktree …` (with `orchard new` shortcut).
6. **Plugin model: deferred but architecture-compatible.** Door stays open. Zero today-cost.
7. **PR #409 reworked**, not abandoned. The rename it proposes is correct *given* the dispatcher; we add the dispatcher as the missing piece.

## What this ADR defers

- Third-party plugin ecosystem as a product commitment (architecture supports it; product decision separate).
- ADR-011 contents (this ADR could *be* ADR-011, or sit at 012 with 011 still owed for the dispatcher pattern itself).
- Daemon-mediated mutation (synthesis Model 2 from v2).
- Per-repo create-hooks for `worktree new`.
- Shell completion + manpages — mechanical, follows the binary decision.

---

## Open questions for Drew

1. **PR #409 — rework or abandon?** Synthesis says rework: rename Rust binary to `orchard-tui` *and* introduce the new dispatcher binary at the `orchard` name. Today's PR only does step 1. Confirm direction?

2. **Bare-verb shortcuts — yes or no?** Synthesis recommends `orchard new 412` resolves to `orchard worktree new 412`. Saves AI tokens, matches "primary unit" model. But some engineers find magic resolution surprising — `orchard new` is ambiguous if a second domain ever needs creation. Ship the shortcuts now (with the discipline rule), or stay namespaced-only?

3. **Third-party extensibility — yes, no, or "later"?** Architecture supports it. Should the ADR explicitly say "third-party plugins are a non-goal for now" (prevent scope creep), or "we leave the door open" (invite contributions if/when ecosystem grows)?

These are the load-bearing decisions. Once answered, sequencing executes.

---

## Companion docs

- `/tmp/worktrunk-design/QUORUM.md` — full debate of all 4 positions (A: monolith, B: two binaries, C: flat verbs, D: dispatcher), with synthesis.
- `/tmp/worktrunk-design/RESEARCH.md` — survey of 14 comparable CLIs (`git`, `cargo`, `gh`, `kubectl`, `docker`, `tmuxinator`, `mise`, `flyctl`, `wrangler`, `direnv`, `stripe-cli`, `glab`, `task`, `goose`) with cross-tool patterns and orchard-specific lessons.
