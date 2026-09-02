//! Wire types for the daemon GraphQL responses the TUI consumes.
//!
//! These are deliberately narrow — only the fields the TUI reads. They mirror
//! `schema.graphql` (see ADR-011) but ignore everything else. Adding a field
//! here is cheap; just ask for it in the query string and bump the struct.
//!
//! # Key types
//!
//! - [`WorkViewSnapshot`] — the primary composite read for TUI refresh. One
//!   round-trip to the daemon yields everything the TUI needs for local data.
//! - [`GraphQlResponse`] / [`GraphQlError`] — generic envelope wrapping every
//!   query response.

use serde::{Deserialize, Deserializer, Serialize};

// ---------------------------------------------------------------------------
// Private helper types for GraphQL object→scalar flattening
//
// The daemon schema exposes some fields as object/list types (e.g. `labels`,
// `windows`, `attachedClients`, `currentWindow`) even when the TUI only needs
// a scalar projection (label names, counts, window name). These thin structs
// serve as deserialization intermediaries so the public struct fields can stay
// as ergonomic scalars (`Vec<String>`, `u32`, `Option<String>`).
// ---------------------------------------------------------------------------

/// Minimum projection of a GraphQL `Label` object — carries only `name`.
#[derive(Deserialize)]
struct LabelNode {
    name: String,
}

/// Minimum projection of a GraphQL `TmuxClient` object — carries only `id`
/// so we can count the list length.
#[derive(Deserialize)]
struct ClientNode {
    #[allow(dead_code)]
    id: String,
}

/// Minimum projection of a GraphQL `TmuxWindow` object — carries only `name`.
#[derive(Deserialize)]
struct WindowNode {
    name: String,
}

/// Minimum projection of a GraphQL `TmuxPane` object — carries only `id`.
#[derive(Deserialize)]
struct PaneNode {
    id: String,
}

/// Minimum projection of a GraphQL `Process` object — carries only `command`.
/// `Process.command` is the basename (e.g. `"claude"`), matching the semantic
/// of `ClaudeInstance.process` as used downstream.
#[derive(Deserialize)]
struct ProcessNode {
    command: String,
}

/// Deserialises `TmuxPane` (nullable) → `Option<String>` (extracts `id`).
fn id_from_pane<'de, D>(de: D) -> Result<String, D::Error>
where
    D: Deserializer<'de>,
{
    let node: PaneNode = PaneNode::deserialize(de)?;
    Ok(node.id)
}

/// Deserialises `Process` (nullable) → `String` (extracts `command`).
fn command_from_process<'de, D>(de: D) -> Result<String, D::Error>
where
    D: Deserializer<'de>,
{
    let node: ProcessNode = ProcessNode::deserialize(de)?;
    Ok(node.command)
}

/// Deserialises `[Label!]!` → `Vec<String>` (extracts each label's `name`).
fn labels_from_objects<'de, D>(de: D) -> Result<Vec<String>, D::Error>
where
    D: Deserializer<'de>,
{
    let nodes: Vec<LabelNode> = Vec::deserialize(de)?;
    Ok(nodes.into_iter().map(|l| l.name).collect())
}

/// Deserialises `[TmuxClient!]!` → `u32` (returns the list length).
fn count_clients<'de, D>(de: D) -> Result<u32, D::Error>
where
    D: Deserializer<'de>,
{
    let nodes: Vec<ClientNode> = Vec::deserialize(de)?;
    Ok(nodes.len() as u32)
}

/// Deserialises `[TmuxWindow!]!` → `u32` (returns the list length).
fn count_windows<'de, D>(de: D) -> Result<u32, D::Error>
where
    D: Deserializer<'de>,
{
    let nodes: Vec<WindowNode> = Vec::deserialize(de)?;
    Ok(nodes.len() as u32)
}

/// Deserialises `TmuxWindow` (nullable) → `Option<String>` (extracts `name`).
fn name_from_window<'de, D>(de: D) -> Result<Option<String>, D::Error>
where
    D: Deserializer<'de>,
{
    let node: Option<WindowNode> = Option::deserialize(de)?;
    Ok(node.map(|w| w.name))
}

/// Top-level GraphQL response envelope.
#[derive(Debug, Deserialize)]
pub struct GraphQlResponse<T> {
    /// Resolved data, when the request succeeded.
    pub data: Option<T>,

    /// GraphQL-level errors. May coexist with partial `data`.
    #[serde(default)]
    pub errors: Vec<GraphQlError>,
}

/// One GraphQL error entry.
#[derive(Debug, Deserialize)]
pub struct GraphQlError {
    /// Human-readable error message.
    pub message: String,
}

/// `Query.health` payload — used as a connectivity probe.
#[derive(Debug, Deserialize)]
pub struct HealthPayload {
    /// `health` field result.
    pub health: Health,
}

/// Health node.
#[derive(Debug, Deserialize)]
pub struct Health {
    /// Status string, "ok" when serving.
    pub status: String,
    /// Daemon uptime in seconds.
    #[serde(rename = "uptimeS")]
    pub uptime_s: i64,
}

/// `Query.tmuxSessions` payload — local sessions on the queried daemon.
#[derive(Debug, Deserialize)]
pub struct TmuxSessionsPayload {
    /// `tmuxSessions` field result.
    #[serde(rename = "tmuxSessions")]
    pub tmux_sessions: Vec<TmuxSession>,
}

/// One tmux session as exposed by the daemon. Narrow projection.
#[derive(Debug, Clone, Deserialize)]
pub struct TmuxSession {
    /// Globally-unique node id (`TmuxSession:<host>:<sessionName>`).
    pub id: String,

    /// Session name as known to the tmux server.
    pub name: String,

    /// True when at least one client is attached.
    #[serde(default)]
    pub attached: bool,

    /// True when an attached client has been active recently.
    #[serde(default, rename = "activeAttached")]
    pub active_attached: bool,

    /// RFC3339 timestamp of most recent activity. Optional in v1.
    #[serde(default, rename = "lastActivityAt")]
    pub last_activity_at: Option<String>,
}

/// `Query.host` payload — local host metadata.
#[derive(Debug, Deserialize)]
pub struct HostPayload {
    /// `host` field result.
    pub host: HostNode,
}

/// `Query.hosts` payload — every host the daemon knows about (local + peers).
#[derive(Debug, Deserialize)]
pub struct HostsPayload {
    /// `hosts` field result.
    pub hosts: Vec<HostNode>,
}

/// One host as exposed by the daemon. Narrow projection.
#[derive(Debug, Clone, Deserialize)]
pub struct HostNode {
    /// Globally-unique node id (`Host:<machineId>`).
    pub id: String,

    /// OS-reported hostname.
    pub hostname: String,

    /// Reachable network address. Null for the local host; populated for peers.
    #[serde(default)]
    pub address: Option<String>,

    /// True when the daemon has reached the host recently.
    #[serde(default)]
    pub reachable: bool,

    /// Peer hosts this daemon federates with.
    #[serde(default)]
    pub peers: Vec<HostNode>,
}

// ============================================================
//  WorkView — composite read for TUI local-data refresh
// ============================================================

/// Top-level `Query.workView` payload — one round-trip supplies everything
/// the TUI needs for local worktrees, tmux sessions, and Claude instances.
#[derive(Debug, Deserialize)]
pub struct WorkViewPayload {
    /// `workView` field result.
    #[serde(rename = "workView")]
    pub work_view: WorkViewSnapshot,
}

/// Complete snapshot of local orchard state as delivered by the daemon's
/// `workView` query. This is the primary input for Phase 1 TUI refresh.
///
/// All three top-level collections are always present (they may be empty
/// vectors when there is nothing to report).
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct WorkViewSnapshot {
    /// One entry per repo the daemon knows about.
    pub repos: Vec<WorkViewRepo>,

    /// All tmux sessions on the local host.
    #[serde(rename = "tmuxSessions")]
    pub tmux_sessions: Vec<WorkViewTmuxSession>,

    /// All Claude Code instances on the local host.
    #[serde(rename = "claudeInstances")]
    pub claude_instances: Vec<ClaudeInstance>,
}

/// One repo as exposed by `workView` (ADR-015 shape).
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct WorkViewRepo {
    /// GitHub-style `owner/repo` slug — the repo's identity.
    pub slug: String,

    /// Absolute path to the working tree.
    pub path: String,

    /// Worktrees belonging to this repo.
    pub worktrees: Vec<WorkViewWorktree>,
}

/// One git worktree as exposed by `workView`. PR and issue joins are
/// performed **daemon-side** and delivered pre-enriched.
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct WorkViewWorktree {
    /// Absolute path to the worktree on disk.
    pub path: String,

    /// Active branch name (`refs/heads/…` stripped).
    pub branch: String,

    /// HEAD commit SHA.
    pub head: String,

    /// True for bare worktrees (the linked-worktree root).
    pub bare: bool,

    /// Host identifier. In daemon v1 this is always `"local"`.
    pub host: String,

    /// Repository slug (`owner/repo`).
    pub repo: String,

    /// Commits ahead of upstream. `None` when no upstream is configured, HEAD
    /// is detached, or the count could not be determined (#483).
    #[serde(default)]
    pub ahead: Option<u32>,

    /// Commits behind upstream. Same null semantics as `ahead`.
    #[serde(default)]
    pub behind: Option<u32>,

    /// Open PR whose head branch matches this worktree's branch. `None` when
    /// there is no open matching PR. Note: closed/merged PRs are **not**
    /// included in v1 (see research/035 for the schema limitation).
    pub pr: Option<WorkViewPr>,

    /// Issue linked to this worktree (via PR closing keywords or branch
    /// convention). `None` when no issue is detected.
    pub issue: Option<WorkViewIssue>,
}

/// Pull request fields carried in a `workView` worktree. Narrow projection —
/// only the badges the TUI renders.
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct WorkViewPr {
    /// GitHub PR number.
    pub number: u64,

    /// PR state string: `"OPEN"`, `"CLOSED"`, or `"MERGED"`.
    pub state: String,

    /// PR title.
    pub title: String,

    /// Aggregated CI status: `"SUCCESS"`, `"FAILURE"`, `"PENDING"`, etc.
    #[serde(default, rename = "statusCheckRollup")]
    pub status_check_rollup: Option<String>,

    /// Latest review decision: `"APPROVED"`, `"CHANGES_REQUESTED"`, etc.
    #[serde(default, rename = "reviewDecision")]
    pub review_decision: Option<String>,

    /// GitHub merge-state status: `"CLEAN"`, `"BLOCKED"`, `"DIRTY"`, etc.
    #[serde(default, rename = "mergeStateStatus")]
    pub merge_state_status: Option<String>,

    /// GitHub mergeable state: `"MERGEABLE"`, `"CONFLICTING"`, or `"UNKNOWN"`.
    ///
    /// Used to derive `has_conflicts`: only `"CONFLICTING"` indicates actual
    /// file-level conflicts. `merge_state_status == "BLOCKED"` can mean CI
    /// failure, missing reviews, etc. — not conflicts. Mirror of the pre-rip
    /// `cache_sources` path which checked `mergeable == "CONFLICTING"`.
    #[serde(default)]
    pub mergeable: Option<String>,

    /// Whether the PR is a draft.
    #[serde(default)]
    pub draft: bool,

    /// Label names applied to the PR.
    ///
    /// The daemon returns `[Label!]!` objects; we flatten to name strings via
    /// [`labels_from_objects`] so consumers see a plain `Vec<String>`.
    #[serde(default, deserialize_with = "labels_from_objects")]
    pub labels: Vec<String>,

    /// Review threads that still block a merge: unresolved and not outdated.
    ///
    /// Defaults to 0 so an older daemon that predates the field (#607) still
    /// deserializes — the PR then reads as having nothing outstanding, which
    /// matches the behaviour before the field existed.
    #[serde(default, rename = "unresolvedThreadCount")]
    pub unresolved_thread_count: u32,
}

/// Issue fields carried in a `workView` worktree. Narrow projection.
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct WorkViewIssue {
    /// GitHub issue number.
    pub number: u64,

    /// Issue state string: `"OPEN"` or `"CLOSED"`.
    pub state: String,

    /// Issue title.
    pub title: String,

    /// Label names applied to the issue.
    ///
    /// The daemon returns `[Label!]!` objects; we flatten to name strings via
    /// [`labels_from_objects`] so consumers see a plain `Vec<String>`.
    #[serde(default, deserialize_with = "labels_from_objects")]
    pub labels: Vec<String>,
}

/// One tmux session as exposed by `workView.tmuxSessions`. Extends the
/// existing [`TmuxSession`] with window metadata needed for session display.
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct WorkViewTmuxSession {
    /// Globally-unique node id (`TmuxSession:<host>:<sessionName>`).
    pub id: String,

    /// Session name as known to the tmux server.
    pub name: String,

    /// True when at least one client is attached.
    #[serde(default)]
    pub attached: bool,

    /// True when an attached client has been active recently.
    #[serde(default, rename = "activeAttached")]
    pub active_attached: bool,

    /// RFC3339 timestamp of most recent activity.
    #[serde(default, rename = "lastActivityAt")]
    pub last_activity_at: Option<String>,

    /// Number of clients currently attached.
    ///
    /// The daemon returns `[TmuxClient!]!` objects; we flatten to a count via
    /// [`count_clients`] so consumers see a plain `u32`.
    #[serde(
        default,
        rename = "attachedClients",
        deserialize_with = "count_clients"
    )]
    pub attached_clients: u32,

    /// Number of windows in this session.
    ///
    /// The daemon returns `[TmuxWindow!]!` objects; we flatten to a count via
    /// [`count_windows`] so consumers see a plain `u32`.
    #[serde(default, deserialize_with = "count_windows")]
    pub windows: u32,

    /// Name of the currently active window.
    ///
    /// The daemon returns a `TmuxWindow` object (nullable); we flatten to its
    /// `name` string via [`name_from_window`].
    #[serde(
        default,
        rename = "currentWindow",
        deserialize_with = "name_from_window"
    )]
    pub current_window: Option<String>,

    /// Working directory of the session's active pane. Used by the client-side
    /// adapter to match sessions to worktrees via [`crate::paths::session_belongs_to_worktree`].
    ///
    /// Absent in older daemon versions; falls back to name-based matching when `None`.
    #[serde(default)]
    pub path: Option<String>,
}

/// One Claude Code instance as exposed by `workView.claudeInstances`.
///
/// The sessions↔claude join is performed **client-side** by walking
/// `ClaudeInstance.pane → TmuxPane → TmuxSession`. No daemon-side join field
/// is available for this relationship.
#[derive(Debug, Deserialize, Serialize, Clone)]
pub struct ClaudeInstance {
    /// Globally-unique node id.
    pub id: String,

    /// Pane reference used to locate the tmux session this instance lives in.
    /// Format: `TmuxPane:<host>:<session>:<window>:<pane>`.
    /// Flattened from the daemon's `TmuxPane` object via [`id_from_pane`].
    #[serde(deserialize_with = "id_from_pane")]
    pub pane: String,

    /// Process command basename (e.g. `"claude"`).
    /// Flattened from the daemon's `Process` object via [`command_from_process`].
    #[serde(deserialize_with = "command_from_process")]
    pub process: String,

    /// Current activity state: `"idle"`, `"working"`, `"waiting"`, etc.
    pub state: String,

    /// Claude session UUID for resuming sessions.
    #[serde(default, rename = "sessionUuid")]
    pub session_uuid: Option<String>,

    /// Whether RC file integration is enabled for this instance.
    #[serde(default, rename = "rcEnabled")]
    pub rc_enabled: bool,

    /// RFC3339 timestamp of most recent activity.
    #[serde(default, rename = "lastActivityAt")]
    pub last_activity_at: Option<String>,

    /// Claude model in use for this session, e.g. `"claude-opus-4-7"`.
    /// `None` when the daemon has not yet observed an assistant message.
    /// Sourced from the conversation jsonl (issue #603 phase 2).
    #[serde(default)]
    pub model: Option<String>,

    /// Number of tool_use calls open (sent by assistant, no matching
    /// tool_result) in the current turn. Sourced from the conversation
    /// jsonl (issue #603 phase 2). Zero when no jsonl is found.
    #[serde(default, rename = "inflightToolCount")]
    pub inflight_tool_count: i32,
}

// ============================================================
//  worktreesCleanup mutation — response types
// ============================================================

/// Top-level `Mutation.worktreesCleanup` payload.
#[derive(Debug, Deserialize)]
pub struct WorktreesCleanupPayload {
    /// `worktreesCleanup` field result.
    #[serde(rename = "worktreesCleanup")]
    pub worktrees_cleanup: WorktreesCleanupResult,
}

/// Result of `Mutation.worktreesCleanup`.
///
/// `ok` is `true` even when some individual entries failed — per-worktree
/// failure is reported in the `entries` vec. `ok` is `false` only when
/// the batch call itself failed (e.g. invalid input, systemic error).
#[derive(Debug, Deserialize)]
pub struct WorktreesCleanupResult {
    /// True when the batch call itself succeeded (input valid, no systemic errors).
    pub ok: bool,
    /// Per-worktree result entries. Present even when `ok` is `false` at the
    /// input-validation level (entries will be empty in that case).
    #[serde(default)]
    pub entries: Vec<WorktreeCleanupEntry>,
    /// Typed input validation error code; set when `ok` is `false`.
    #[serde(rename = "errCode", default)]
    pub err_code: Option<String>,
    /// Human-readable error message; set when `ok` is `false`.
    #[serde(rename = "errMsg", default)]
    pub err_msg: Option<String>,
}

/// Per-worktree result inside [`WorktreesCleanupResult`].
#[derive(Debug, Deserialize)]
pub struct WorktreeCleanupEntry {
    /// The stable worktree ID this entry describes.
    #[serde(rename = "worktreeId")]
    pub worktree_id: String,
    /// True when cleanup succeeded or was a clean no-op (idempotent re-run).
    pub ok: bool,
    /// The stage that failed when `ok` is `false`.
    /// One of: `"worktree-remove"`, `"branch-delete"`, `"docker-teardown"`.
    #[serde(default)]
    pub stage: Option<String>,
    /// Human-readable failure or skip message.
    #[serde(default)]
    pub message: Option<String>,
    /// True when this worktree was already removed before this call.
    ///
    /// The daemon emits `null` (not `false`) when the worktree was actively
    /// removed this call — the GraphQL schema declares this field `Boolean`
    /// (nullable). `None` and `Some(false)` both mean "not already removed";
    /// only `Some(true)` means the worktree was already gone on entry.
    #[serde(rename = "alreadyRemoved", default)]
    pub already_removed: Option<bool>,
    /// Non-fatal per-stage warnings (e.g. branch-skip, tmux-kill failure).
    #[serde(default)]
    pub warnings: Vec<String>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_health_envelope() {
        let raw = r#"{"data":{"health":{"status":"ok","uptimeS":42}}}"#;
        let env: GraphQlResponse<HealthPayload> = serde_json::from_str(raw).unwrap();
        let h = env.data.unwrap().health;
        assert_eq!(h.status, "ok");
        assert_eq!(h.uptime_s, 42);
    }

    #[test]
    fn parses_tmux_sessions() {
        let raw = r#"{
            "data": {
                "tmuxSessions": [
                    {"id":"TmuxSession:H:a","name":"a","attached":true,"activeAttached":true,"lastActivityAt":"2026-05-05T12:00:00Z"},
                    {"id":"TmuxSession:H:b","name":"b","attached":false,"activeAttached":false}
                ]
            }
        }"#;
        let env: GraphQlResponse<TmuxSessionsPayload> = serde_json::from_str(raw).unwrap();
        let sessions = env.data.unwrap().tmux_sessions;
        assert_eq!(sessions.len(), 2);
        assert_eq!(sessions[0].name, "a");
        assert!(sessions[0].attached);
        assert!(!sessions[1].active_attached);
        assert!(sessions[1].last_activity_at.is_none());
    }

    #[test]
    fn parses_hosts_with_peers() {
        let raw = r#"{
            "data": {
                "hosts": [
                    {"id":"Host:local","hostname":"local","address":null,"reachable":true,
                     "peers":[
                        {"id":"Host:p1","hostname":"box-1","address":"box-1.boxd.sh","reachable":true,"peers":[]}
                     ]
                    }
                ]
            }
        }"#;
        let env: GraphQlResponse<HostsPayload> = serde_json::from_str(raw).unwrap();
        let hosts = env.data.unwrap().hosts;
        assert_eq!(hosts.len(), 1);
        assert_eq!(hosts[0].hostname, "local");
        assert!(hosts[0].address.is_none());
        assert_eq!(hosts[0].peers.len(), 1);
        assert_eq!(hosts[0].peers[0].address.as_deref(), Some("box-1.boxd.sh"));
    }

    #[test]
    fn surfaces_graphql_errors() {
        let raw = r#"{"errors":[{"message":"introspection disabled"}],"data":null}"#;
        let env: GraphQlResponse<HealthPayload> = serde_json::from_str(raw).unwrap();
        assert!(env.data.is_none());
        assert_eq!(env.errors.len(), 1);
        assert_eq!(env.errors[0].message, "introspection disabled");
    }

    // -----------------------------------------------------------------------
    //  WorkViewSnapshot parser tests
    // -----------------------------------------------------------------------

    /// Fixture representing a minimal valid `workView` GraphQL response. Covers
    /// all fields required by the Phase 1 implementation: one project with one
    /// worktree carrying a PR+issue join, one tmux session with window metadata,
    /// and one Claude instance.
    const WORK_VIEW_FIXTURE: &str = r#"{
        "data": {
            "workView": {
                "repos": [
                    {
                        "slug": "orchardist",
                        "path": "/home/example/workspace/orchardist",
                        "worktrees": [
                            {
                                "path": "/home/example/workspace/orchardist/.worktrees/issue429/rip-cache",
                                "branch": "issue429/rip-cache-sources",
                                "head": "abc1234def5678",
                                "bare": false,
                                "host": "local",
                                "repo": "drewdrewthis/orchardist",
                                "pr": {
                                    "number": 429,
                                    "state": "OPEN",
                                    "title": "Rip cache_sources from TUI",
                                    "statusCheckRollup": "SUCCESS",
                                    "reviewDecision": "APPROVED",
                                    "mergeStateStatus": "CLEAN",
                                    "draft": false,
                                    "labels": [
                                        {"name": "enhancement"},
                                        {"name": "phase-1"}
                                    ]
                                },
                                "issue": {
                                    "number": 429,
                                    "state": "OPEN",
                                    "title": "Rip cache_sources from TUI dashboard refresh"
                                }
                            },
                            {
                                "path": "/home/example/workspace/orchardist",
                                "branch": "main",
                                "head": "deadbeef",
                                "bare": false,
                                "host": "local",
                                "repo": "drewdrewthis/orchardist",
                                "pr": null,
                                "issue": null
                            }
                        ]
                    }
                ],
                "tmuxSessions": [
                    {
                        "id": "TmuxSession:local:issue429",
                        "name": "issue429",
                        "attached": true,
                        "activeAttached": true,
                        "lastActivityAt": "2026-05-08T10:00:00Z",
                        "attachedClients": [{"id": "TmuxClient:local:/dev/ttys001"}],
                        "windows": [
                            {"name": "shell"},
                            {"name": "editor"},
                            {"name": "logs"}
                        ],
                        "currentWindow": {"name": "editor"}
                    }
                ],
                "claudeInstances": [
                    {
                        "id": "ClaudeInstance:local:12345",
                        "pane": {"id": "TmuxPane:local:issue429:editor:0"},
                        "process": {"command": "claude"},
                        "state": "working",
                        "sessionUuid": "550e8400-e29b-41d4-a716-446655440000",
                        "rcEnabled": true,
                        "lastActivityAt": "2026-05-08T10:01:00Z"
                    }
                ]
            }
        }
    }"#;

    #[test]
    fn parses_work_view_snapshot_envelope() {
        let env: GraphQlResponse<WorkViewPayload> =
            serde_json::from_str(WORK_VIEW_FIXTURE).unwrap();
        let snapshot = env.data.unwrap().work_view;

        // repos
        assert_eq!(snapshot.repos.len(), 1);
        let repo = &snapshot.repos[0];
        assert_eq!(repo.slug, "orchardist");
        assert_eq!(repo.path, "/home/example/workspace/orchardist");

        // worktrees
        assert_eq!(repo.worktrees.len(), 2);
        let wt = &repo.worktrees[0];
        assert_eq!(wt.branch, "issue429/rip-cache-sources");
        assert_eq!(wt.head, "abc1234def5678");
        assert!(!wt.bare);
        assert_eq!(wt.host, "local");
        assert_eq!(wt.repo, "drewdrewthis/orchardist");

        // worktree without PR/issue
        assert!(repo.worktrees[1].pr.is_none());
        assert!(repo.worktrees[1].issue.is_none());
    }

    #[test]
    fn parses_work_view_pr_fields() {
        let env: GraphQlResponse<WorkViewPayload> =
            serde_json::from_str(WORK_VIEW_FIXTURE).unwrap();
        let wt = &env.data.unwrap().work_view.repos[0].worktrees[0];
        let pr = wt.pr.as_ref().unwrap();

        assert_eq!(pr.number, 429);
        assert_eq!(pr.state, "OPEN");
        assert_eq!(pr.title, "Rip cache_sources from TUI");
        assert_eq!(pr.status_check_rollup.as_deref(), Some("SUCCESS"));
        assert_eq!(pr.review_decision.as_deref(), Some("APPROVED"));
        assert_eq!(pr.merge_state_status.as_deref(), Some("CLEAN"));
        assert!(!pr.draft);
        assert_eq!(pr.labels, vec!["enhancement", "phase-1"]);
    }

    #[test]
    fn parses_work_view_issue_fields() {
        let env: GraphQlResponse<WorkViewPayload> =
            serde_json::from_str(WORK_VIEW_FIXTURE).unwrap();
        let wt = &env.data.unwrap().work_view.repos[0].worktrees[0];
        let issue = wt.issue.as_ref().unwrap();

        assert_eq!(issue.number, 429);
        assert_eq!(issue.state, "OPEN");
        assert_eq!(issue.title, "Rip cache_sources from TUI dashboard refresh");
    }

    #[test]
    fn parses_work_view_tmux_session_fields() {
        let env: GraphQlResponse<WorkViewPayload> =
            serde_json::from_str(WORK_VIEW_FIXTURE).unwrap();
        let snapshot = env.data.unwrap().work_view;
        assert_eq!(snapshot.tmux_sessions.len(), 1);
        let s = &snapshot.tmux_sessions[0];

        assert_eq!(s.id, "TmuxSession:local:issue429");
        assert_eq!(s.name, "issue429");
        assert!(s.attached);
        assert!(s.active_attached);
        assert_eq!(s.last_activity_at.as_deref(), Some("2026-05-08T10:00:00Z"));
        assert_eq!(s.attached_clients, 1);
        assert_eq!(s.windows, 3);
        assert_eq!(s.current_window.as_deref(), Some("editor"));
    }

    #[test]
    fn parses_work_view_claude_instance_fields() {
        let env: GraphQlResponse<WorkViewPayload> =
            serde_json::from_str(WORK_VIEW_FIXTURE).unwrap();
        let snapshot = env.data.unwrap().work_view;
        assert_eq!(snapshot.claude_instances.len(), 1);
        let ci = &snapshot.claude_instances[0];

        assert_eq!(ci.id, "ClaudeInstance:local:12345");
        assert_eq!(ci.pane, "TmuxPane:local:issue429:editor:0");
        assert_eq!(ci.process, "claude");
        assert_eq!(ci.state, "working");
        assert_eq!(
            ci.session_uuid.as_deref(),
            Some("550e8400-e29b-41d4-a716-446655440000")
        );
        assert!(ci.rc_enabled);
        assert_eq!(ci.last_activity_at.as_deref(), Some("2026-05-08T10:01:00Z"));
    }

    #[test]
    fn parses_work_view_optional_fields_absent() {
        // Minimal WorkViewSnapshot: empty collections, optional fields absent.
        let raw = r#"{
            "data": {
                "workView": {
                    "repos": [],
                    "tmuxSessions": [],
                    "claudeInstances": []
                }
            }
        }"#;
        let env: GraphQlResponse<WorkViewPayload> = serde_json::from_str(raw).unwrap();
        let snapshot = env.data.unwrap().work_view;
        assert!(snapshot.repos.is_empty());
        assert!(snapshot.tmux_sessions.is_empty());
        assert!(snapshot.claude_instances.is_empty());
    }

    #[test]
    fn parses_work_view_pr_optional_fields_default() {
        // PR with only required fields — optional fields should default cleanly.
        let raw = r#"{
            "data": {
                "workView": {
                    "repos": [{
                        "slug": "repo",
                        "path": "/repo",
                        "worktrees": [{
                            "path": "/repo/wt",
                            "branch": "feat/x",
                            "head": "cafe",
                            "bare": false,
                            "host": "local",
                            "repo": "owner/repo",
                            "pr": {
                                "number": 1,
                                "state": "OPEN",
                                "title": "Fix it"
                            },
                            "issue": null
                        }]
                    }],
                    "tmuxSessions": [],
                    "claudeInstances": []
                }
            }
        }"#;
        let env: GraphQlResponse<WorkViewPayload> = serde_json::from_str(raw).unwrap();
        let wt = &env.data.unwrap().work_view.repos[0].worktrees[0];
        let pr = wt.pr.as_ref().unwrap();
        assert!(pr.status_check_rollup.is_none());
        assert!(pr.review_decision.is_none());
        assert!(pr.merge_state_status.is_none());
        assert!(!pr.draft);
        assert!(pr.labels.is_empty());
    }

    // -----------------------------------------------------------------------
    //  WorktreesCleanup null-tolerance tests
    // -----------------------------------------------------------------------

    /// `alreadyRemoved: null` (the normal successful-cleanup case) must
    /// deserialize to `already_removed: None` without error.  This is the
    /// regression test for the bug introduced in commit e2b763d where the
    /// field was typed `bool` and `#[serde(default)]` only filled in absent
    /// keys — an explicit JSON `null` produced a deserialization error.
    #[test]
    fn cleanup_entry_already_removed_null_is_none() {
        let raw = r#"{
            "worktreeId": "repo:feat/x",
            "ok": true,
            "stage": null,
            "message": null,
            "alreadyRemoved": null,
            "warnings": []
        }"#;
        let entry: WorktreeCleanupEntry = serde_json::from_str(raw).unwrap();
        assert_eq!(entry.worktree_id, "repo:feat/x");
        assert!(entry.ok);
        // null → None, treated as "not already removed"
        assert_eq!(entry.already_removed, None);
        assert!(!entry.already_removed.unwrap_or(false));
    }

    /// `alreadyRemoved: true` must deserialize to `Some(true)`.
    #[test]
    fn cleanup_entry_already_removed_true_is_some_true() {
        let raw = r#"{
            "worktreeId": "repo:feat/y",
            "ok": true,
            "alreadyRemoved": true,
            "warnings": []
        }"#;
        let entry: WorktreeCleanupEntry = serde_json::from_str(raw).unwrap();
        assert_eq!(entry.already_removed, Some(true));
        assert!(entry.already_removed.unwrap_or(false));
    }

    /// `alreadyRemoved` key absent must deserialize to `None` (no regression
    /// on the previous `#[serde(default)]` behavior for absent keys).
    #[test]
    fn cleanup_entry_already_removed_absent_is_none() {
        let raw = r#"{
            "worktreeId": "repo:feat/z",
            "ok": true,
            "warnings": []
        }"#;
        let entry: WorktreeCleanupEntry = serde_json::from_str(raw).unwrap();
        assert_eq!(entry.already_removed, None);
    }

    /// Full `WorktreesCleanupResult` envelope with a null `alreadyRemoved`
    /// entry — round-trips the actual GraphQL response shape.
    #[test]
    fn cleanup_result_envelope_null_already_removed() {
        let raw = r#"{
            "data": {
                "worktreesCleanup": {
                    "ok": true,
                    "entries": [
                        {
                            "worktreeId": "myrepo:issue/123",
                            "ok": true,
                            "stage": null,
                            "message": null,
                            "alreadyRemoved": null,
                            "warnings": []
                        }
                    ]
                }
            }
        }"#;
        let env: GraphQlResponse<WorktreesCleanupPayload> = serde_json::from_str(raw).unwrap();
        let result = env.data.unwrap().worktrees_cleanup;
        assert!(result.ok);
        assert_eq!(result.entries.len(), 1);
        let entry = &result.entries[0];
        assert_eq!(entry.worktree_id, "myrepo:issue/123");
        assert!(entry.ok);
        assert_eq!(entry.already_removed, None);
        assert!(!entry.already_removed.unwrap_or(false));
    }
}
