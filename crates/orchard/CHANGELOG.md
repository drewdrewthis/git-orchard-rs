# Changelog

## Unreleased

### ⚠ BREAKING CHANGES

* **config: global config moves to `~/.orchard/config.json`** (resolves [#424](https://github.com/drewdrewthis/git-orchard-rs/issues/424)).

  Orchard's global config previously lived at `~/.config/orchard/config.json` (XDG-style). It now lives at **`~/.orchard/config.json`** — matching every other dotdir tool in the stack (`~/.aws`, `~/.kube`, `~/.ssh`, `~/.cargo`, `~/.claude`).

  **Migrate your existing config:**

  ```bash
  mv ~/.config/orchard ~/.orchard
  ```

  The legacy path is no longer read. If you start the daemon or any orchard CLI command without migrating, you will get a config-not-found error pointing you at the new path with the migration command above. See [ADR-014](docs/adr/014-config-dotdir-location.md) for the rationale.

  Out of scope for this release: the orchardist working directory at `~/.config/orchard/.orchardist/` and the state directory `~/.local/state/orchard` both stay where they are.

## [1.2.0](https://github.com/drewdrewthis/orchardist/compare/orchard-v1.1.0...orchard-v1.2.0) (2026-09-04)


### Features

* **cleanup:** daemon-owned stale-worktree cleanup — Phase 1 ([#693](https://github.com/drewdrewthis/orchardist/issues/693)) ([#695](https://github.com/drewdrewthis/orchardist/issues/695)) ([a8c4db2](https://github.com/drewdrewthis/orchardist/commit/a8c4db25a1069c4bf6a3d56041811805d6203e8c))
* **daemon,plugins,specs:** consolidated big-refactor — repo constitution + 12 domain modules + claude-contracts plugin + T8 parity ([#660](https://github.com/drewdrewthis/orchardist/issues/660)) ([552a850](https://github.com/drewdrewthis/orchardist/commit/552a8501bb6b8481b0a35d3e2ff651e927511715))
* **daemon:** Worktree.ahead and Worktree.behind (closes [#483](https://github.com/drewdrewthis/orchardist/issues/483)) ([#587](https://github.com/drewdrewthis/orchardist/issues/587)) ([5cdae77](https://github.com/drewdrewthis/orchardist/commit/5cdae77b0bf27d86f99d74b1587bce9afda7ca6f))
* **gh:** expose PullRequest headRefOid, reviews and reviewThreads ([#764](https://github.com/drewdrewthis/orchardist/issues/764)) ([b3686e9](https://github.com/drewdrewthis/orchardist/commit/b3686e9b7b693930979438a8a205e61a5c2bc0b1))
* orchard shell — outer tmux wrapper, sidebar v2, install/upgrade/doctor ([#747](https://github.com/drewdrewthis/orchardist/issues/747)) ([f2d8d4a](https://github.com/drewdrewthis/orchardist/commit/f2d8d4ace2c88669406dbed9555cad4ca0f4fc9d))
* publish recall as the orchardist plugin + rename repo refs ([#694](https://github.com/drewdrewthis/orchardist/issues/694)) ([4306d7c](https://github.com/drewdrewthis/orchardist/commit/4306d7c97c736aceb2b71b07025434bd6eb069b5))
* **release:** orchard shell/upgrade verbs, suite dist tarballs, curl-able installer ([#747](https://github.com/drewdrewthis/orchardist/issues/747)) ([5a9fcb5](https://github.com/drewdrewthis/orchardist/commit/5a9fcb5027e64e517bb6e3f38de0cf68c4c2bb7b))

## [1.1.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v1.0.0...orchard-v1.1.0) (2026-05-11)


### Features

* daemon-side joins (Worktree.claudeInstances, ClaudeInstance.{worktree,conversation}) + worktree lens ([137893f](https://github.com/drewdrewthis/git-orchard-rs/commit/137893f58380a0bbf89c271a98e401c43564bc1e))
* **orchard-gui:** adopt Tailwind v4 (foundation PR for [#544](https://github.com/drewdrewthis/git-orchard-rs/issues/544)) ([#556](https://github.com/drewdrewthis/git-orchard-rs/issues/556)) ([5201bc2](https://github.com/drewdrewthis/git-orchard-rs/commit/5201bc21e892d3f7588c0a3e79ec980e5601069d))


### Bug Fixes

* **github:** skip ISO date years in branch-name issue extractor ([#531](https://github.com/drewdrewthis/git-orchard-rs/issues/531)) ([3a237a9](https://github.com/drewdrewthis/git-orchard-rs/commit/3a237a9d88476818c04d34aa12e8bb1e2a434ac1)), closes [#379](https://github.com/drewdrewthis/git-orchard-rs/issues/379)
* restore canonical schema.json key order (test fixture invariance) ([7d1633e](https://github.com/drewdrewthis/git-orchard-rs/commit/7d1633e6ed0b944e22505afb5ca945a139f8fb15))
* **tui:** three UX ergonomics fixes — [#331](https://github.com/drewdrewthis/git-orchard-rs/issues/331), [#438](https://github.com/drewdrewthis/git-orchard-rs/issues/438), [#545](https://github.com/drewdrewthis/git-orchard-rs/issues/545) ([#574](https://github.com/drewdrewthis/git-orchard-rs/issues/574)) ([b70ce86](https://github.com/drewdrewthis/git-orchard-rs/commit/b70ce86b90e82a4cdaa32e333b08bf5fe5cf0d40))

## [1.0.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.14.0...orchard-v1.0.0) (2026-05-09)


### ⚠ BREAKING CHANGES

* **config:** orchard's global config now lives at ~/.orchard/config.json instead of ~/.config/orchard/config.json. Migrate with: mv ~/.config/orchard ~/.orchard

### Features

* **config:** move global config to ~/.orchard/ ([#424](https://github.com/drewdrewthis/git-orchard-rs/issues/424)) ([#477](https://github.com/drewdrewthis/git-orchard-rs/issues/477)) ([3e736ff](https://github.com/drewdrewthis/git-orchard-rs/commit/3e736ffc5296a9425cdd106eefdea8ecefb0a286))
* **tui:** rip cache_sources from dashboard refresh path ([#426](https://github.com/drewdrewthis/git-orchard-rs/issues/426) phase 5) ([#482](https://github.com/drewdrewthis/git-orchard-rs/issues/482)) ([19e0413](https://github.com/drewdrewthis/git-orchard-rs/commit/19e0413491c72d2625a5d64a92dc9829521e8f0c))


### Bug Fixes

* **global_config:** preserve unknown top-level keys on save ([#432](https://github.com/drewdrewthis/git-orchard-rs/issues/432)) ([#479](https://github.com/drewdrewthis/git-orchard-rs/issues/479)) ([d2ef6c9](https://github.com/drewdrewthis/git-orchard-rs/commit/d2ef6c9d6c3d2bc3460967e1cf0bbc836f439554))
* **restore:** stop resurrecting killed tmux sessions on every read ([#460](https://github.com/drewdrewthis/git-orchard-rs/issues/460)) ([#480](https://github.com/drewdrewthis/git-orchard-rs/issues/480)) ([2394cf8](https://github.com/drewdrewthis/git-orchard-rs/commit/2394cf829f3e79548679d341f5ce2571112eb896))

## [0.14.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.13.0...orchard-v0.14.0) (2026-05-07)


### Features

* **daemon:** ClaudeInstance.lastActivityAt for TUI v2 LAST column ([#443](https://github.com/drewdrewthis/git-orchard-rs/issues/443)) ([#472](https://github.com/drewdrewthis/git-orchard-rs/issues/472)) ([d279ebf](https://github.com/drewdrewthis/git-orchard-rs/commit/d279ebf6a33b78f55af35e52f99aeab786518637))

## [0.13.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.12.1...orchard-v0.13.0) (2026-05-07)


### Features

* **tui:** thin-shell sessions over the daemon — federated + tmux-into-any ([#426](https://github.com/drewdrewthis/git-orchard-rs/issues/426)) ([#430](https://github.com/drewdrewthis/git-orchard-rs/issues/430)) ([f9bec04](https://github.com/drewdrewthis/git-orchard-rs/commit/f9bec0442c204866e73b0c20e66a6eb5025461f3))


### Bug Fixes

* **tmux:** self-protection invariant for kill_tmux_session callers (resolves [#369](https://github.com/drewdrewthis/git-orchard-rs/issues/369)) ([#457](https://github.com/drewdrewthis/git-orchard-rs/issues/457)) ([ce7e6bf](https://github.com/drewdrewthis/git-orchard-rs/commit/ce7e6bf74edffd51abb9a127b5ed2aaf86b27ae3))

## [0.12.1](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.12.0...orchard-v0.12.1) (2026-05-05)


### Bug Fixes

* rename rust binary to orchard-tui (resolves [#409](https://github.com/drewdrewthis/git-orchard-rs/issues/409)) ([#414](https://github.com/drewdrewthis/git-orchard-rs/issues/414)) ([82392a2](https://github.com/drewdrewthis/git-orchard-rs/commit/82392a272d5d84e1849fb3385fa190bbacbd69e9))

## [0.12.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.11.0...orchard-v0.12.0) (2026-04-27)


### Features

* **#374,#375:** orchard --json is a live read; add sessions --json ([#376](https://github.com/drewdrewthis/git-orchard-rs/issues/376)) ([f890035](https://github.com/drewdrewthis/git-orchard-rs/commit/f89003552977a8f1d81eab7ea57f226c341fc2b7))


### Bug Fixes

* **#361:** heal must never kill the invoking tmux session ([#368](https://github.com/drewdrewthis/git-orchard-rs/issues/368)) ([bce70d1](https://github.com/drewdrewthis/git-orchard-rs/commit/bce70d1e0089c96cbd851c431ab41c6751801377))

## [0.11.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.10.0...orchard-v0.11.0) (2026-04-25)


### Features

* **#337:** validate launch-remote + TUI Enter + federation on v0.9.0 ([#338](https://github.com/drewdrewthis/git-orchard-rs/issues/338)) ([18f4c4d](https://github.com/drewdrewthis/git-orchard-rs/commit/18f4c4df1e423a86cb66767599ed756dcb27673c))

## [0.10.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.9.0...orchard-v0.10.0) (2026-04-24)


### Features

* **federation:** transitive remote discovery ([#363](https://github.com/drewdrewthis/git-orchard-rs/issues/363)) ([#364](https://github.com/drewdrewthis/git-orchard-rs/issues/364)) ([403ea27](https://github.com/drewdrewthis/git-orchard-rs/commit/403ea27c77314e0d844de06e293e1685883c32c0))

## [0.9.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.8.0...orchard-v0.9.0) (2026-04-21)


### Features

* **remote:** federated orchard — discover remotes via `ssh host orchard --json` ([#329](https://github.com/drewdrewthis/git-orchard-rs/issues/329)) ([#330](https://github.com/drewdrewthis/git-orchard-rs/issues/330)) ([d36ece7](https://github.com/drewdrewthis/git-orchard-rs/commit/d36ece77605db90a92ba19ee6a273767c74cbe88))

## [0.8.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.7.0...orchard-v0.8.0) (2026-04-20)


### Features

* **cli:** add --toon output flag for token-efficient agent consumption ([#265](https://github.com/drewdrewthis/git-orchard-rs/issues/265)) ([84ff9d9](https://github.com/drewdrewthis/git-orchard-rs/commit/84ff9d901ce94d4c04b52d1f4675add4a17c50f4))
* **json:** expose unresolved review threads and review comments ([#307](https://github.com/drewdrewthis/git-orchard-rs/issues/307)) ([eb11f6a](https://github.com/drewdrewthis/git-orchard-rs/commit/eb11f6add46a5b024c84fa0fbc21b52a6f62e7df))
* **remote:** Boxd as first-class backend — hexagonal port + SWR ([#267](https://github.com/drewdrewthis/git-orchard-rs/issues/267)) ([#284](https://github.com/drewdrewthis/git-orchard-rs/issues/284)) ([7902754](https://github.com/drewdrewthis/git-orchard-rs/commit/790275438ac02de7b1591a0babe320711407f849))
* **signal:** add UnresolvedThreads pipeline status ([#320](https://github.com/drewdrewthis/git-orchard-rs/issues/320)) ([#321](https://github.com/drewdrewthis/git-orchard-rs/issues/321)) ([031129a](https://github.com/drewdrewthis/git-orchard-rs/commit/031129a6605415011f221ac03b20c0384cae843d))
* **tui:** pulse idle/input glyphs in column A; drop ❓ from status ([#304](https://github.com/drewdrewthis/git-orchard-rs/issues/304)) ([4bc0449](https://github.com/drewdrewthis/git-orchard-rs/commit/4bc0449dd59d6d0107b3d9fa8fcc531cac6f4983))
* **tui:** session restore — reconstruct tmux sessions from persisted state ([#277](https://github.com/drewdrewthis/git-orchard-rs/issues/277)) ([9ce1f66](https://github.com/drewdrewthis/git-orchard-rs/commit/9ce1f660d8e2a40b0d8b8268a3553559ec50f480))


### Bug Fixes

* **heal:** use live active-pane cwd instead of frozen session_path ([#313](https://github.com/drewdrewthis/git-orchard-rs/issues/313)) ([852e4fa](https://github.com/drewdrewthis/git-orchard-rs/commit/852e4fae029b86da1425beed01aca1e740fead5b))
* **join:** detect claude via TUI output when command is subshell ([#287](https://github.com/drewdrewthis/git-orchard-rs/issues/287)) ([fbf42ac](https://github.com/drewdrewthis/git-orchard-rs/commit/fbf42acb0ee6bbba40937b7dcb8f661abf6a7e35))
* **perf:** parallelize gh api, enforce hard SSH probe deadline ([#308](https://github.com/drewdrewthis/git-orchard-rs/issues/308)) ([ef5a643](https://github.com/drewdrewthis/git-orchard-rs/commit/ef5a64308946fa416fbc70536fabfb64bf2d9f90)), closes [#246](https://github.com/drewdrewthis/git-orchard-rs/issues/246)
* **remote:** boxd-fork session discovery via adapter-routed tmux refresh ([#288](https://github.com/drewdrewthis/git-orchard-rs/issues/288)) ([7c4cf3c](https://github.com/drewdrewthis/git-orchard-rs/commit/7c4cf3cbdd17eeb8a342ad18db5cf0467e3cfa8d))
* **scripts:** bookend send-keys Enter to flush stuck orchardist signals ([#278](https://github.com/drewdrewthis/git-orchard-rs/issues/278)) ([7f256ce](https://github.com/drewdrewthis/git-orchard-rs/commit/7f256ce9dbefbc209ed7d5aeca8bb088a915510b))
* **tui:** match tmux sessions by live active-pane cwd ([#292](https://github.com/drewdrewthis/git-orchard-rs/issues/292)) ([7a97ac0](https://github.com/drewdrewthis/git-orchard-rs/commit/7a97ac03b75316b3a1af37edcb8f5fc3e30f9471))
* **tui:** parallelize host reachability probes ([#263](https://github.com/drewdrewthis/git-orchard-rs/issues/263)) ([#271](https://github.com/drewdrewthis/git-orchard-rs/issues/271)) ([ebd1660](https://github.com/drewdrewthis/git-orchard-rs/commit/ebd16602cceb25040a7212266e85f511409e79e6))
* **tui:** parallelize reconnect_unreachable_hosts probes ([#274](https://github.com/drewdrewthis/git-orchard-rs/issues/274)) ([4aca09b](https://github.com/drewdrewthis/git-orchard-rs/commit/4aca09b8b0146982c912dcbd202f446684204cef))
* **tui:** preserve user collapse across refresh ([#261](https://github.com/drewdrewthis/git-orchard-rs/issues/261)) ([#305](https://github.com/drewdrewthis/git-orchard-rs/issues/305)) ([055bee0](https://github.com/drewdrewthis/git-orchard-rs/commit/055bee03685ab98339ea7d898a275088634ed7f7))
* **tui:** treat unprobed remote host as unknown, not blocked, on Enter ([#285](https://github.com/drewdrewthis/git-orchard-rs/issues/285)) ([88a0052](https://github.com/drewdrewthis/git-orchard-rs/commit/88a0052a42445d7ba749cc44d810b7e5421778b4))

## [0.7.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.6.0...orchard-v0.7.0) (2026-04-14)


### Features

* **claude:** richer worker telemetry in orchard --json ([#220](https://github.com/drewdrewthis/git-orchard-rs/issues/220)) ([#225](https://github.com/drewdrewthis/git-orchard-rs/issues/225)) ([ccdf522](https://github.com/drewdrewthis/git-orchard-rs/commit/ccdf522b6bfb5a2319b548935bcbb1b8cb36df2f))
* expose raw labels in JSON output ([#237](https://github.com/drewdrewthis/git-orchard-rs/issues/237)) ([0890f5f](https://github.com/drewdrewthis/git-orchard-rs/commit/0890f5f99ba70344af8f70a8044adf9170102c47))
* GitHub webhook receiver + event stream ([#215](https://github.com/drewdrewthis/git-orchard-rs/issues/215)) ([#226](https://github.com/drewdrewthis/git-orchard-rs/issues/226)) ([6825030](https://github.com/drewdrewthis/git-orchard-rs/commit/682503064fa0b9144f94b63e0aa17b0e5d3d93cf))
* **json:** first-class phase field on PRs and issues ([#219](https://github.com/drewdrewthis/git-orchard-rs/issues/219)) ([#224](https://github.com/drewdrewthis/git-orchard-rs/issues/224)) ([cf50df5](https://github.com/drewdrewthis/git-orchard-rs/commit/cf50df50852e41b0f192e5c6bac83314eadaa328))
* simplify data pipeline — canonical types + enriched fields ([#233](https://github.com/drewdrewthis/git-orchard-rs/issues/233)) ([#234](https://github.com/drewdrewthis/git-orchard-rs/issues/234)) ([8b756c3](https://github.com/drewdrewthis/git-orchard-rs/commit/8b756c343e51076803ae27c056fe50dd0e6c577a))
* split ci state into code-failing vs gate-failing ([#228](https://github.com/drewdrewthis/git-orchard-rs/issues/228)) ([e86ee1d](https://github.com/drewdrewthis/git-orchard-rs/commit/e86ee1ddbe7e6f2af1020aede5dbe12f7cae7f41))
* **tui:** session elapsed time in claude column ([#250](https://github.com/drewdrewthis/git-orchard-rs/issues/250)) ([1050b61](https://github.com/drewdrewthis/git-orchard-rs/commit/1050b6196182126d209c52191cb8db3f93855498))
* **tui:** signal lexicon — row pipeline state + tmux pane agent rollup ([#251](https://github.com/drewdrewthis/git-orchard-rs/issues/251)) ([#253](https://github.com/drewdrewthis/git-orchard-rs/issues/253)) ([be40a71](https://github.com/drewdrewthis/git-orchard-rs/commit/be40a710709f4eb36a8ed4d418abc2da0ab7bf0f))
* **tui:** smart sorting, priority indicators, and label badges ([#244](https://github.com/drewdrewthis/git-orchard-rs/issues/244)) ([25b1262](https://github.com/drewdrewthis/git-orchard-rs/commit/25b12625fca77c701eaf4d4fff457cc216d2bf71))


### Bug Fixes

* use /tmp for SSH ControlPath — macOS $TMPDIR too long even with %C ([b4d8b46](https://github.com/drewdrewthis/git-orchard-rs/commit/b4d8b466ec5715bc36c15c63e1d820b5ce14894c)), closes [#241](https://github.com/drewdrewthis/git-orchard-rs/issues/241)
* use SSH %C hash token to avoid ControlPath length limit ([#242](https://github.com/drewdrewthis/git-orchard-rs/issues/242)) ([8c7244e](https://github.com/drewdrewthis/git-orchard-rs/commit/8c7244e0c6dcc08c4140a438a6c749cc5956586a)), closes [#241](https://github.com/drewdrewthis/git-orchard-rs/issues/241)
* **watch:** debounce claude status transitions to suppress flicker ([#223](https://github.com/drewdrewthis/git-orchard-rs/issues/223)) ([0a1f379](https://github.com/drewdrewthis/git-orchard-rs/commit/0a1f379bbd9ddfd81a641d7ddd6449d9ee2ba7ae))

## [0.6.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.5.0...orchard-v0.6.0) (2026-04-10)


### Features

* **gui:** GUI v0 prototype — workspace refactor + Tauri scaffold + Rust commands ([#161](https://github.com/drewdrewthis/git-orchard-rs/issues/161)) ([#172](https://github.com/drewdrewthis/git-orchard-rs/issues/172)) ([50816aa](https://github.com/drewdrewthis/git-orchard-rs/commit/50816aabf6713061a8705e321eecf3bfe08b7823))
* **tui:** auto-zoom pane on Enter ([#182](https://github.com/drewdrewthis/git-orchard-rs/issues/182)) ([#185](https://github.com/drewdrewthis/git-orchard-rs/issues/185)) ([9bbf360](https://github.com/drewdrewthis/git-orchard-rs/commit/9bbf360ff683cf4b0df874cf390a2f203c60f9a5))
* **tui:** expand all session rows by default ([#199](https://github.com/drewdrewthis/git-orchard-rs/issues/199)) ([898d691](https://github.com/drewdrewthis/git-orchard-rs/commit/898d691c19822f6a79e98311cb2cee8fe1f586dd))
* **tui:** full tmux session/window/pane hierarchy ([#189](https://github.com/drewdrewthis/git-orchard-rs/issues/189)) ([2072504](https://github.com/drewdrewthis/git-orchard-rs/commit/20725047c39ecf4c9c213fb6f472eeec24659a15))
* **tui:** replace space-leader with dedicated search bar ([#183](https://github.com/drewdrewthis/git-orchard-rs/issues/183)) ([#187](https://github.com/drewdrewthis/git-orchard-rs/issues/187)) ([edf9aa6](https://github.com/drewdrewthis/git-orchard-rs/commit/edf9aa669f9385f8ef015d828e99d5aa079c3af6))
* **watch:** event-driven watch system with subscription model ([#194](https://github.com/drewdrewthis/git-orchard-rs/issues/194)) ([955b542](https://github.com/drewdrewthis/git-orchard-rs/commit/955b542f597e33f1c9a8922cebf606f5bdc6d93f))


### Bug Fixes

* **init:** embed absolute orchard path in wrapper scripts ([#186](https://github.com/drewdrewthis/git-orchard-rs/issues/186)) ([f024a56](https://github.com/drewdrewthis/git-orchard-rs/commit/f024a56ae002f29059a96461362ffabc0b31a3a6))
* **tui:** align fuzzy highlight offsets and fix window sub-row layout ([#200](https://github.com/drewdrewthis/git-orchard-rs/issues/200)) ([72910ff](https://github.com/drewdrewthis/git-orchard-rs/commit/72910ffdb0b43a10d7365dab8147af9732c2c593))
* **tui:** Enter activates highlighted row even with active filter ([#188](https://github.com/drewdrewthis/git-orchard-rs/issues/188)) ([85222b0](https://github.com/drewdrewthis/git-orchard-rs/commit/85222b0a4aa64627d16f63065f201ad4a90ade01)), closes [#163](https://github.com/drewdrewthis/git-orchard-rs/issues/163)

## [0.5.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.4.0...orchard-v0.5.0) (2026-04-08)


### Features

* ephemeral quick-chat popup with orchardist ([#165](https://github.com/drewdrewthis/git-orchard-rs/issues/165)) ([#170](https://github.com/drewdrewthis/git-orchard-rs/issues/170)) ([5dec054](https://github.com/drewdrewthis/git-orchard-rs/commit/5dec0543fe636f921587614707d16144189080bd))
* **tui:** fuzzy filter across all visible fields ([#162](https://github.com/drewdrewthis/git-orchard-rs/issues/162)) ([#169](https://github.com/drewdrewthis/git-orchard-rs/issues/169)) ([6a3df42](https://github.com/drewdrewthis/git-orchard-rs/commit/6a3df42f35e93e12cd2ba3d8e767a382738e8bc6))


### Bug Fixes

* **hooks:** claude state machine + structured stop_reason handling ([#113](https://github.com/drewdrewthis/git-orchard-rs/issues/113)) ([#171](https://github.com/drewdrewthis/git-orchard-rs/issues/171)) ([aa7d7c6](https://github.com/drewdrewthis/git-orchard-rs/commit/aa7d7c6b8f3f448b6370d7bd81dd862e7503c668))
* persist auto-registered CWD repo to global config ([#160](https://github.com/drewdrewthis/git-orchard-rs/issues/160)) ([dd66c98](https://github.com/drewdrewthis/git-orchard-rs/commit/dd66c98d1a1724e7a52fd2a2e25c2b24156d48a6))
* **tui:** remember last selected workspace/repo on launch ([#164](https://github.com/drewdrewthis/git-orchard-rs/issues/164)) ([#167](https://github.com/drewdrewthis/git-orchard-rs/issues/167)) ([c339589](https://github.com/drewdrewthis/git-orchard-rs/commit/c33958924f745b612c3288748389d4c0060a888a))

## [Unreleased]

### Features

* feat: quick-chat popup to send one-line prompts to the live orchardist Claude session ([#165](https://github.com/drewdrewthis/git-orchard-rs/issues/165))
  - New `orchard chat [--target <session>] [--message <text>]` subcommand delivers a prompt to the orchardist pane via `tmux send-keys`
  - New `orchard-chat` wrapper script installed by `orchard init --install`
  - New tmux keybinding `prefix + O` (capital O) opens a 60%×20% popup for quick dispatch
  - `chat_target` field added to global config for persisting the preferred orchardist session
  - Falls back to the first `tmux_sessions` entry when `chat_target` is unset

## [0.4.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.3.2...orchard-v0.4.0) (2026-04-07)


### Features

* add GitNexus code intelligence integration ([#130](https://github.com/drewdrewthis/git-orchard-rs/issues/130)) ([#143](https://github.com/drewdrewthis/git-orchard-rs/issues/143)) ([9780d08](https://github.com/drewdrewthis/git-orchard-rs/commit/9780d08b1c5369d56ef426c3ecff10aa072f784d))
* cap table height and add preview mouse scrolling ([#150](https://github.com/drewdrewthis/git-orchard-rs/issues/150)) ([3e273cc](https://github.com/drewdrewthis/git-orchard-rs/commit/3e273cc9c6a5b9b821f30de0336a0152ab61511b))
* leader-key input model with instant search ([#149](https://github.com/drewdrewthis/git-orchard-rs/issues/149)) ([bffdc5d](https://github.com/drewdrewthis/git-orchard-rs/commit/bffdc5dbc1dffb314de177ff844f51b8c33716a6))
* remember last selected worktree on launch ([#154](https://github.com/drewdrewthis/git-orchard-rs/issues/154)) ([89fc5ae](https://github.com/drewdrewthis/git-orchard-rs/commit/89fc5ae55fdf0f0b5ccedc116e7005f2d72462bc))
* two-tier TUI refresh — local 5s, full 60s ([#141](https://github.com/drewdrewthis/git-orchard-rs/issues/141)) ([2fd8409](https://github.com/drewdrewthis/git-orchard-rs/commit/2fd8409cf03df88762bfa03ee9c6301820b7b014))


### Bug Fixes

* correct GitHub URLs and add Substack link ([#147](https://github.com/drewdrewthis/git-orchard-rs/issues/147)) ([#148](https://github.com/drewdrewthis/git-orchard-rs/issues/148)) ([b1a62fd](https://github.com/drewdrewthis/git-orchard-rs/commit/b1a62fd73ac953e36768227a03c49753608d2ec4))

## [0.3.2](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.3.1...orchard-v0.3.2) (2026-04-04)


### Bug Fixes

* upgrade to Node 24 for npm trusted publishing (OIDC) ([#138](https://github.com/drewdrewthis/git-orchard-rs/issues/138)) ([2129866](https://github.com/drewdrewthis/git-orchard-rs/commit/2129866501b3524ecb8f093b8d5379fc4a18b0ad))

## [0.3.1](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.3.0...orchard-v0.3.1) (2026-04-04)


### Bug Fixes

* strip component prefix from release tag for npm version ([#136](https://github.com/drewdrewthis/git-orchard-rs/issues/136)) ([28ead07](https://github.com/drewdrewthis/git-orchard-rs/commit/28ead0735fcf0ab14e7b613450c4439595eb4618))

## [0.3.0](https://github.com/drewdrewthis/git-orchard-rs/compare/orchard-v0.2.0...orchard-v0.3.0) (2026-04-04)


### Features

* add --version/-V flag ([#49](https://github.com/drewdrewthis/git-orchard-rs/issues/49)) ([cf183db](https://github.com/drewdrewthis/git-orchard-rs/commit/cf183db6c4c3a7bdc285cffb922d5cb3f1c6dd3e)), closes [#33](https://github.com/drewdrewthis/git-orchard-rs/issues/33)
* add /install-orchard skill for new user onboarding ([ac17505](https://github.com/drewdrewthis/git-orchard-rs/commit/ac17505268c1e152b64acf6aba1ef9e427300051))
* add `i` keybinding to open linked GitHub issue in browser ([31707fd](https://github.com/drewdrewthis/git-orchard-rs/commit/31707fd78e1bf5f8e44176e9a0b805b2d64ba97b)), closes [#4](https://github.com/drewdrewthis/git-orchard-rs/issues/4)
* add debug logging throughout codebase (ADR-002) ([0b0b6b5](https://github.com/drewdrewthis/git-orchard-rs/commit/0b0b6b52b7227a364d4fe609dee4420f62e1feb5))
* add i keybinding to open linked GitHub issue in browser ([ae88f5e](https://github.com/drewdrewthis/git-orchard-rs/commit/ae88f5e68b00867ab177ae608151e9ea9f761a79))
* add integration test suite (33 new tests) ([83f96b2](https://github.com/drewdrewthis/git-orchard-rs/commit/83f96b2ec246fcf4d89f1cc1a1b0df7056ee08e6))
* add integration test suite with 33 new tests ([#3](https://github.com/drewdrewthis/git-orchard-rs/issues/3)) ([9615a1f](https://github.com/drewdrewthis/git-orchard-rs/commit/9615a1f6c4184f5b6612a69789b7cb5557f89ef9))
* add mouse support to TUI ([#77](https://github.com/drewdrewthis/git-orchard-rs/issues/77)) ([ad342ea](https://github.com/drewdrewthis/git-orchard-rs/commit/ad342ea0260fe0ed9abeb793edb75905b8f66a84))
* add orchard heal command for self-repair and cleanup ([#16](https://github.com/drewdrewthis/git-orchard-rs/issues/16)) ([#71](https://github.com/drewdrewthis/git-orchard-rs/issues/71)) ([3952ce1](https://github.com/drewdrewthis/git-orchard-rs/commit/3952ce14cca89909eeaa47e841dcaa29ba3a5598))
* add panic hooks to prevent terminal corruption on crash ([#66](https://github.com/drewdrewthis/git-orchard-rs/issues/66)) ([1454366](https://github.com/drewdrewthis/git-orchard-rs/commit/1454366f5f3b96119029e7b3175d961b0f72ce56))
* add setup-remote command to provision remote hosts ([#69](https://github.com/drewdrewthis/git-orchard-rs/issues/69)) ([d5e1231](https://github.com/drewdrewthis/git-orchard-rs/commit/d5e1231196484769db6405894fe51dcbad74cc45))
* add Theme struct to centralize TUI styling ([#67](https://github.com/drewdrewthis/git-orchard-rs/issues/67)) ([8a46f7d](https://github.com/drewdrewthis/git-orchard-rs/commit/8a46f7dace14e2c570fe47c5931d6ee404093099))
* add TUI keybinding to create new worktrees ([#57](https://github.com/drewdrewthis/git-orchard-rs/issues/57)) ([#73](https://github.com/drewdrewthis/git-orchard-rs/issues/73)) ([c3ab9d0](https://github.com/drewdrewthis/git-orchard-rs/commit/c3ab9d07a149ddedf6bf0b393c36d27858205be0))
* add TUI screenshot test helper ([#75](https://github.com/drewdrewthis/git-orchard-rs/issues/75)) ([#76](https://github.com/drewdrewthis/git-orchard-rs/issues/76)) ([a886e0d](https://github.com/drewdrewthis/git-orchard-rs/commit/a886e0dc4b68fb82df1c86b353fb5982289446ca))
* adopt ratatui ecosystem widgets for TUI polish ([#56](https://github.com/drewdrewthis/git-orchard-rs/issues/56)) ([#78](https://github.com/drewdrewthis/git-orchard-rs/issues/78)) ([be3f6e9](https://github.com/drewdrewthis/git-orchard-rs/commit/be3f6e952ac4e82ff595c696487dbdbbfd25354c))
* auto-create main tmux session at worktree origin on TUI startup ([#2](https://github.com/drewdrewthis/git-orchard-rs/issues/2)) ([4f1b5e7](https://github.com/drewdrewthis/git-orchard-rs/commit/4f1b5e7b85214153e58a44b60187d6f5c02e00b5)), closes [#1](https://github.com/drewdrewthis/git-orchard-rs/issues/1)
* Claude hooks state detection, branch column, auto-session creation ([5cf4742](https://github.com/drewdrewthis/git-orchard-rs/commit/5cf474225cdcd3c1003d391c20e3b9f162ebeba3))
* click notification to open Warp and switch tmux session ([892e517](https://github.com/drewdrewthis/git-orchard-rs/commit/892e5174ce21b9328ae23162ee681f9fe1b6ba17))
* expandable pane sub-rows with column reorder ([#97](https://github.com/drewdrewthis/git-orchard-rs/issues/97)) ([a2f1ab6](https://github.com/drewdrewthis/git-orchard-rs/commit/a2f1ab62ecb5d762cfef9a19148bfca278ba12c1))
* fetch Claude state files from remote hosts over SSH ([#72](https://github.com/drewdrewthis/git-orchard-rs/issues/72)) ([92a4e8c](https://github.com/drewdrewthis/git-orchard-rs/commit/92a4e8cf951bfcb053114653c5e3c17fd0848eaf))
* focused default view — collapse backlog, filter, toggle columns ([#14](https://github.com/drewdrewthis/git-orchard-rs/issues/14)) ([4e65266](https://github.com/drewdrewthis/git-orchard-rs/commit/4e65266c867f8dd7294203e3262ee5eb79837695))
* full Rust + Ratatui rewrite of git-orchard ([d107b0f](https://github.com/drewdrewthis/git-orchard-rs/commit/d107b0f5cca4f81d2bfb5ee87305b33bae6b753a))
* make terminal app configurable via global config and init wizard ([#52](https://github.com/drewdrewthis/git-orchard-rs/issues/52)) ([8580e78](https://github.com/drewdrewthis/git-orchard-rs/commit/8580e785c324c8fb2851cc44e87cc9ba158efdcb))
* popup mode, task-centric TUI, and service cache architecture ([a9563d5](https://github.com/drewdrewthis/git-orchard-rs/commit/a9563d59dfb5072698089dc6b386832a1b25bee2))
* remove backlog collapse, add priority toggle, left/right repo nav ([3fb994a](https://github.com/drewdrewthis/git-orchard-rs/commit/3fb994a4815d604fe41d565844681408b5783cd7))
* remove legacy collector, TUI fully driven by build_state ([#22](https://github.com/drewdrewthis/git-orchard-rs/issues/22) phase 3) ([bbc2b41](https://github.com/drewdrewthis/git-orchard-rs/commit/bbc2b41b949afa1e19515f590de3c7abe756d4f5))
* render standalone shepherd sessions in TUI ([#81](https://github.com/drewdrewthis/git-orchard-rs/issues/81)) ([#82](https://github.com/drewdrewthis/git-orchard-rs/issues/82)) ([2aee418](https://github.com/drewdrewthis/git-orchard-rs/commit/2aee418f30465166128c217bb4593d04ae059c80))
* restore original logo, add ❤ footer, remove filters, upgrade borders ([e84b68a](https://github.com/drewdrewthis/git-orchard-rs/commit/e84b68aecd1ba0ba4e15f6fec35c3eb3d561d6ec))
* shepherd persistent global session ([#47](https://github.com/drewdrewthis/git-orchard-rs/issues/47)) ([#68](https://github.com/drewdrewthis/git-orchard-rs/issues/68)) ([e5bc127](https://github.com/drewdrewthis/git-orchard-rs/commit/e5bc127709ccdf1b204c6964ee29ea792fe159bc))
* show Claude Code activity indicator in status badge ([7eb3ab9](https://github.com/drewdrewthis/git-orchard-rs/commit/7eb3ab9894ed8cbeaaae75ba31e1c1a5fc774756))
* task-centric TUI with cache architecture and multi-repo support ([a7ca2f6](https://github.com/drewdrewthis/git-orchard-rs/commit/a7ca2f67781748369f8a378ec792fa007d68cf48))
* unified OrchardState data model with build_state compositor ([#22](https://github.com/drewdrewthis/git-orchard-rs/issues/22)) ([5ec21b6](https://github.com/drewdrewthis/git-orchard-rs/commit/5ec21b6956f1f07770037070ce90a8617ebd1686))
* wire --json through build_state, consolidate cache reading ([#22](https://github.com/drewdrewthis/git-orchard-rs/issues/22) phase 2) ([23e4003](https://github.com/drewdrewthis/git-orchard-rs/commit/23e400357519326bb380bc594fc612024b2b11d9))
* workspace tab bar with colored repo indicators ([#91](https://github.com/drewdrewthis/git-orchard-rs/issues/91)) ([a5e3e53](https://github.com/drewdrewthis/git-orchard-rs/commit/a5e3e532ca9b4c8e6ae97fbbfd5a1b6e8cb8c0f9))


### Bug Fixes

* add clone step to install-orchard skill ([cf565d3](https://github.com/drewdrewthis/git-orchard-rs/commit/cf565d345eae178cd1ee02a71b29aeb36084bac0))
* add key event and tmux switch debug logging ([8df9ec1](https://github.com/drewdrewthis/git-orchard-rs/commit/8df9ec14a1605c5fd01d5fc400bde4a8bb2c34c4))
* allow tilde in shell_escape safe characters ([1bf3964](https://github.com/drewdrewthis/git-orchard-rs/commit/1bf39648d39ae553276b7d0b36827244429b8511))
* capture stderr from remote tmux switch-client for better error messages ([bc615b7](https://github.com/drewdrewthis/git-orchard-rs/commit/bc615b7574f87257118334e6cfd750ef0093b4c3))
* cleanup dialog shows progress spinner and deletion results ([ef38cef](https://github.com/drewdrewthis/git-orchard-rs/commit/ef38cef46452c8a2c7519411ead905c8df3d2d21))
* correct softprops/action-gh-release SHA (v2.6.1) ([#126](https://github.com/drewdrewthis/git-orchard-rs/issues/126)) ([3a86005](https://github.com/drewdrewthis/git-orchard-rs/commit/3a8600549585ba935052bec25225712a84fc361b))
* critical bugs from code review ([b2bfc0b](https://github.com/drewdrewthis/git-orchard-rs/commit/b2bfc0bf7b373b7baaf6c80993393c3aae1ce021))
* Ctrl+C exits with code 130 to break the shell restart loop ([59284c5](https://github.com/drewdrewthis/git-orchard-rs/commit/59284c5114f58220f49b0525dbcebc8f26a981a4))
* dim remote rows until connectivity is confirmed ([a7d4213](https://github.com/drewdrewthis/git-orchard-rs/commit/a7d4213c27ac3251a6dc3424e92274472e64e61d))
* fix release-please workflow and add npm wrapper package ([#124](https://github.com/drewdrewthis/git-orchard-rs/issues/124)) ([ffe859f](https://github.com/drewdrewthis/git-orchard-rs/commit/ffe859f4900dfa0e0fbfcf0073d9ea62e314155a))
* forward CLI command arg through tmux popup re-launch ([#85](https://github.com/drewdrewthis/git-orchard-rs/issues/85)) ([3ad7622](https://github.com/drewdrewthis/git-orchard-rs/commit/3ad762283e9c787b003a19b26c71131238e8ddbb))
* guard macOS-only notification code with cfg attrs ([#50](https://github.com/drewdrewthis/git-orchard-rs/issues/50)) ([0ba4540](https://github.com/drewdrewthis/git-orchard-rs/commit/0ba454024122ccb0aa1a178c6fa06f584c1279ca))
* issues cache — remove assignee filter, fetch all states ([#21](https://github.com/drewdrewthis/git-orchard-rs/issues/21)) ([cb9603c](https://github.com/drewdrewthis/git-orchard-rs/commit/cb9603c7d18ab0d987ae2cdd2586eb7fe8b8ee5b))
* logo rendering, preview height, ANSI escape codes, refresh behavior ([99843cf](https://github.com/drewdrewthis/git-orchard-rs/commit/99843cf7b27b21daac70df04a0f6cd4e8f9046c2))
* preserve remote worktrees during refresh, normalize paths, improve status display ([03cd93b](https://github.com/drewdrewthis/git-orchard-rs/commit/03cd93b1795d39a924827db415453b3682ac9f15))
* q switches back to previous tmux session instead of quitting ([e4cbf37](https://github.com/drewdrewthis/git-orchard-rs/commit/e4cbf37bb4613d8c415e26aca0a0dea43866ed0b))
* remove alternate screen so tmux switch-client works seamlessly ([ad1f3bf](https://github.com/drewdrewthis/git-orchard-rs/commit/ad1f3bf9f368550ffb47e55fbeee79c9b6cf8f0a))
* remove while-true loop so q actually quits the TUI ([54bc939](https://github.com/drewdrewthis/git-orchard-rs/commit/54bc9396f5522fb6f89da81d36229997051d7381))
* replace hardcoded /tmp paths with system temp directory ([#41](https://github.com/drewdrewthis/git-orchard-rs/issues/41)) ([0f81549](https://github.com/drewdrewthis/git-orchard-rs/commit/0f81549735616fc7448c54febb86a1c36080aae2)), closes [#29](https://github.com/drewdrewthis/git-orchard-rs/issues/29)
* restore 'p' key to priority toggle, remove Transfer feature ([#102](https://github.com/drewdrewthis/git-orchard-rs/issues/102)) ([794f880](https://github.com/drewdrewthis/git-orchard-rs/commit/794f880658f3f50f0d977b451304b666fa38d993))
* show closed issues and merged PRs in status, sort to needs attention ([4474073](https://github.com/drewdrewthis/git-orchard-rs/commit/4474073ef7a14459f996e4985f03f139d3d6d1be))
* skip default branches in PR matching, populate issue_state ([b8fee0b](https://github.com/drewdrewthis/git-orchard-rs/commit/b8fee0bcb9a14bc77aaf882d273f828496b31b2e))
* suspend terminal before tmux switch so session switching works ([40a9be0](https://github.com/drewdrewthis/git-orchard-rs/commit/40a9be0a921e338c97b5d4367803312aacd2f650))
* switch x86_64-apple-darwin to macos-14 runner ([#128](https://github.com/drewdrewthis/git-orchard-rs/issues/128)) ([8ad2aba](https://github.com/drewdrewthis/git-orchard-rs/commit/8ad2aba74bac768a61ba0051642fc2d0169dcfec))
* tmux session switching runs in background, shows warning on failure ([ba9ae11](https://github.com/drewdrewthis/git-orchard-rs/commit/ba9ae11e163a05ef01464740323d41721d55a0e2))
* treat null reviewDecision as no review required ([2ea6c8a](https://github.com/drewdrewthis/git-orchard-rs/commit/2ea6c8a5a618c541a5a8663a3b2b8b7e0eba7317))
* treat null reviewDecision as no review required ([#6](https://github.com/drewdrewthis/git-orchard-rs/issues/6)) ([8413af7](https://github.com/drewdrewthis/git-orchard-rs/commit/8413af76f8ddfd83fe102bd69a0d0eedd5a3a757))
* update test assertions after PII scrub from git history ([2ca05b0](https://github.com/drewdrewthis/git-orchard-rs/commit/2ca05b08610a2bd4939bd8c7ffaec8697d89305f))
* use correct git+https repository URL format in npm package ([#132](https://github.com/drewdrewthis/git-orchard-rs/issues/132)) ([a049444](https://github.com/drewdrewthis/git-orchard-rs/commit/a049444b78d4b7b96602436da6c8b4e9d396e29f))
* use release-please manifest to set version to 0.2.0 ([#134](https://github.com/drewdrewthis/git-orchard-rs/issues/134)) ([856fa7f](https://github.com/drewdrewthis/git-orchard-rs/commit/856fa7fd78b267bc796d419d3ebcc9de4ce11b06))
* use shell_escape for session name in notification execute command ([#42](https://github.com/drewdrewthis/git-orchard-rs/issues/42)) ([2a74110](https://github.com/drewdrewthis/git-orchard-rs/commit/2a74110ab10b3d58518727ec14bd451c25198df2)), closes [#32](https://github.com/drewdrewthis/git-orchard-rs/issues/32)

## [0.1.2](https://github.com/drewdrewthis/git-orchard-rs/compare/v0.1.1...v0.1.2) (2026-04-04)


### Bug Fixes

* switch x86_64-apple-darwin to macos-14 runner ([#128](https://github.com/drewdrewthis/git-orchard-rs/issues/128)) ([8ad2aba](https://github.com/drewdrewthis/git-orchard-rs/commit/8ad2aba74bac768a61ba0051642fc2d0169dcfec))

## [0.1.1](https://github.com/drewdrewthis/git-orchard-rs/compare/v0.1.0...v0.1.1) (2026-04-04)


### Bug Fixes

* correct softprops/action-gh-release SHA (v2.6.1) ([#126](https://github.com/drewdrewthis/git-orchard-rs/issues/126)) ([3a86005](https://github.com/drewdrewthis/git-orchard-rs/commit/3a8600549585ba935052bec25225712a84fc361b))

## 0.1.0 (2026-04-04)


### Features

* add --version/-V flag ([#49](https://github.com/drewdrewthis/git-orchard-rs/issues/49)) ([cf183db](https://github.com/drewdrewthis/git-orchard-rs/commit/cf183db6c4c3a7bdc285cffb922d5cb3f1c6dd3e)), closes [#33](https://github.com/drewdrewthis/git-orchard-rs/issues/33)
* add /install-orchard skill for new user onboarding ([ac17505](https://github.com/drewdrewthis/git-orchard-rs/commit/ac17505268c1e152b64acf6aba1ef9e427300051))
* add `i` keybinding to open linked GitHub issue in browser ([31707fd](https://github.com/drewdrewthis/git-orchard-rs/commit/31707fd78e1bf5f8e44176e9a0b805b2d64ba97b)), closes [#4](https://github.com/drewdrewthis/git-orchard-rs/issues/4)
* add debug logging throughout codebase (ADR-002) ([0b0b6b5](https://github.com/drewdrewthis/git-orchard-rs/commit/0b0b6b52b7227a364d4fe609dee4420f62e1feb5))
* add i keybinding to open linked GitHub issue in browser ([ae88f5e](https://github.com/drewdrewthis/git-orchard-rs/commit/ae88f5e68b00867ab177ae608151e9ea9f761a79))
* add integration test suite (33 new tests) ([83f96b2](https://github.com/drewdrewthis/git-orchard-rs/commit/83f96b2ec246fcf4d89f1cc1a1b0df7056ee08e6))
* add integration test suite with 33 new tests ([#3](https://github.com/drewdrewthis/git-orchard-rs/issues/3)) ([9615a1f](https://github.com/drewdrewthis/git-orchard-rs/commit/9615a1f6c4184f5b6612a69789b7cb5557f89ef9))
* add mouse support to TUI ([#77](https://github.com/drewdrewthis/git-orchard-rs/issues/77)) ([ad342ea](https://github.com/drewdrewthis/git-orchard-rs/commit/ad342ea0260fe0ed9abeb793edb75905b8f66a84))
* add orchard heal command for self-repair and cleanup ([#16](https://github.com/drewdrewthis/git-orchard-rs/issues/16)) ([#71](https://github.com/drewdrewthis/git-orchard-rs/issues/71)) ([3952ce1](https://github.com/drewdrewthis/git-orchard-rs/commit/3952ce14cca89909eeaa47e841dcaa29ba3a5598))
* add panic hooks to prevent terminal corruption on crash ([#66](https://github.com/drewdrewthis/git-orchard-rs/issues/66)) ([1454366](https://github.com/drewdrewthis/git-orchard-rs/commit/1454366f5f3b96119029e7b3175d961b0f72ce56))
* add setup-remote command to provision remote hosts ([#69](https://github.com/drewdrewthis/git-orchard-rs/issues/69)) ([d5e1231](https://github.com/drewdrewthis/git-orchard-rs/commit/d5e1231196484769db6405894fe51dcbad74cc45))
* add Theme struct to centralize TUI styling ([#67](https://github.com/drewdrewthis/git-orchard-rs/issues/67)) ([8a46f7d](https://github.com/drewdrewthis/git-orchard-rs/commit/8a46f7dace14e2c570fe47c5931d6ee404093099))
* add TUI keybinding to create new worktrees ([#57](https://github.com/drewdrewthis/git-orchard-rs/issues/57)) ([#73](https://github.com/drewdrewthis/git-orchard-rs/issues/73)) ([c3ab9d0](https://github.com/drewdrewthis/git-orchard-rs/commit/c3ab9d07a149ddedf6bf0b393c36d27858205be0))
* add TUI screenshot test helper ([#75](https://github.com/drewdrewthis/git-orchard-rs/issues/75)) ([#76](https://github.com/drewdrewthis/git-orchard-rs/issues/76)) ([a886e0d](https://github.com/drewdrewthis/git-orchard-rs/commit/a886e0dc4b68fb82df1c86b353fb5982289446ca))
* adopt ratatui ecosystem widgets for TUI polish ([#56](https://github.com/drewdrewthis/git-orchard-rs/issues/56)) ([#78](https://github.com/drewdrewthis/git-orchard-rs/issues/78)) ([be3f6e9](https://github.com/drewdrewthis/git-orchard-rs/commit/be3f6e952ac4e82ff595c696487dbdbbfd25354c))
* auto-create main tmux session at worktree origin on TUI startup ([#2](https://github.com/drewdrewthis/git-orchard-rs/issues/2)) ([4f1b5e7](https://github.com/drewdrewthis/git-orchard-rs/commit/4f1b5e7b85214153e58a44b60187d6f5c02e00b5)), closes [#1](https://github.com/drewdrewthis/git-orchard-rs/issues/1)
* Claude hooks state detection, branch column, auto-session creation ([5cf4742](https://github.com/drewdrewthis/git-orchard-rs/commit/5cf474225cdcd3c1003d391c20e3b9f162ebeba3))
* click notification to open Warp and switch tmux session ([892e517](https://github.com/drewdrewthis/git-orchard-rs/commit/892e5174ce21b9328ae23162ee681f9fe1b6ba17))
* expandable pane sub-rows with column reorder ([#97](https://github.com/drewdrewthis/git-orchard-rs/issues/97)) ([a2f1ab6](https://github.com/drewdrewthis/git-orchard-rs/commit/a2f1ab62ecb5d762cfef9a19148bfca278ba12c1))
* fetch Claude state files from remote hosts over SSH ([#72](https://github.com/drewdrewthis/git-orchard-rs/issues/72)) ([92a4e8c](https://github.com/drewdrewthis/git-orchard-rs/commit/92a4e8cf951bfcb053114653c5e3c17fd0848eaf))
* focused default view — collapse backlog, filter, toggle columns ([#14](https://github.com/drewdrewthis/git-orchard-rs/issues/14)) ([4e65266](https://github.com/drewdrewthis/git-orchard-rs/commit/4e65266c867f8dd7294203e3262ee5eb79837695))
* full Rust + Ratatui rewrite of git-orchard ([d107b0f](https://github.com/drewdrewthis/git-orchard-rs/commit/d107b0f5cca4f81d2bfb5ee87305b33bae6b753a))
* make terminal app configurable via global config and init wizard ([#52](https://github.com/drewdrewthis/git-orchard-rs/issues/52)) ([8580e78](https://github.com/drewdrewthis/git-orchard-rs/commit/8580e785c324c8fb2851cc44e87cc9ba158efdcb))
* popup mode, task-centric TUI, and service cache architecture ([a9563d5](https://github.com/drewdrewthis/git-orchard-rs/commit/a9563d59dfb5072698089dc6b386832a1b25bee2))
* remove backlog collapse, add priority toggle, left/right repo nav ([3fb994a](https://github.com/drewdrewthis/git-orchard-rs/commit/3fb994a4815d604fe41d565844681408b5783cd7))
* remove legacy collector, TUI fully driven by build_state ([#22](https://github.com/drewdrewthis/git-orchard-rs/issues/22) phase 3) ([bbc2b41](https://github.com/drewdrewthis/git-orchard-rs/commit/bbc2b41b949afa1e19515f590de3c7abe756d4f5))
* render standalone shepherd sessions in TUI ([#81](https://github.com/drewdrewthis/git-orchard-rs/issues/81)) ([#82](https://github.com/drewdrewthis/git-orchard-rs/issues/82)) ([2aee418](https://github.com/drewdrewthis/git-orchard-rs/commit/2aee418f30465166128c217bb4593d04ae059c80))
* restore original logo, add ❤ footer, remove filters, upgrade borders ([e84b68a](https://github.com/drewdrewthis/git-orchard-rs/commit/e84b68aecd1ba0ba4e15f6fec35c3eb3d561d6ec))
* shepherd persistent global session ([#47](https://github.com/drewdrewthis/git-orchard-rs/issues/47)) ([#68](https://github.com/drewdrewthis/git-orchard-rs/issues/68)) ([e5bc127](https://github.com/drewdrewthis/git-orchard-rs/commit/e5bc127709ccdf1b204c6964ee29ea792fe159bc))
* show Claude Code activity indicator in status badge ([7eb3ab9](https://github.com/drewdrewthis/git-orchard-rs/commit/7eb3ab9894ed8cbeaaae75ba31e1c1a5fc774756))
* task-centric TUI with cache architecture and multi-repo support ([a7ca2f6](https://github.com/drewdrewthis/git-orchard-rs/commit/a7ca2f67781748369f8a378ec792fa007d68cf48))
* unified OrchardState data model with build_state compositor ([#22](https://github.com/drewdrewthis/git-orchard-rs/issues/22)) ([5ec21b6](https://github.com/drewdrewthis/git-orchard-rs/commit/5ec21b6956f1f07770037070ce90a8617ebd1686))
* wire --json through build_state, consolidate cache reading ([#22](https://github.com/drewdrewthis/git-orchard-rs/issues/22) phase 2) ([23e4003](https://github.com/drewdrewthis/git-orchard-rs/commit/23e400357519326bb380bc594fc612024b2b11d9))
* workspace tab bar with colored repo indicators ([#91](https://github.com/drewdrewthis/git-orchard-rs/issues/91)) ([a5e3e53](https://github.com/drewdrewthis/git-orchard-rs/commit/a5e3e532ca9b4c8e6ae97fbbfd5a1b6e8cb8c0f9))


### Bug Fixes

* add clone step to install-orchard skill ([cf565d3](https://github.com/drewdrewthis/git-orchard-rs/commit/cf565d345eae178cd1ee02a71b29aeb36084bac0))
* add key event and tmux switch debug logging ([8df9ec1](https://github.com/drewdrewthis/git-orchard-rs/commit/8df9ec14a1605c5fd01d5fc400bde4a8bb2c34c4))
* allow tilde in shell_escape safe characters ([1bf3964](https://github.com/drewdrewthis/git-orchard-rs/commit/1bf39648d39ae553276b7d0b36827244429b8511))
* capture stderr from remote tmux switch-client for better error messages ([bc615b7](https://github.com/drewdrewthis/git-orchard-rs/commit/bc615b7574f87257118334e6cfd750ef0093b4c3))
* cleanup dialog shows progress spinner and deletion results ([ef38cef](https://github.com/drewdrewthis/git-orchard-rs/commit/ef38cef46452c8a2c7519411ead905c8df3d2d21))
* critical bugs from code review ([b2bfc0b](https://github.com/drewdrewthis/git-orchard-rs/commit/b2bfc0bf7b373b7baaf6c80993393c3aae1ce021))
* Ctrl+C exits with code 130 to break the shell restart loop ([59284c5](https://github.com/drewdrewthis/git-orchard-rs/commit/59284c5114f58220f49b0525dbcebc8f26a981a4))
* dim remote rows until connectivity is confirmed ([a7d4213](https://github.com/drewdrewthis/git-orchard-rs/commit/a7d4213c27ac3251a6dc3424e92274472e64e61d))
* fix release-please workflow and add npm wrapper package ([#124](https://github.com/drewdrewthis/git-orchard-rs/issues/124)) ([ffe859f](https://github.com/drewdrewthis/git-orchard-rs/commit/ffe859f4900dfa0e0fbfcf0073d9ea62e314155a))
* forward CLI command arg through tmux popup re-launch ([#85](https://github.com/drewdrewthis/git-orchard-rs/issues/85)) ([3ad7622](https://github.com/drewdrewthis/git-orchard-rs/commit/3ad762283e9c787b003a19b26c71131238e8ddbb))
* guard macOS-only notification code with cfg attrs ([#50](https://github.com/drewdrewthis/git-orchard-rs/issues/50)) ([0ba4540](https://github.com/drewdrewthis/git-orchard-rs/commit/0ba454024122ccb0aa1a178c6fa06f584c1279ca))
* issues cache — remove assignee filter, fetch all states ([#21](https://github.com/drewdrewthis/git-orchard-rs/issues/21)) ([cb9603c](https://github.com/drewdrewthis/git-orchard-rs/commit/cb9603c7d18ab0d987ae2cdd2586eb7fe8b8ee5b))
* logo rendering, preview height, ANSI escape codes, refresh behavior ([99843cf](https://github.com/drewdrewthis/git-orchard-rs/commit/99843cf7b27b21daac70df04a0f6cd4e8f9046c2))
* preserve remote worktrees during refresh, normalize paths, improve status display ([03cd93b](https://github.com/drewdrewthis/git-orchard-rs/commit/03cd93b1795d39a924827db415453b3682ac9f15))
* q switches back to previous tmux session instead of quitting ([e4cbf37](https://github.com/drewdrewthis/git-orchard-rs/commit/e4cbf37bb4613d8c415e26aca0a0dea43866ed0b))
* remove alternate screen so tmux switch-client works seamlessly ([ad1f3bf](https://github.com/drewdrewthis/git-orchard-rs/commit/ad1f3bf9f368550ffb47e55fbeee79c9b6cf8f0a))
* remove while-true loop so q actually quits the TUI ([54bc939](https://github.com/drewdrewthis/git-orchard-rs/commit/54bc9396f5522fb6f89da81d36229997051d7381))
* replace hardcoded /tmp paths with system temp directory ([#41](https://github.com/drewdrewthis/git-orchard-rs/issues/41)) ([0f81549](https://github.com/drewdrewthis/git-orchard-rs/commit/0f81549735616fc7448c54febb86a1c36080aae2)), closes [#29](https://github.com/drewdrewthis/git-orchard-rs/issues/29)
* restore 'p' key to priority toggle, remove Transfer feature ([#102](https://github.com/drewdrewthis/git-orchard-rs/issues/102)) ([794f880](https://github.com/drewdrewthis/git-orchard-rs/commit/794f880658f3f50f0d977b451304b666fa38d993))
* show closed issues and merged PRs in status, sort to needs attention ([4474073](https://github.com/drewdrewthis/git-orchard-rs/commit/4474073ef7a14459f996e4985f03f139d3d6d1be))
* skip default branches in PR matching, populate issue_state ([b8fee0b](https://github.com/drewdrewthis/git-orchard-rs/commit/b8fee0bcb9a14bc77aaf882d273f828496b31b2e))
* suspend terminal before tmux switch so session switching works ([40a9be0](https://github.com/drewdrewthis/git-orchard-rs/commit/40a9be0a921e338c97b5d4367803312aacd2f650))
* tmux session switching runs in background, shows warning on failure ([ba9ae11](https://github.com/drewdrewthis/git-orchard-rs/commit/ba9ae11e163a05ef01464740323d41721d55a0e2))
* treat null reviewDecision as no review required ([2ea6c8a](https://github.com/drewdrewthis/git-orchard-rs/commit/2ea6c8a5a618c541a5a8663a3b2b8b7e0eba7317))
* treat null reviewDecision as no review required ([#6](https://github.com/drewdrewthis/git-orchard-rs/issues/6)) ([8413af7](https://github.com/drewdrewthis/git-orchard-rs/commit/8413af76f8ddfd83fe102bd69a0d0eedd5a3a757))
* update test assertions after PII scrub from git history ([2ca05b0](https://github.com/drewdrewthis/git-orchard-rs/commit/2ca05b08610a2bd4939bd8c7ffaec8697d89305f))
* use shell_escape for session name in notification execute command ([#42](https://github.com/drewdrewthis/git-orchard-rs/issues/42)) ([2a74110](https://github.com/drewdrewthis/git-orchard-rs/commit/2a74110ab10b3d58518727ec14bd451c25198df2)), closes [#32](https://github.com/drewdrewthis/git-orchard-rs/issues/32)
