Feature: Persistent state and event system
  As an orchard user
  I want tasks, sessions, and events persisted to disk
  So that I don't lose track of work across restarts, crashes, and repos

  Background:
    Given the state directory is "~/.local/state/git-orchard/"
    And the config directory is "~/.config/git-orchard/"

  # ===================================================================
  # Global config (multi-repo)
  # ===================================================================

  @unit
  Scenario: Global config declares managed repos
    Given a config file "~/.config/git-orchard/config.json" containing:
      """json
      {
        "repos": [
          { "path": "/Users/hope/workspace/git-orchard-rs" },
          { "path": "/Users/hope/workspace/api-server", "remote": { "host": "devbox", "repoPath": "/srv/api-server" } }
        ]
      }
      """
    When config is loaded
    Then 2 repos are configured
    And repo "api-server" has a remote on host "devbox"
    And repo "git-orchard-rs" has no remote

  @unit
  Scenario: Per-repo .git/orchard.json is still read for remote config
    Given a repo at "/Users/hope/workspace/myrepo" with ".git/orchard.json":
      """json
      { "remote": { "host": "devbox", "repoPath": "/srv/myrepo" } }
      """
    And the global config has repo "/Users/hope/workspace/myrepo" with no remote
    When config is loaded for "myrepo"
    Then the remote config from ".git/orchard.json" is used

  @unit
  Scenario: Global config remote overrides per-repo config
    Given a global config with repo "/Users/hope/workspace/myrepo" and remote host "newbox"
    And a ".git/orchard.json" with remote host "oldbox"
    When config is loaded for "myrepo"
    Then the remote host is "newbox"

  @unit
  Scenario: Running orchard in an unconfigured repo auto-adds it
    Given the global config has no entry for "/Users/hope/workspace/newrepo"
    When orchard is run from "/Users/hope/workspace/newrepo"
    Then "/Users/hope/workspace/newrepo" is added to the global config repos list

  # ===================================================================
  # State file — structure and persistence
  # ===================================================================

  @unit
  Scenario: State file is created on first run
    Given no state file exists at "~/.local/state/git-orchard/state.json"
    When orchard starts
    Then a state file is created with version 1 and an empty tasks array

  @unit
  Scenario: State file schema
    Given a state file at "~/.local/state/git-orchard/state.json"
    Then it has the structure:
      """json
      {
        "version": 1,
        "tasks": [
          {
            "id": "git-orchard-rs#47",
            "source": { "type": "github_issue", "repo": "hopegrace/git-orchard-rs", "number": 47 },
            "status": "in_progress",
            "priority": 1,
            "worktree": "/Users/hope/workspace/git-orchard-rs-47",
            "sessions": ["git-orchard-rs_47_main"],
            "pr": 53,
            "created_at": "2026-03-18T10:00:00Z",
            "updated_at": "2026-03-20T14:32:00Z"
          }
        ]
      }
      """

  @unit
  Scenario: Tasks have a sessions array, not a single session
    Given a task with id "git-orchard-rs#47"
    Then its "sessions" field is an array of tmux session names
    And it may contain zero or more entries

  @unit
  Scenario: State file is written atomically
    When the state is saved
    Then it is written to a temporary file first
    And then renamed to "state.json"
    So that a crash mid-write does not corrupt the state

  @unit
  Scenario: State file is saved after every mutation
    When a task status changes
    Then the state file is written to disk before the next event loop tick

  # ===================================================================
  # Task lifecycle
  # ===================================================================

  @unit
  Scenario: Task statuses
    Then valid task statuses are:
      | status      |
      | backlog     |
      | ready       |
      | in_progress |
      | in_review   |
      | done        |

  @integration
  Scenario: Creating a task from a GitHub issue
    Given GitHub issue #47 exists in repo "hopegrace/git-orchard-rs" with title "Task-centric state system"
    When I create a task from issue #47
    Then a task is added to the state file with:
      | field  | value                                   |
      | id     | git-orchard-rs#47                       |
      | source | github_issue:hopegrace/git-orchard-rs#47 |
      | status | ready                                   |

  @integration
  Scenario: GitHub issue sync creates tasks for open issues
    Given the global config has repo "hopegrace/git-orchard-rs"
    And GitHub has open issues #47, #48, #52 for that repo
    And the state file has no tasks for those issues
    When issue sync runs
    Then tasks are created for #47, #48, #52 with status "backlog"

  @integration
  Scenario: Issue sync does not duplicate existing tasks
    Given the state file already has a task for "git-orchard-rs#47"
    When issue sync runs and #47 is still open
    Then no duplicate task is created
    And the existing task is unchanged

  @integration
  Scenario: Issue sync marks done tasks when issue is closed
    Given the state file has a task for "git-orchard-rs#47" with status "in_progress"
    When issue sync runs and #47 is now closed
    Then the task status changes to "done"
    And an event "task.status_change" is logged

  # ===================================================================
  # Enter key — progressive resource creation
  # ===================================================================

  @e2e
  Scenario: Enter on a ready task with no worktree or session
    Given a task "git-orchard-rs#47" with status "ready"
    And it has no worktree and no sessions
    When I press Enter on the task
    Then a git worktree is created for the task's branch
    And a tmux session is created in that worktree
    And the session name is added to the task's sessions array
    And the task status changes to "in_progress"
    And my tmux client switches to the new session

  @e2e
  Scenario: Enter on an in-progress task with one session
    Given a task "git-orchard-rs#47" with status "in_progress"
    And it has one session "git-orchard-rs_47_main"
    When I press Enter on the task
    Then my tmux client switches to "git-orchard-rs_47_main"

  @e2e
  Scenario: Enter on an in-progress task with multiple sessions
    Given a task "git-orchard-rs#47" with status "in_progress"
    And it has sessions ["git-orchard-rs_47_main", "git-orchard-rs_47_claude"]
    When I press Enter on the task
    Then an inline session picker is shown with both sessions
    And each session shows its pane status (agent active, idle, etc.)

  @e2e
  Scenario: Enter on a task with a worktree but no session
    Given a task "git-orchard-rs#47" with status "in_progress"
    And it has worktree "/Users/hope/workspace/git-orchard-rs-47"
    And it has no sessions
    When I press Enter on the task
    Then a tmux session is created in the existing worktree
    And the session is added to the task's sessions array
    And my tmux client switches to the new session

  # ===================================================================
  # Session management — multiple sessions per task
  # ===================================================================

  @e2e
  Scenario: Creating a new session on a task
    Given the cursor is on task "git-orchard-rs#47" which has worktree and session
    When I press "n"
    Then a text input prompts for session suffix
    When I type "claude" and press Enter
    Then a tmux session named "git-orchard-rs_47_claude" is created in the task's worktree
    And it is added to the task's sessions array
    And my tmux client switches to the new session

  @integration
  Scenario: Session discovery binds orphaned sessions to tasks
    Given a tmux session "git-orchard-rs_47_main" exists at path "/Users/hope/workspace/git-orchard-rs-47"
    And task "git-orchard-rs#47" has worktree "/Users/hope/workspace/git-orchard-rs-47"
    And the task's sessions array is empty
    When the collector runs session discovery
    Then "git-orchard-rs_47_main" is added to the task's sessions array
    And an event "session.bound" is logged

  @integration
  Scenario: Orphaned session with no matching task is flagged
    Given a tmux session "mystery-session" exists
    And no task has a worktree matching the session's path
    When the collector runs session discovery
    Then the session is surfaced in the TUI as "orphaned"
    And an event "session.orphaned" is logged

  @integration
  Scenario: Dead session is detected and surfaced
    Given task "git-orchard-rs#47" has session "git-orchard-rs_47_claude" in its sessions array
    And tmux reports no session named "git-orchard-rs_47_claude"
    When the collector runs session reconciliation
    Then the TUI shows "git-orchard-rs_47_claude" as dead
    And an event "session.dead" is logged

  # ===================================================================
  # Pane-level awareness
  # ===================================================================

  @integration
  Scenario: Collector fetches all panes per session
    Given a tmux session "git-orchard-rs_47_main" has 2 panes:
      | pane_id | command | pane_title   |
      | %4      | zsh     | zsh          |
      | %5      | claude  | Claude Code  |
    When the collector runs
    Then the session data includes both panes with their commands and titles

  @integration
  Scenario: Claude agent detected in any pane marks session as agent-active
    Given a session has panes:
      | pane_id | command | pane_title  |
      | %4      | zsh     | zsh         |
      | %5      | claude  | Claude Code |
    When agent detection runs
    Then the session is marked as having an active agent in pane %5

  @integration
  Scenario: Multiple Claude panes in one session are all tracked
    Given a session has panes:
      | pane_id | command | pane_title  |
      | %4      | zsh     | zsh         |
      | %5      | claude  | Claude Code |
      | %6      | claude  | Claude Code |
    When agent detection runs
    Then panes %5 and %6 are both marked as agent-active

  @integration
  Scenario: TUI shows pane-level detail for in-progress tasks
    Given task "git-orchard-rs#47" has session "git-orchard-rs_47_main"
    And the session has panes: zsh (idle), claude (active)
    When the TUI renders the task in expanded view
    Then it shows:
      """
      session: git-orchard-rs_47_main
         ├─ pane 1: zsh
         └─ pane 2: ⚡ claude active
      """

  # ===================================================================
  # Task cleanup and archival
  # ===================================================================

  @e2e
  Scenario: Marking a task as done
    Given task "git-orchard-rs#47" is in status "in_progress"
    When I press "d" on the task
    Then the task status changes to "done"
    And an event "task.status_change" is logged

  @e2e
  Scenario: Cleanup archives done tasks and removes resources
    Given tasks "git-orchard-rs#33" and "git-orchard-rs#35" have status "done"
    And they have worktrees and sessions
    When I press "c" to open the cleanup dialog
    Then I see a checkbox list of done tasks with their resources
    When I confirm cleanup
    Then for each selected task:
      | action                              |
      | tmux sessions are killed            |
      | git worktree is removed             |
      | task is removed from the state file |
    And events "session.killed", "worktree.removed", "task.archived" are logged

  # ===================================================================
  # Event log (structured JSON lines)
  # ===================================================================

  @unit
  Scenario: Event log location
    Then events are appended to "~/.local/state/git-orchard/events.jsonl"

  @unit
  Scenario: Event log entry structure
    When any event is logged
    Then it is a single JSON line with at minimum:
      | field | type   | description               |
      | ts    | string | ISO 8601 UTC timestamp    |
      | event | string | dot-namespaced event type |

  @unit
  Scenario: Event types
    Then the following event types exist:
      | event                | additional fields                                    |
      | task.created         | task, source                                         |
      | task.status_change   | task, from, to, reason                               |
      | task.archived        | task                                                 |
      | session.created      | task, session                                        |
      | session.bound        | task, session, reason                                |
      | session.orphaned     | session, path                                         |
      | session.dead         | task, session                                        |
      | session.killed       | task, session                                        |
      | session.switch       | task, session, trigger                               |
      | pane.detected        | session, pane_id, command, agent                     |
      | agent.active         | task, session, pane_id                               |
      | agent.idle           | task, session, pane_id, was_active_for               |
      | worktree.created     | task, path                                           |
      | worktree.removed     | task, path                                           |
      | refresh.complete     | duration_ms, tasks, sessions, worktrees              |
      | config.repo_added    | path                                                 |
      | error                | message, context                                     |

  @unit
  Scenario: Event log rotation
    Given the events.jsonl file exceeds 50 MB
    When a new event is logged
    Then the file is rotated to "events.jsonl.1"
    And a new events.jsonl is started
    And at most 3 rotated files are kept

  @integration
  Scenario: Session switch is logged
    Given the TUI is running
    When I press Enter to switch to session "git-orchard-rs_47_main"
    Then an event is logged:
      """json
      {"ts":"...","event":"session.switch","task":"git-orchard-rs#47","session":"git-orchard-rs_47_main","trigger":"keypress"}
      """

  @integration
  Scenario: Refresh completion is logged with timing
    When a collector refresh completes in 1823ms finding 12 tasks, 5 sessions, 8 worktrees
    Then an event is logged:
      """json
      {"ts":"...","event":"refresh.complete","duration_ms":1823,"tasks":12,"sessions":5,"worktrees":8}
      """

  @integration
  Scenario: Agent state transitions are logged
    Given session "git-orchard-rs_47_main" pane %5 was previously detected as agent-active
    When the next refresh detects pane %5 is no longer running claude
    Then an event "agent.idle" is logged with "was_active_for" duration

  # ===================================================================
  # Startup — cached state + live enrichment
  # ===================================================================

  @e2e
  Scenario: TUI shows cached state immediately on startup
    Given the state file has 12 tasks with cached status and PR info
    When the TUI starts
    Then the task list is rendered immediately from cached state
    And a background refresh begins to enrich with live data

  @integration
  Scenario: Background refresh updates cached state
    Given the TUI rendered from cached state
    When the background refresh completes
    Then tasks are updated with fresh PR status, CI results, and session liveness
    And the state file is saved with the enriched data
    And the TUI re-renders with updated information

  @integration
  Scenario: Stale session in cached state is reconciled
    Given the cached state says task #47 has session "git-orchard-rs_47_claude"
    And tmux reports that session no longer exists
    When the background refresh completes reconciliation
    Then the session is marked as dead in the TUI
    And an event "session.dead" is logged

  # ===================================================================
  # TUI layout — task-centric view
  # ===================================================================

  @e2e
  Scenario: Main view groups tasks by status
    Given tasks exist in all statuses
    When the TUI renders
    Then tasks are grouped in this order:
      | section     | display                                |
      | READY       | compact: number, title, repo           |
      | IN PROGRESS | expanded: worktree, PR, sessions/panes |
      | IN REVIEW   | semi-expanded: PR status, sessions     |
      | BACKLOG     | compact, paginated (page size 10)      |
    And done tasks are not shown (cleaned up separately)

  @e2e
  Scenario: Task line shows repo name for multi-repo
    Given tasks from repos "git-orchard-rs" and "api-server"
    When the TUI renders
    Then each task line includes the repo name

  @e2e
  Scenario: In-progress task shows session and pane detail
    Given an in-progress task with session "orchard-rs_47_main" having 2 panes
    When the TUI renders the task
    Then it shows:
      """
      3  #47  Task-centric state system             orchard-rs
              ├─ wt: ~/ws/git-orchard-rs-47
              ├─ pr: #53 ● passing ✓ approved
              └─ session: orchard-rs_47_main
                 ├─ pane 1: zsh
                 └─ pane 2: ⚡ claude active
      """

  @e2e
  Scenario: In-progress task with multiple sessions shows all
    Given an in-progress task with 2 sessions
    When the TUI renders the task
    Then it shows:
      """
      3  #47  Task-centric state system             orchard-rs
              ├─ wt: ~/ws/git-orchard-rs-47
              ├─ pr: #53 ● passing ✓ approved
              ├─ a: orchard-rs_47_main              ● you
              └─ b: orchard-rs_47_claude             ⚡ active
      """

  @e2e
  Scenario: Backlog is paginated
    Given 25 tasks with status "backlog"
    When the TUI renders
    Then the BACKLOG section shows tasks 1-10
    And a hint "j/k ▼▲" indicates scrolling is available

  # ===================================================================
  # Priority and sorting
  # ===================================================================

  @e2e
  Scenario: Tasks within a status group are sorted by priority
    Given tasks #47 (priority 2) and #48 (priority 1) both in status "ready"
    When the TUI renders
    Then #48 appears before #47

  @e2e
  Scenario: Changing task priority
    Given the cursor is on task #47
    When I press "p"
    Then a priority input is shown (1-9, 1 = highest)
    When I enter "1"
    Then the task priority is set to 1
    And the state file is saved
    And the task list re-sorts

  # ===================================================================
  # Relationship between existing debug log and event log
  # ===================================================================

  @unit
  Scenario: Debug log and event log coexist
    Then "~/.local/state/git-orchard/debug.log" contains low-level debug output (SSH commands, git output, errors)
    And "~/.local/state/git-orchard/events.jsonl" contains structured high-level events
    And they are independent — neither reads from the other

  @unit
  Scenario: Events are queryable with standard tools
    Given events.jsonl contains 1000 events
    When I run: jq 'select(.event=="agent.idle")' events.jsonl
    Then I get all agent idle events with their durations
    When I run: jq 'select(.event=="refresh.complete") | .duration_ms' events.jsonl
    Then I get all refresh durations for performance analysis
