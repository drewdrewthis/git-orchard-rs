---
id: plan.747-outer-shell-product
kind: plan
date: 2026-09-02
keywords: [outer-shell, orchard-shell, packaging, release, upgrade, install, sweatshop, tmux-wrapper, versioning, doctor]
links: { plans: [], decisions: [], research: [] }
status: active
---

# 747 — Outer shell: from spike to shipped product

Issue [#747](https://github.com/drewdrewthis/orchardist/issues/747) · PR [#748](https://github.com/drewdrewthis/orchardist/pull/748) · branch `issue747/outer-tmux-wrapper`

## 1. Goal

Ship `orchard shell` as an installable, versioned, self-upgrading product — one
command that boots the outer tmux wrapper with the sidebar, installable on the
sweatshop box (`ubuntu@10.0.3.222`) via a curl-able installer and upgradable in
place, with a `doctor` that proves the install is sound.

## 2. Non-goals

- **No daemon schema changes.** `switchClient` / `renameSession` / `killSession` /
  `launchSession.command` stay as issues [#726](https://github.com/drewdrewthis/orchardist/issues/726)
  and [#751](https://github.com/drewdrewthis/orchardist/issues/751). The tmux
  shims documented in `docs/outer-shell-prototype.md` ("Routing orchard-sidebar's
  own tmux execs") remain as-is. Any feature needing a new mutation is out.
- **No GUI / orchard-gui work.** Different sidebar entirely (see §7 naming note).
- **No Homebrew tap, no apt/deb, no Docker image.** One installer + one upgrade
  path. Package-manager distribution is a later decision.
- **No Windows.** The whole thing is tmux-shaped.
- **No rewrite of `orchard-sidebar`'s data layer.** It keeps its current GraphQL
  fast/slow lanes.
- **No change to the npm `git-orchard` package's contract.** `orchard-<triple>.tar.gz`
  stays reserved for the dispatcher alone (`npm/install.js` hardcodes that name).

## 3. Decisions (made, not open)

### D1 — `orchard shell` is a Go binary (`cmd/orchard-shell`), not bash

`scripts/outer-shell/launch.sh` is replaced by `cmd/orchard-shell`, dispatched as
`orchard shell` per ADR-013. `outer.conf` stays a real file in the repo (source of
truth, still driven by `verify.sh`) and is additionally `go:embed`-ed into the
binary; at boot the binary materialises it to
`${XDG_STATE_HOME:-~/.local/state}/orchard/outer-<sha256[:12]>.conf` and passes
`-f` to every outer-socket tmux invocation. Content-hashed filename so an upgrade
can never re-use a stale conf.

**Why, against the constitution:**

- **RULES L1/L2/L2c do not bind here.** L1 governs *external-world operations that
  both the daemon and the CLI invoke* — the point is one implementation, two
  wrappers. `orchard shell` has exactly one consumer (a human's terminal), is never
  exec'd by the daemon, and returns no data. L2's `{ok,data,error}` envelope is
  meaningless for a process that `exec`s into `tmux attach` and never returns.
  Entrypoints in this repo are binaries (`orchard-tui`, `orchard-daemon`), not
  scripts; the leaf ops under `scripts/tmux/`, `scripts/git/` are the L1 class.
- **ADR-013 gives it for free.** The dispatcher resolves `orchard-<verb>` *next to
  the running `orchard` executable first, then `$PATH`*
  (`crates/orchard-dispatcher/src/main.rs::resolve_helper`). That is exactly the
  problem `launch.sh` hand-rolls today (`$REPO_ROOT/bin/orchard-sidebar` else
  `command -v`, with the stale-PATH hazard called out in its own comments).
  `os.Executable()` + sibling lookup makes "find *my* sidebar" a two-line function
  with no PATH ambiguity.
- **Packaging requires it.** Bash cannot be `--version`-stamped by `-ldflags`,
  cannot be shipped as one checksum-verified artifact with its config, and cannot
  hold the update-check goroutine. Every §4/§5 requirement lands on a binary.
- **ADR-016/017/018 are unaffected.** The wrapper's tmux calls are outer-server
  operations against a server the daemon does not model at all. Moving them from
  bash to Go changes no protocol boundary; the ADR-016 debt named in
  `docs/outer-shell-prototype.md` §"Next steps" stays open and unchanged.

**Tradeoff accepted:** a config change now needs a rebuild for the embedded copy
(mitigated: `--conf <path>` flag overrides the embedded one, and `verify.sh`
drives the repo file directly). Bash's edit-and-rerun loop is lost; correctness and
distributability are bought.

### D2 — one version for the whole suite, riding the dispatcher's tag

`release-please-config.json` keeps a single package (`crates/orchard`). The Go
binaries do **not** get their own release-please entries. The release tag is the
single source: the build job derives `VERSION=${tag##*v}` and passes it to
`make`, which bakes it via `-ldflags "-X main.version=$(VERSION)"` into every Go
binary (the pattern `cmd/orchard-daemon/main.go:27` already uses).

**Why not separate packages:** three independent semvers make
`doctor`'s "binaries are version-matched" check unstateable, and a user reading
`orchard --version` would have no idea which daemon/shell they have. **Tradeoff:**
a dispatcher-only change ships a byte-identical-except-version daemon. Go builds
are seconds; this is the cheap side of the trade.

### D3 — `orchard upgrade` gets a real implementation in a new `cmd/orchard-upgrade`

Not `orchard-shell upgrade`. `orchard upgrade` already exists as a user-facing verb
routed to `orchard-tui` (`crates/orchard-dispatcher/src/main.rs:62` `TUI_VERBS`),
where it is a stub that prints a URL (`crates/orchard/src/main.rs:209`
`handle_upgrade`). Upgrading the suite is not a shell concern and not a TUI
concern. The verb moves from `TUI_VERBS` to `NAMESPACE_VERBS` and resolves to a
new Go binary. Shared logic (GitHub releases client, semver compare, checksum
verify, atomic replace, update-check cache) lives in `internal/release/`, consumed
by both `orchard-upgrade` and `orchard-shell`'s background check.

**Tradeoff:** `scripts/install.sh` (bash, pre-install bootstrap) duplicates
"resolve latest release + verify sha256" in a second language. Accepted: the
installer must run on a box with no orchard binary at all, so it cannot reuse Go
code. The two are ~30 lines of overlap and are pinned by separate tests.

### D4 — cross-compilation is pure Go

Verified this turn: `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./cmd/orchard-daemon`
and `./cmd/orchard-sidebar` both succeed from darwin/arm64. All four
OS×arch combinations therefore build on a single `ubuntu-latest` runner — no
cross toolchain, no matrix of runners for the Go job.

### D5 — sweatshop arch is verified, not presumed

Step 0 runs `ssh sweatshop uname -m` (read-only). The plan assumes `x86_64`; if it
returns `aarch64` nothing in the design changes (both are built) — only the
verification commands in §8 change their expected triple.

## 4. Files to change

| File | Change |
|---|---|
| `cmd/orchard-shell/main.go` *(new)* | Entry: flags, boot, reattach, exec into attach |
| `cmd/orchard-shell/outer.go` *(new)* | Outer-server lifecycle: new-session, split, send-keys, env wiring — the port of `scripts/outer-shell/launch.sh` |
| `cmd/orchard-shell/conf.go` *(new)* | `go:embed outer.conf`, content-hashed materialisation, `--conf` override |
| `cmd/orchard-shell/doctor.go` *(new)* | `orchard shell doctor [--json]` |
| `cmd/orchard-shell/outer.conf` *(new, symlink-free copy)* | `go:embed` cannot reach `../../scripts/`; the canonical file moves here and `scripts/outer-shell/` keeps only `verify.sh`, pointed at the new path |
| `cmd/orchard-upgrade/main.go` *(new)* | `orchard upgrade [--check] [--version vX.Y.Z]` |
| `internal/release/release.go` *(new)* | GitHub releases API client, `ORCHARD_RELEASE_REPO` override |
| `internal/release/verify.go` *(new)* | SHA256SUMS parse + verify, atomic same-filesystem replace |
| `internal/release/check.go` *(new)* | 24h-cached update check → `${XDG_STATE_HOME}/orchard/update-check.json` |
| `cmd/orchard-daemon/main.go:27` | unchanged (`var version = "dev"` is the pattern the others copy) |
| `cmd/orchard-sidebar/main.go:278` | arg parse gains `--version`; header gains the update indicator |
| `cmd/orchard-sidebar/view.go`, `model.go`, `format.go` | §7 features (filter, badge, jump, persisted collapse) |
| `crates/orchard-dispatcher/src/main.rs:49` | `NAMESPACE_VERBS` += `"shell"`, `"upgrade"` |
| `crates/orchard-dispatcher/src/main.rs:62` | `TUI_VERBS` -= `"upgrade"` |
| `crates/orchard-dispatcher/src/main.rs:76` | `HELP` documents both |
| `crates/orchard-dispatcher/src/main.rs` (tests ~266-330) | `namespace_verbs_match_adr`, `tui_verbs_cover_orchard_tui_argv_parser` assert exact lists — both must be updated in the same commit |
| `crates/orchard/src/main.rs:160,209` | delete `handle_upgrade` + its match arm; `orchard-tui upgrade` no longer exists |
| `docs/adr/013-orchard-cli-ecosystem.md` | amendment note: `shell` + `upgrade` join the helper-binary set |
| `Makefile` | `VERSION` plumbing; `sidebar`, `shell`, `upgrade-cli`, `install-sidebar`, `install-shell`, `install-upgrade-cli`, `dist` targets |
| `.github/workflows/release-please.yml` | new `build-go` job + `checksums` job |
| `.github/workflows/ci.yml` | `go build ./cmd/...` + `go test ./cmd/orchard-shell/... ./internal/release/...`; version-parity check |
| `scripts/install.sh` *(new)* | curl-able installer, `--json` envelope on stdout |
| `scripts/install.bats` *(new)* | L2 envelope + arch-detection tests |
| `scripts/init/orchard.service` | `ExecStart` becomes a placeholder the installer rewrites to the resolved path |
| `scripts/outer-shell/launch.sh` | **deleted** (replaced by the binary) |
| `scripts/outer-shell/verify.sh` | drive `bin/orchard-shell` instead of `launch.sh`; `--conf` at the new path |
| `docs/install.md` *(new)* | install/upgrade/sweatshop runbook |
| `docs/outer-shell-prototype.md` | retitled to `docs/outer-shell.md`, spike framing removed, `launch.sh` references repointed |
| `specs/features/outer-shell-*.feature` *(new, 3 files)* | §6 |
| `npm/install.js` | **unchanged** — asset name contract preserved |

## 5. Step sequence

Each step is one coder dispatch. Steps 1-4 are the spine; 5-8 packaging; 9-13 product.

### Step 0 — verify sweatshop arch (operator, read-only, 30s)
`ssh sweatshop uname -m && ssh sweatshop 'tmux -V; systemctl --user is-active orchard.service; command -v orchard'`
Record the output in the PR body. No mutation. Blocks nothing; informs §8.

### Step 1 — `internal/release/`: the shared release client
Pure-Go, no binary wiring yet. `LatestRelease(ctx, repo)` and `ReleaseByTag`;
`ORCHARD_RELEASE_REPO` (default `drewdrewthis/orchardist`); semver compare that
treats `dev` as "always older"; `SHA256SUMS` parser; `VerifyAndReplace(path, r,
wantSum)` writing to `path + ".new"` on the same filesystem then `os.Rename`.
Tests against an `httptest.Server` — no network in unit tests (RULES T1).

### Step 2 — `cmd/orchard-shell`: port `launch.sh`, embed `outer.conf`
Move `scripts/outer-shell/outer.conf` → `cmd/orchard-shell/outer.conf` (git mv, byte-identical).
Flags: `--inner-socket` (default `default`), `--session` (default: the inner
server's most-recently-attached session), `--width` (default 40), `--outer-socket`
(default `orchard-shell`), `--conf`, `--detach`, `--version`, plus the `doctor`
subcommand stub. Every behaviour `launch.sh` encodes is preserved verbatim and
each carries its comment forward: split-before-send-keys ordering; `TMUX=` before
the inner attach; `ORCHARD_TMUX_SOCKET` / `ORCHARD_TMUX_CLIENT` (from 0.1's
`#{pane_tty}`, resolved *after* the attach send) / `ORCHARD_OUTER_PANE` (from
`#{pane_id}`); `-f` on every outer invocation; `select-pane -t 0.1` unconditionally
before the final attach. Sidebar discovery: sibling-of-self → `$PATH` → `watch(1)`
placeholder. `--detach` boots the outer session and exits without attaching.
Delete `launch.sh`; repoint `verify.sh` at `bin/orchard-shell` and re-run the full
battery — **`verify.sh` passing unchanged is the acceptance gate for this step.**

### Step 3 — reattach instead of erroring (§7 feature #1)
Re-running `orchard shell` while an outer session exists already attaches
(launch.sh's idempotent path). Harden: if the outer session exists but its pane 0.1
inner client is dead (inner server restarted), respawn 0.1 rather than attaching to
a corpse. If the requested `--session` is absent from the inner server, list what
*is* there and exit 2 with that list, instead of the current bare error. If no
inner server exists at all, say so and print the `orchard new` hint.

### Step 4 — dispatcher wiring
`NAMESPACE_VERBS` += `"shell"`, `"upgrade"`; `TUI_VERBS` -= `"upgrade"`; `HELP`
updated; the three exact-list tests updated; `handle_upgrade` and its match arm
deleted from `crates/orchard/src/main.rs`; ADR-013 amendment note appended.

### Step 5 — VERSION plumbing + `--version` everywhere
`var version = "dev"` in `cmd/orchard-shell`, `cmd/orchard-sidebar`,
`cmd/orchard-upgrade` (daemon already has it). Makefile: `sidebar`, `shell`,
`upgrade-cli` targets each `go build -ldflags "-X main.version=$(VERSION)"` into
`bin/`; `install-sidebar`, `install-shell`, `install-upgrade-cli`; `install` gains
all three. Port `cmd/orchard-daemon/version_test.go` to the new binaries (same two
scenarios: ldflags-injected semver, and `dev` without).

### Step 6 — `make dist` + release workflow Go job
`make dist` builds all four OS×arch combos (`CGO_ENABLED=0`) of the three Go
binaries into `dist/`, tars each as `<pkg>-<rust-triple>.tar.gz` (the *rust* triple
naming, so every asset on the release reads the same way), and emits `SHA256SUMS`.
Workflow: a `build-go` job gated on the same `paths_released` condition, running on
one `ubuntu-latest`, deriving `VERSION` from `crates/orchard--tag_name`, calling
`make dist VERSION=$V`, uploading every tarball plus per-asset `.sha256` (existing
convention) plus the aggregate `SHA256SUMS` to the same tag. Also emits
`orchard-suite-<triple>.tar.gz` containing all six binaries + `share/orchard/scripts/`
+ `orchard.service` — the artifact `install.sh` prefers. `orchard-<triple>.tar.gz`
remains the dispatcher-only asset `npm/install.js` expects.

### Step 7 — `scripts/install.sh`
POSIX-ish bash, curl-able. Detects OS/arch → rust triple; resolves the release
(`--version vX.Y.Z` else latest); downloads `orchard-suite-<triple>.tar.gz` +
`SHA256SUMS`; verifies; installs to `--prefix` (default: `~/.local/bin` if writable
and on `$PATH`, else `/usr/local/bin` if writable, else `~/.local/bin` with a PATH
warning); idempotent (re-running the same version is a no-op that still exits 0);
`--from-source` builds via `make all sidebar shell upgrade-cli` when the repo is
present; on Linux, installs `orchard.service` with `ExecStart` rewritten to the
resolved absolute `orchard` path and prints (does not run) the
`systemctl --user enable --now` line. `--json` prints the L2 envelope on stdout,
progress on stderr. `scripts/install.bats` covers: arch detection table, checksum
mismatch → `ok:false` + non-zero exit, idempotent re-run, `--prefix` honoured.

### Step 8 — `cmd/orchard-upgrade`
`orchard upgrade` — resolve latest, compare against the running `orchard --version`,
download the suite tarball, verify against `SHA256SUMS`, atomically replace each
binary found beside the running `orchard`. `--check` exits 0 and prints
current/latest without writing anything. `--version vX.Y.Z` pins (including
downgrade). Refuses and explains when the install directory is not writable, when
a target binary is on a different filesystem than its temp file, and when the
running process *is* one of the binaries being replaced (rename-over-running is
fine on Linux/macOS — the check is for the directory, not the inode). Honours
`ORCHARD_RELEASE_REPO`.

### Step 9 — `orchard shell doctor`
`orchard shell doctor [--json]`, L2 envelope. Checks, each pass/warn/fail with a
one-line remedy: `tmux -V` ≥ 3.2; `$TMUX` set at invocation (warn — you are already
inside tmux); daemon reachable (`POST {__typename}` to the configured endpoint,
default `127.0.0.1:7777`, 2s timeout); all six binaries resolvable and reporting
the *same* version (`dev` counts as a warn, a mismatch is a fail); inner socket
exists under `$TMUX_TMPDIR`/`/tmp/tmux-$(id -u)`; outer socket state; install dir
on `$PATH`; on Linux, `systemctl --user is-enabled orchard.service`. Non-zero exit
if any check fails.

### Step 10 — update indicator, network-free in the sidebar
`orchard-shell` at boot spawns one goroutine: if
`${XDG_STATE_HOME:-~/.local/state}/orchard/update-check.json` is older than 24h
and `ORCHARD_NO_UPDATE_CHECK` is unset, refresh it (`{checked_at, current, latest}`).
The sidebar **only reads that file** — never the network — and renders `⇧ v1.3.0`
in its header when `latest > current`. Clicking it is not a hit target (it would
mean running an upgrade under the user's live session); the remedy is text: the
hint line shows `orchard upgrade`.

### Step 11 — sidebar features A (§7 #2, #4)
Persisted collapse+width: `${XDG_STATE_HOME}/orchard/sidebar-ui.json`
`{collapsed, width}`, written on change (debounced), read by `orchard-shell` at
boot so the pane is *created* at the remembered width — not resized after paint.
Needs-attention badge: the count of the "Needs attention" bucket
(`rowBucket` in `model.go`) rendered in the header; `--bell` (off by default) emits
one terminal bell when that count transitions upward.

### Step 12 — sidebar features B (§7 #3, #5)
`/` opens a filter line in the footer; typing filters rows by session name, mission
and directory (case-insensitive substring); `Esc` clears; the filter never changes
the selection's *identity*, only visibility, and an empty result set shows "no
match" rather than an empty band. `M-1`..`M-9` in `outer.conf` send an index-select
to pane 0.0, jumping to the nth *visible* row and switching to it — the same code
path as Enter, hand-back included.

### Step 13 — docs + specs
`docs/install.md` (install, upgrade, doctor, the sweatshop runbook);
`docs/outer-shell-prototype.md` → `docs/outer-shell.md`, spike framing removed,
`launch.sh` references repointed, ADR-016 debt paragraph retained verbatim;
the three feature files from §6; PR #748 body rewritten; README gains a one-line
install stanza.

## 6. Specs

Three files, matching the repo's Gherkin convention (`Feature:` + `As a/I want/So
that` + `Background:` + tagged `Scenario:`). **Naming:** `sidebar-v1-hardening-*`
is the *orchard-gui* sidebar — a different component. These are `outer-shell-*`,
and no scenario title collides.

- `specs/features/outer-shell-wrapper.feature` — sidebar width invariant across
  inner churn and outer resize; `TMUX=` clearing (attach refuses without it);
  popup renders over the inner pane; no outer prefix swallowing; reattach on
  re-run; missing-session error lists what exists.
- `specs/features/outer-shell-install-upgrade.feature` — fresh install to a clean
  box; idempotent re-run; checksum mismatch aborts and leaves the old binaries
  untouched; `--version` pin; `upgrade --check` mutates nothing; version-matched
  binaries after upgrade.
- `specs/features/outer-shell-doctor.feature` — each check's pass and fail shape;
  `--json` envelope; non-zero exit on any fail.

`specs/features/` is **not** covered by `make check-feature-parity` (that gates
`daemon/features/`, `crates/orchard/features/`, `crates/orchard-gui/features/`), so
there is no CI parity obligation — but annotate the Go tests
`// @scenario <verbatim title>` anyway, per T8's intent.

## 7. Smart features — ranked, top 5 included

Included (steps 3, 11, 12):

| # | Feature | Value | Cost | Step |
|---|---|---|---|---|
| 1 | Reattach + honest missing-session error instead of a bare failure | Highest — it is the first thing a second `orchard shell` does | Low | 3 |
| 2 | Persist collapsed state + width across restarts | High — the wrapper is a daily-driver; re-collapsing every boot is a papercut | Low | 11 |
| 3 | `/` filter in the sidebar list | High — the list is the product, and at 30 sessions it needs one | Medium | 12 |
| 4 | Needs-attention count badge (+ opt-in `--bell`) | High — "what needs me?" is the sidebar's stated single question | Low | 11 |
| 5 | `M-1`..`M-9` jump to the nth session | Medium-high — muscle memory, and free given the existing `M-Up`/`M-Down` forwarding | Low | 12 |

Deferred, ranked, with reasons:

6. **Auto-collapse when the inner pane is narrow** — good idea, but it fights the
   persisted-collapse state from #2 and needs a hysteresis rule to avoid flapping
   on every resize. Do it after #2 has settled.
7. **Double-click a card to zoom the inner pane** — cheap, but `resize-pane -Z` on
   the outer server while a card click also drives a `switch-client` on the inner
   one is two servers mutating in one gesture; wants its own verify.sh check.
8. **`--theme light/dark`** — **rejected.** `format.go` already uses
   `lipgloss.AdaptiveColor`, which resolves against the terminal's own background.
   A flag would add a second, conflicting source of truth for zero gain.
9. **Hover tooltips** — impossible. tmux delivers no hover events, only clicks,
   drags and wheel. Skip permanently.

Every item above is client-and-wrapper-local. Anything wanting `renameSession`,
`killSession` or `launchSession.command` is out of scope by §2 and belongs to
#726/#751.

## 8. Ordering, commits, and the live drive

**Commits on `issue747/outer-tmux-wrapper`,** one per step, conventional-commit
scoped so release-please reads them correctly (`feat(shell):`, `feat(release):`,
`fix(sidebar):`, `docs(747):`):

```
0  (no commit — operator probe, recorded in the PR body)
1  feat(release): shared GitHub release client, checksum verify, atomic replace
2  feat(shell): orchard-shell binary replaces launch.sh, embeds outer.conf
3  feat(shell): reattach to a live outer session; name what inner sessions exist
4  feat(cli): dispatch `orchard shell` and `orchard upgrade` to helper binaries
5  feat(build): bake VERSION into every Go binary; --version on all three
6  feat(release): build and publish Go binaries for 4 targets with SHA256SUMS
7  feat(install): curl-able scripts/install.sh with checksum verification
8  feat(upgrade): orchard upgrade downloads, verifies and replaces the suite
9  feat(shell): orchard shell doctor
10 feat(sidebar): show an update indicator from the cached daily check
11 feat(sidebar): persist collapse/width; needs-attention badge and opt-in bell
12 feat(sidebar): / filter and M-1..9 jump-to-session
13 docs(747): install runbook, outer-shell doc, outer-shell feature specs
```

Steps 1-2 are the only hard serialisation (2 depends on 1 only for the shared
module's existence, and can start before 1 lands if `internal/release` is stubbed).
5 must precede 6. 7 and 8 both depend on 6's asset names. 10 depends on 1. 11-12
are independent of everything after 3 and can run in parallel with 6-9 — but per
`feedback_agent_sequencing`, 11 and 12 touch the same sidebar files and must be
serialised against each other.

**PR #748:** body rewritten at step 13 to lead with what `orchard shell` *is*, the
install one-liner, the §7 feature list, and the §0 probe output. Mark ready only
after CI is green **and** the live drive below passes — never on a pending run.

**Operator live drive (the finished-product gate).** On the sweatshop box, from a
clean state, with the release published:

1. `ssh sweatshop 'curl -fsSL https://raw.githubusercontent.com/drewdrewthis/orchardist/main/scripts/install.sh | bash'` → exits 0.
2. `ssh sweatshop 'orchard --version && orchard shell --version && orchard-daemon --version'` → three identical semvers.
3. `ssh sweatshop 'systemctl --user enable --now orchard.service && sleep 2 && systemctl --user is-active orchard.service'` → `active`.
4. `ssh sweatshop 'orchard shell doctor --json'` → `{"ok":true,...}`, every check pass.
5. Interactive: `ssh -t sweatshop orchard shell` → sidebar renders on the left,
   keyboard lands in the inner pane, a card click switches the inner session and
   hands focus back, `M-s` collapses, `/` filters, `M-3` jumps, `Ctrl-A` reaches
   the inner shell's readline unharmed.
6. Detach, re-run `orchard shell` → reattaches to the same outer session; the
   sidebar comes back at its remembered width/collapse state.
7. Publish `vX.Y.Z+1`; `ssh sweatshop 'orchard upgrade --check'` → reports the new
   version, changes nothing; `orchard upgrade` → all six binaries replaced,
   `doctor` still green, the running outer session survives.

## 9. Alternatives considered

- **Keep `launch.sh` and ship it inside a tarball.** Cheapest, and preserves the
  edit-and-rerun loop. Rejected: no `--version`, no self-location, no update check,
  and `orchard shell` would have to be a binary *anyway* just to find the script —
  at which point the script is a liability, not a saving.
- **Make `orchard shell` a Rust helper alongside the dispatcher.** Consistent with
  the dispatcher's language and would ride the existing release matrix untouched.
  Rejected: it must co-design with `cmd/orchard-sidebar` (Go) — same env contract,
  same state files, same version stamp — and the release client is shared with
  `orchard-upgrade`, which must run where the daemon runs. A second Go binary is
  cheaper than a Rust/Go seam through the middle of one feature.
- **Separate release-please packages per Go binary.** Rejected in D2.
- **One fat `orchard` binary (drop the dispatcher).** Rejected: ADR-013 is
  ACCEPTED and the dispatcher's sibling-resolution is load-bearing for D1.
- **Homebrew tap / apt repo instead of `install.sh`.** Better long-term UX, much
  more moving parts, and neither covers the sweatshop box's actual shape today
  (SSH + systemd user units, no root wanted). Revisit after the installer proves
  the artifact layout.
- **Vendoring `outer.conf` to `/usr/local/share/orchard/`** (the pattern
  `install-scripts` already uses). Rejected: it makes the binary's behaviour depend
  on a second file that upgrade must keep in lockstep — the exact drift `go:embed`
  removes. The `--conf` flag preserves the override case.

## 10. Risks

- **One-way door: the `orchard-<triple>.tar.gz` asset name.** `npm/install.js`
  hardcodes it. Reusing it for the suite bundle silently breaks every `npm i -g
  git-orchard`. Mitigated by the distinct `orchard-suite-` prefix; a CI assertion
  that the dispatcher asset still exists on the release belongs in step 6.
- **One-way door: deleting `handle_upgrade`.** `orchard upgrade` changes from
  "prints a URL" to "replaces your binaries". A user with a stale dispatcher and a
  new `orchard-upgrade` on PATH gets the new behaviour from an old CLI. Acceptable
  (the new behaviour is strictly better) but it must be in the release notes.
- **`ProtectSystem=strict` in `orchard.service`** blocks writes outside the
  declared `ReadWritePaths`. If `orchard upgrade` is ever invoked *from* the unit's
  context it will fail confusingly. Doctor should note that upgrade is a shell-side
  operation.
- **Version skew mid-upgrade.** Replacing six binaries is not atomic as a set; a
  crash between renames leaves a mixed install. Mitigation: replace into a staging
  dir, verify every file, then rename in dependency order (helpers first,
  `orchard` last) so a half-finished upgrade still has a working dispatcher
  pointing at consistent helpers. Doctor's version-match check is the detector.
- **`go:embed` + `verify.sh` drift.** The canonical `outer.conf` moves under
  `cmd/orchard-shell/`; a stale copy left at `scripts/outer-shell/` would be edited
  by someone and silently ignored. Step 2 must `git mv`, not copy.
- **The reattach path can attach to a dead inner client** (step 3's whole subject).
  Getting this wrong presents as "the right pane is a dead shell" — the exact
  failure `docs/outer-shell-prototype.md` §"The one place `TMUX=` is needed"
  describes, and easy to mistake for the `TMUX=` bug when triaging.
- **Rate limits on unauthenticated GitHub API** (60/hr/IP) for the update check. The
  24h cache plus `ORCHARD_NO_UPDATE_CHECK` covers it; a failed check must be silent
  in the sidebar, never an error banner.
- **Scope.** Thirteen steps is a large PR. If it needs splitting, the cut line is
  after step 9: steps 1-9 are "installable and upgradable", 10-13 are "nicer".

## 11. AC draft

<!-- ACs ready for ac-reviewer -->

**AC1 — `orchard shell` is a dispatched Go binary that boots the wrapper.**
- `make shell` produces `bin/orchard-shell`; `bin/orchard-shell --version` prints the `VERSION` passed to make, and `dev` when none was.
- With `orchard` and `orchard-shell` in the same directory, `orchard shell --version` prints the same string as `orchard-shell --version`.
- `scripts/outer-shell/verify.sh`, repointed at `bin/orchard-shell`, passes every check it passed against `launch.sh` — including "outer prefix does not swallow keystrokes", the wheel-forwarding checks, and the popup-render check.
- `scripts/outer-shell/launch.sh` does not exist; `grep -rn "outer-shell/launch.sh" .` returns no hits outside `docs/plans/` and git history.
- `cmd/orchard-shell/outer.conf` is the only `outer.conf` in the tree (`find . -name outer.conf -not -path './.git/*'` returns exactly one path).
- Booting with `--conf /tmp/other.conf` uses that file: `tmux -L <outer> show-options -g` reflects a value set only in `/tmp/other.conf`.
- With no `--conf`, the materialised path under `${XDG_STATE_HOME}/orchard/` has a name containing the first 12 hex chars of the embedded conf's sha256, and its content is byte-identical to `cmd/orchard-shell/outer.conf`.

**AC2 — the wrapper's env contract is preserved exactly.**
- After boot, `tmux -L <outer> -f <conf> display -p -t shell:0.0 '#{pane_start_command}'` contains `ORCHARD_TMUX_SOCKET=<inner>`, `ORCHARD_TMUX_CLIENT=<0.1's pane_tty>` and `ORCHARD_OUTER_PANE=<0.1's pane_id>`.
- Pane 0.1's command contains a literal `TMUX= ` prefix before `tmux -L <inner> attach`.
- After boot, the active outer pane is `0.1`, not `0.0`.
- The pane that runs the sidebar is `0.0` and is 40 columns wide (or the persisted width, per AC9).

**AC3 — reattach and honest failure.**
- Running `orchard shell` twice against the same outer socket produces exactly one outer session (`tmux -L <outer> list-sessions | wc -l` = 1) and the second invocation attaches.
- With the outer session alive but pane 0.1's inner client dead, a re-run leaves 0.1 running a live inner attach (`tmux -L <inner> list-clients` includes 0.1's tty) rather than a shell prompt.
- `orchard shell --session nope` with sessions `a`,`b` present exits 2 and prints both names on stderr.
- `orchard shell --inner-socket nosuchsocket` exits non-zero with a message naming the socket, and creates no outer session.

**AC4 — dispatch table.**
- `orchard shell --version` and `orchard upgrade --check` both resolve (exit code ≠ 127).
- `orchard --help` lists `shell` and `upgrade`.
- `cargo test -p orchard-dispatcher` passes with `NAMESPACE_VERBS` containing `shell` and `upgrade` and `TUI_VERBS` containing neither.
- `orchard-tui upgrade` exits non-zero with an unknown-command error (the stub is gone).

**AC5 — release artifacts.**
- A release tag produces, for each of `x86_64-apple-darwin`, `aarch64-apple-darwin`, `x86_64-unknown-linux-gnu`, `aarch64-unknown-linux-gnu`: `orchard-daemon-<t>.tar.gz`, `orchard-sidebar-<t>.tar.gz`, `orchard-shell-<t>.tar.gz`, `orchard-upgrade-<t>.tar.gz`, `orchard-suite-<t>.tar.gz`, each with a `.sha256` sibling.
- `orchard-<t>.tar.gz` (the dispatcher asset `npm/install.js` fetches) is still present on the release with unchanged contents.
- A single `SHA256SUMS` asset lists every tarball, and `sha256sum -c SHA256SUMS` passes in a directory holding all of them.
- Every binary inside `orchard-suite-<t>.tar.gz` prints the release semver for `--version`.
- `make dist VERSION=9.9.9` locally produces the same file names under `dist/`.

**AC6 — installer.**
- On a machine with no orchard installed, the curl one-liner exits 0 and `orchard --version` afterwards prints the released semver.
- Re-running the identical command exits 0, prints that it is already current, and leaves every binary's mtime and sha256 unchanged.
- With a corrupted `SHA256SUMS` entry, the installer exits non-zero, prints the mismatch, and installs nothing (`command -v orchard` still empty on a clean box; unchanged on a box with a prior install).
- `--version v1.0.0` installs exactly that version even when a newer one exists.
- `--json` prints a single line on stdout matching `{"ok":true,"data":{...}}` on success and `{"ok":false,"error":{"code":...,"message":...}}` with non-zero exit on failure; `jq -e .ok` succeeds/fails accordingly.
- `--prefix /tmp/pfx` places the binaries there and nowhere else.
- `--from-source` in a checkout with no network produces working binaries.
- On Linux the installed `~/.config/systemd/user/orchard.service` has an `ExecStart` whose path exists and is executable.
- `scripts/install.bats` passes under `make bats-test`.

**AC7 — upgrade.**
- `orchard upgrade --check` on an up-to-date install exits 0, prints current and latest, and modifies no file (verified by mtime + sha256 over the install dir before/after).
- `orchard upgrade` from vN to vN+1 leaves all six binaries reporting vN+1.
- `orchard upgrade --version <older>` downgrades and the binaries report the older version.
- With the install directory read-only, upgrade exits non-zero, names the directory, and leaves every binary at its original sha256.
- With a tampered download (checksum mismatch simulated via `ORCHARD_RELEASE_REPO` pointed at a fixture server), no binary is replaced and the exit is non-zero.
- `ORCHARD_RELEASE_REPO=<fixture>` is honoured — the real GitHub API is not contacted (assertable by pointing it at an `httptest` server and asserting the request count there).
- Upgrading while `orchard shell` is attached does not kill the running outer session.

**AC8 — doctor.**
- `orchard shell doctor --json` on a healthy sweatshop box prints `{"ok":true,...}` and exits 0.
- With the daemon stopped, it exits non-zero and the daemon check reports fail with a remedy string containing `systemctl --user start orchard`.
- With a deliberately mismatched binary (an older `orchard-sidebar` placed first on `$PATH`), the version-match check fails and names both versions.
- Run from inside an existing tmux session, the `$TMUX` check reports warn — not fail — and the overall exit stays 0 if nothing else failed.
- With `tmux` absent from `$PATH`, the tmux check fails rather than the binary panicking.
- Every check appears in `--json` `data.checks[]` with `{id, status, detail, remedy}` and `status ∈ {pass,warn,fail}`.

**AC9 — sidebar: persisted UI state, badge, update indicator.**
- Collapsing the sidebar, quitting `orchard shell`, and re-running it produces a pane 3 columns wide at boot — the pane is *created* collapsed (assert `pane_width` on the first `display -p` after boot, with no intervening resize command in the trace).
- Dragging the divider to 55, restarting, gives a 55-column pane.
- The state file `${XDG_STATE_HOME}/orchard/sidebar-ui.json` contains `{"collapsed":true,"width":55}`-shaped JSON; deleting it restores the 40-column default without error.
- With `ORCHARD_SIDEBAR_FAKE` producing 3 attention-bucket rows, the header renders the count `3`; with 0 such rows, no badge is rendered (not a `0` badge).
- `--bell` emits exactly one `\a` when the attention count goes 2→3 and none when it goes 3→2 or stays flat.
- With `update-check.json` naming a `latest` greater than `current`, the header renders the indicator; with equal versions it renders nothing; with the file absent it renders nothing and logs nothing.
- The sidebar makes no outbound network request for the update check: with `ORCHARD_RELEASE_REPO` pointed at a fixture server and `orchard-shell` not running, launching the sidebar alone produces zero requests at the fixture.
- `ORCHARD_NO_UPDATE_CHECK=1` means `update-check.json` is never written or refreshed by `orchard-shell`.
- A failed update check (fixture returns 500 or times out) leaves the sidebar rendering normally with no banner and no crash.

**AC10 — sidebar: filter and jump.**
- `/` then `pay` narrows the list to rows whose name, mission or directory contains `pay` case-insensitively; the count of rendered cards equals the count of matching rows.
- `Esc` restores the full list with the same session selected as before the filter opened.
- A filter matching nothing renders an explicit no-match line, not an empty band, and the footer git box still draws its fixed `gitBoxRows`.
- Filtering never triggers a `switch-client` (assert zero `switch-client` execs during a filter session).
- `M-3` from either pane selects and switches to the 3rd *visible* row, and hands focus back to pane 0.1 (outer active pane is 0.1 afterwards).
- `M-9` with only 4 visible rows is a no-op: no switch, no error, selection unchanged.
- With a filter active, `M-2` targets the 2nd filtered row, not the 2nd unfiltered row.

**AC11 — docs and specs.**
- `docs/install.md` exists and its sweatshop runbook commands are the ones the §8 live drive actually ran (no aspirational commands).
- `docs/outer-shell.md` exists, `docs/outer-shell-prototype.md` does not, and no doc references `launch.sh`.
- The three `specs/features/outer-shell-*.feature` files parse as Gherkin and every `Scenario:` title is matched by a `// @scenario <title>` annotation in a Go test.
- No scenario title in `specs/features/outer-shell-*.feature` duplicates one in `specs/features/sidebar-v1-hardening-*.feature`.
- `make check-feature-parity` still passes (these specs are outside its scope and must not break it).

**AC12 — nothing regressed.**
- `cargo test`, `go test ./...`, `cargo clippy -- -D warnings`, `cargo fmt --check` and `make bats-test` all pass.
- `npm/install.js` is unmodified in the diff.
- No daemon schema file (`schema.graphql`, `daemon/*/schema.graphql`) is modified in the diff.
- `orchard-sidebar` launched *outside* the wrapper (no `ORCHARD_TMUX_SOCKET`) behaves byte-for-byte as before — its tmux execs carry no `-L` and no `-c`.

## Handoff

- ACs ready for ac-reviewer (see §11 above).
- Implementation → coder for steps 1, 2, 3, 8, 9, 11, 12 (design judgment in each);
  fast-coder for steps 4, 5 and the mechanical half of 13, per
  `~/.claude/references/model-selection.md`.
- Step 0 and the §8 live drive → operator.
