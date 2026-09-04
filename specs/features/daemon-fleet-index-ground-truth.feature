Feature: Daemon fleet index reflects git/tmux ground truth (#701)
  As an orchardist relying on the daemon as the primary fleet interface
  I want `repos[].worktrees`, `tmuxSessions`, and `host.tmuxSessions` to match
  live `git worktree list` + `tmux list-sessions`
  So that the fleet view never silently returns a tiny, wrong subset of reality

  # Scope: Go daemon only — internal/server/providers/git/, internal/server/providers/tmux/,
  # and internal/cli/daemon/ wiring. Three independent defects:
  #   D1  bare-clone gitdir resolution (git/adapter.go resolveGitDir + FetchAll)
  #   D2  git provider never re-converged on post-boot repo discovery
  #       (repodiscovery is pull-only TTL; ApplyProjects fires only on config.json change)
  #   D3  tmux `-F` TAB separator sanitized to `_` under a non-UTF-8 locale, rows dropped silently
  # Out of scope: Rust `orchard sessions --json` rebind (→ #426; interactive `orchard sessions`
  # already uses the daemon and inherits these fixes); #699's wrong-socket fix (distinct root cause).
  #
  # Evidence harness for @e2e: a throwaway daemon started with
  #   env -i HOME=<tmp>/home PATH=$PATH TMUX_TMPDIR=<tmp>/tmux \
  #     ./bin/orchard-daemon daemon start --addr 127.0.0.1:7799
  # against real git fixtures under <tmp>/repos and a throwaway tmux socket <sock>.
  # Fresh PID via `pgrep -f 'orchard-daemon.*7799'`. The TTL bound is repodiscovery
  # DefaultTTL (30s unless the harness overrides via WithTTL). No systemctl / launchctl.

  Background:
    Given a throwaway daemon on 127.0.0.1:7799 with real git fixtures under <tmp>/repos
    And a throwaway tmux socket <sock>
    And every assertion is captured beside `git -C <repo> worktree list` and/or `tmux -S <sock> list-sessions`

  # =======================================================================
  # AC1 — Normal repo: all linked worktrees (regression guard for the .git path)
  # =======================================================================

  @integration @issue-701
  Scenario: Adapter enumerates a non-bare repo's main plus all linked worktrees
    Given a non-bare repo with N linked worktrees (`../wt`, `.worktrees/x`) on disk
    When `GitWorktreeAdapter.FetchAll` scans the project
    Then it returns exactly N+1 worktrees whose paths equal the `git worktree list` path set
    And no linked worktree path is missing or duplicated

  @e2e @issue-701
  Scenario: Daemon GraphQL returns every linked worktree of a non-bare repo
    Given the non-bare repo registered in the throwaway daemon's config
    When `{repos{id worktrees{path}}}` is queried
    Then that repo's `worktrees` path set equals `git -C <repo> worktree list`
    And the GraphQL response and `git worktree list` are captured side by side

  # =======================================================================
  # AC2 — Worktree REMOVAL reflected within one repodiscovery DefaultTTL
  # =======================================================================

  @e2e @issue-701
  Scenario: Removing a linked worktree drops it from the daemon within one TTL
    Given the AC1 repo with its worktrees visible in `repos[].worktrees`
    When one worktree is removed via `git -C <repo> worktree remove <wt>`
    Then within one repodiscovery DefaultTTL the removed path is absent from `repos[].worktrees`
    And that repo's worktree count drops by exactly one
    And before/after GraphQL captures bracket the `git worktree remove` command

  # =======================================================================
  # AC3 — Worktree ADD to an existing config repo within one TTL (D2 positive case)
  # =======================================================================

  @e2e @issue-701
  Scenario: Adding a linked worktree to a config repo surfaces it within one TTL without a config edit
    Given the AC1 repo already present in config.json
    When `git -C <repo> worktree add <new-wt>` creates a new linked worktree after boot
    Then within one repodiscovery DefaultTTL `repos[].worktrees` includes the new path
    And the repo's worktree count rises by exactly one
    And no config.json edit and no `orchard refresh` was performed

  # =======================================================================
  # AC4 — Multi-repo fleet: every repo's full worktree set
  # =======================================================================

  @e2e @issue-701
  Scenario: A K-repo fleet reports each repo's complete worktree set
    Given K >= 3 configured repos each with a distinct worktree set
    When `{repos{id worktrees{path}}}` is queried once
    Then all K repos are present
    And each repo's `worktrees` path set equals that repo's `git worktree list` path set

  # =======================================================================
  # AC5 / AC6 — D1: bare clones
  # =======================================================================

  @integration @issue-701
  Scenario: resolveGitDir treats a bare repository's working dir as the gitdir
    Given a directory holding `HEAD` and `objects/` but no `.git` entry (a bare clone root)
    When `resolveGitDir` resolves it
    Then it returns the directory itself as the gitdir
    And the main worktree entry is marked `bare: true` with `branch: ""`

  @e2e @issue-701
  Scenario: Daemon GraphQL lists a bare clone's bare root plus all its linked worktrees
    Given a `git clone --bare` repo at `<repo>.git` with M linked worktrees added via `git worktree add`
    When `{repos{id worktrees{path branch bare}}}` is queried
    Then the repo returns M+1 worktrees
    And the bare-root entry has `bare: true` and `branch: ""`
    And the M linked entries' paths equal the linked rows of `git -C <repo>.git worktree list`

  @e2e @issue-701
  Scenario: Bare-repo seed no longer logs a resolve-gitdir failure (diagnostic, subordinate to AC5)
    Given the AC5 bare clone and the AC5 GraphQL output already proving enumeration
    When the throwaway daemon log is grepped for `resolve gitdir`
    Then it returns zero hits for the bare repo
    And a green grep alone does not stand in for the AC5 output proof

  # =======================================================================
  # AC7 — D2: repo discovered after boot gets its worktrees within one TTL
  # =======================================================================

  @e2e @issue-701
  Scenario: A repo discovered post-boot via a tmux pane cwd gains its worktrees within one TTL
    Given the daemon booted with repo R absent from config.json
    When a tmux pane whose cwd is under R makes repodiscovery discover R
    Then R appears in `repos` with an auto-derived slug
    And within one repodiscovery DefaultTTL R's `worktrees` equals `git -C R worktree list`
    And R never persists with `worktrees: []` past one DefaultTTL

  # =======================================================================
  # AC8 — Degraded source: one repo failing does not empty the fleet
  # =======================================================================

  @integration @issue-701
  Scenario: FetchAll skips one unreadable project and still returns every healthy project's worktrees
    Given K projects where one path has neither `.git` nor bare markers
    When `GitWorktreeAdapter.FetchAll` runs
    Then every healthy project's worktrees are returned
    And the failing project is omitted or empty but never suppresses siblings
    And FetchAll does not return a whole-set error

  @e2e @issue-701
  Scenario: A corrupt repo does not empty the daemon fleet or crash it
    Given K configured repos, one made unreadable/corrupt
    When `{repos{id worktrees{path}}}` is queried
    Then every healthy repo is present with its correct worktree set
    And the `repos` field returns no GraphQL error
    And the daemon process is still alive per `pgrep`

  # =======================================================================
  # AC9 / AC10 / AC11 / AC12 — D3: tmux under non-UTF-8 locale
  # =======================================================================

  @e2e @issue-701
  Scenario: tmuxSessions is correct with LANG and LC_* fully unset (systemd and launchd class)
    Given the daemon started with `env -i` (no LANG/LC_*) against socket <sock> with S live sessions
    When `{tmuxServer{alive} tmuxSessions{name}}` is queried
    Then `tmuxSessions` lists exactly the S names from `tmux -S <sock> list-sessions`
    And the D3 signature `{tmuxServer{alive:true} tmuxSessions:[]}` never occurs
    # Proves the D3 locale/separator fix only — NOT #699's wrong-socket defect.

  @e2e @issue-701
  Scenario: host.tmuxSessions agrees with top-level tmuxSessions
    Given the AC9 `env -i` fixture
    When `{tmuxSessions{name} host{tmuxSessions{name}}}` is queried in one request
    Then the two name sets are identical
    And both equal `tmux -S <sock> list-sessions`
    # The two are separate resolver paths; the fix must correct both.

  @e2e @issue-701
  Scenario: A normal UTF-8 locale still returns the full set including non-ASCII names (happy-path guard)
    Given the daemon started with `LANG=en_US.UTF-8` (or `C.UTF-8`) and a session with a non-ASCII name
    When `{tmuxSessions{name}}` is queried
    Then the full session set is returned
    And the non-ASCII name matches `tmux -S <sock> list-sessions` byte-for-byte

  @integration @issue-701
  Scenario: A row failing the field-count check is logged at WARN before being dropped
    Given the tmux adapter fed a real `list-panes -a` row mangled by the `_`-for-TAB case
    When the adapter parses the row
    Then it emits a WARN naming the raw field count versus the expected ListAllFieldCount
    And the WARN is a real daemon log line, not a mocked test record
    And no row is dropped silently

  # =======================================================================
  # AC13 / AC14 / AC15 — Sessions: no phantom, no stale-cache authority
  # =======================================================================

  @e2e @issue-701
  Scenario: No session absent from tmux appears in GraphQL
    Given socket <sock> with a known session set
    When `{tmuxSessions{name}}` is queried
    Then its name set is a subset of `tmux -S <sock> list-sessions`
    And the set-difference (GraphQL minus tmux) is empty
    # Generalizes the `ubuntu_orchardist` phantom.

  @integration @issue-701
  Scenario: A genuinely-down tmux server reports alive:false and an empty list, no phantom
    Given no tmux server on <sock> (`tmux -S <sock> kill-server`) polled after one IsAlive TTL
    When `{tmuxServer{alive} tmuxSessions{name}}` is queried
    Then `tmuxServer.alive` is false
    And `tmuxSessions` is empty with no phantom names and no GraphQL error

  @e2e @issue-701
  Scenario: Daemon GraphQL ignores a poisoned Rust on-disk cache
    Given `<home>/.cache/orchard/tmux_sessions.json` poisoned with a name absent from `tmux -S <sock> list-sessions`
    When `{tmuxSessions{name}}` is queried
    Then the poisoned name does not appear
    And the poisoned cache contents and the GraphQL capture are quoted together

  # =======================================================================
  # AC16 / AC17 — Cross-cutting
  # =======================================================================

  @e2e @issue-701
  Scenario: A fresh daemon reproduces correct output within 5 seconds without orchard refresh
    Given a fresh throwaway daemon (new PID) with the AC1, AC5, and AC9 fixtures in place
    When it is queried within 5 seconds of the boot log line and with no `orchard refresh` invoked
    Then `tmuxSessions` returns the live session set
    And `repos[].worktrees` returns the full worktree sets for both the normal and bare repos

  @e2e @issue-701
  Scenario: The sidecar janitor sweeps orphans without affecting enumeration
    Given orphan `orchard-claude-*.json` files present in <tmp> at boot
    When the daemon boots and is queried in the same run
    Then the log shows `sidecar janitor swept orphan files count=N dir=<dir>` (N may be > 0)
    And `tmuxSessions` equals `tmux list-sessions`
    And `repos[].worktrees` equals `git worktree list`
    # Guards the issue's own misleading "reconstructed from sidecars" signal.

  # =======================================================================
  # AC18 — Out-of-scope guard for Key decision 1 (Rust --json stays cache-backed)
  # =======================================================================

  @unit @issue-701
  Scenario: orchard sessions --json is not rebound to the daemon in this change
    Given this PR's diff
    When `orchard sessions --json` is run
    Then its stderr deprecation warning references migration issue #426
    And `git diff crates/orchard/src/sessions_index.rs` is empty
    And the emitted document keeps the legacy SessionsIndexOutput shape

  # --- AC Coverage Map ---
  # AC1  Normal repo all linked worktrees (regression)
  #   -> Adapter enumerates a non-bare repo's main plus all linked worktrees
  #   -> Daemon GraphQL returns every linked worktree of a non-bare repo
  # AC2  Worktree removal within one TTL
  #   -> Removing a linked worktree drops it from the daemon within one TTL
  # AC3  Worktree add to existing config repo within one TTL (D2 positive)
  #   -> Adding a linked worktree to a config repo surfaces it within one TTL without a config edit
  # AC4  Multi-repo fleet
  #   -> A K-repo fleet reports each repo's complete worktree set
  # AC5  Bare clone: bare root + all linked (D1)
  #   -> resolveGitDir treats a bare repository's working dir as the gitdir
  #   -> Daemon GraphQL lists a bare clone's bare root plus all its linked worktrees
  # AC6  Bare seed no resolve-gitdir error (subordinate to AC5)
  #   -> Bare-repo seed no longer logs a resolve-gitdir failure
  # AC7  Post-boot discovered repo gets worktrees within one TTL (D2)
  #   -> A repo discovered post-boot via a tmux pane cwd gains its worktrees within one TTL
  # AC8  Degraded source isolation
  #   -> FetchAll skips one unreadable project and still returns every healthy project's worktrees
  #   -> A corrupt repo does not empty the daemon fleet or crash it
  # AC9  tmuxSessions correct under env -i (systemd + launchd locale class) (D3)
  #   -> tmuxSessions is correct with LANG and LC_* fully unset
  # AC10 host.tmuxSessions agrees with top-level tmuxSessions
  #   -> host.tmuxSessions agrees with top-level tmuxSessions
  # AC11 UTF-8 happy-path regression guard (non-ASCII names)
  #   -> A normal UTF-8 locale still returns the full set including non-ASCII names
  # AC12 WARN on row-drop (real daemon log)
  #   -> A row failing the field-count check is logged at WARN before being dropped
  # AC13 No phantom (subset)
  #   -> No session absent from tmux appears in GraphQL
  # AC14 Server-down inverse (alive:false + empty, no phantom)
  #   -> A genuinely-down tmux server reports alive:false and an empty list, no phantom
  # AC15 Independent of the Rust on-disk cache
  #   -> Daemon GraphQL ignores a poisoned Rust on-disk cache
  # AC16 Fresh daemon within 5s, no refresh
  #   -> A fresh daemon reproduces correct output within 5 seconds without orchard refresh
  # AC17 Sidecar janitor sweeps without affecting enumeration
  #   -> The sidecar janitor sweeps orphans without affecting enumeration
  # AC18 Rust --json out-of-scope guard
  #   -> orchard sessions --json is not rebound to the daemon in this change
  #
  # AC count: 18. Mapped scenarios: 22 (>= 1 per AC). No drops, no gaps.
