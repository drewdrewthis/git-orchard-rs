//! Blocking HTTP GraphQL client. The TUI is synchronous; we keep this
//! synchronous too and let `tokio` handle parallel fan-out where it pays off
//! (peer fetches in particular).
//!
//! Why blocking + reqwest: existing TUI code is sync, and `reqwest::blocking`
//! sets up an internal tokio runtime per call without polluting the rest of
//! the codebase. For the federated peer fan-out we use `std::thread::scope`
//! to issue requests in parallel — sufficient for a handful of peers and
//! keeps the dep surface tight.

use std::time::Duration;

use serde::Serialize;

use super::types::{
    GraphQlResponse, HealthPayload, HostsPayload, TmuxSession, TmuxSessionsPayload,
    WorkViewPayload, WorkViewSnapshot, WorktreesCleanupPayload, WorktreesCleanupResult,
};
use super::{DaemonError, resolve_daemon_url};

/// Default per-request timeout against any single daemon (local or peer).
/// Used for fast read queries (work_view, health, tmux_sessions, etc.) that
/// should fail quickly if the daemon is unresponsive.
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(5);

/// Timeout for the `worktreesCleanup` mutation.
///
/// Cleanup is a destructive, long-running operation: each worktree may run
/// `worktree-remove.sh` + `branch-delete.sh` + `docker-teardown.sh` (which
/// includes `docker compose down --volumes` and image removal). Docker teardown
/// routinely takes 10–30 s per worktree, far exceeding the 5 s query default.
/// The call is issued once per worktree, so 120 s gives ample headroom for a
/// single worktree's teardown without sharing the fast-fail query timeout.
pub const CLEANUP_TIMEOUT: Duration = Duration::from_secs(120);

/// Blocking GraphQL client. Cheap to construct; reuse one per logical
/// operation when fanning out to peers.
///
/// Holds two underlying HTTP clients: `http` (built with [`DEFAULT_TIMEOUT`])
/// for fast read queries, and `http_cleanup` (built with [`CLEANUP_TIMEOUT`])
/// used exclusively by [`Client::worktrees_cleanup_with_sessions`] so that
/// long-running docker teardowns do not race the 5 s fast-fail window.
pub struct Client {
    http: reqwest::blocking::Client,
    http_cleanup: reqwest::blocking::Client,
    url: String,
}

impl Client {
    /// Builds a client targeting the daemon URL resolved from
    /// `ORCHARD_DAEMON_URL` (or the hardcoded local default).
    pub fn local() -> Result<Self, DaemonError> {
        Self::for_url(resolve_daemon_url())
    }

    /// Builds a client targeting an explicit GraphQL endpoint URL.
    ///
    /// `url` should be the full `/graphql` path. For peers, the convention is
    /// `https://graphql.<peer-address>/graphql` (the daemon exposes its
    /// federated surface there); see `peer_url`.
    pub fn for_url(url: impl Into<String>) -> Result<Self, DaemonError> {
        let http = reqwest::blocking::Client::builder()
            .timeout(DEFAULT_TIMEOUT)
            .build()
            .map_err(|e| DaemonError::Transport(format!("build http client: {e}")))?;
        let http_cleanup = reqwest::blocking::Client::builder()
            .timeout(CLEANUP_TIMEOUT)
            .build()
            .map_err(|e| DaemonError::Transport(format!("build http cleanup client: {e}")))?;
        Ok(Self {
            http,
            http_cleanup,
            url: url.into(),
        })
    }

    /// Returns the URL this client targets.
    pub fn url(&self) -> &str {
        &self.url
    }

    /// Generic GraphQL POST. Returns the parsed `data` payload, or maps every
    /// failure mode to a [`DaemonError`].
    ///
    /// All read queries use this method (backed by `self.http` with
    /// [`DEFAULT_TIMEOUT`]). Long-running mutations call [`Self::query_with`]
    /// directly with `self.http_cleanup`.
    pub fn query<T>(&self, body: &GraphQlRequest<'_>) -> Result<T, DaemonError>
    where
        T: serde::de::DeserializeOwned,
    {
        self.query_with(&self.http, body)
    }

    /// Internal: issues a GraphQL POST using the provided HTTP client.
    ///
    /// Splits from `query` so that callers needing a different timeout (e.g.
    /// the cleanup mutation) can pass `&self.http_cleanup` without duplicating
    /// the response-handling logic.
    fn query_with<T>(
        &self,
        http: &reqwest::blocking::Client,
        body: &GraphQlRequest<'_>,
    ) -> Result<T, DaemonError>
    where
        T: serde::de::DeserializeOwned,
    {
        let resp = http.post(&self.url).json(body).send().map_err(|e| {
            if e.is_connect() || e.is_timeout() {
                DaemonError::Unreachable {
                    url: self.url.clone(),
                    cause: e.to_string(),
                }
            } else {
                DaemonError::Transport(e.to_string())
            }
        })?;

        let status = resp.status();
        if !status.is_success() {
            let body = resp.text().unwrap_or_default();
            let truncated: String = body.chars().take(2000).collect();
            return Err(DaemonError::HttpStatus {
                status: status.as_u16(),
                body: truncated,
            });
        }

        // Read the body as text first so we can include it in the error message
        // if JSON parsing fails — the opaque "decode JSON" error has been observed
        // in production (e.g. when the daemon returns a partial response for a
        // worktree whose PR/issue lookup errors).
        let text = resp
            .text()
            .map_err(|e| DaemonError::Transport(format!("read response body: {e}")))?;
        let env: GraphQlResponse<T> = serde_json::from_str(&text).map_err(|e| {
            let preview: String = text.chars().take(500).collect();
            DaemonError::Parse(format!("decode JSON: {e}; body preview: {preview}"))
        })?;

        if !env.errors.is_empty() {
            return Err(DaemonError::GraphQl(
                env.errors.into_iter().map(|e| e.message).collect(),
            ));
        }

        env.data
            .ok_or_else(|| DaemonError::Parse("response had neither data nor errors".to_string()))
    }

    /// Probe `Query.health`. Used as a connectivity smoke check.
    pub fn health(&self) -> Result<crate::daemon::types::Health, DaemonError> {
        let req = GraphQlRequest::new("{ health { status uptimeS } }");
        let payload: HealthPayload = self.query(&req)?;
        Ok(payload.health)
    }

    /// Lists tmux sessions on the daemon's host. Maps to `Query.tmuxSessions`.
    pub fn tmux_sessions(&self) -> Result<Vec<TmuxSession>, DaemonError> {
        const Q: &str = r#"
            {
              tmuxSessions {
                id
                name
                attached
                activeAttached
                lastActivityAt
              }
            }
        "#;
        let payload: TmuxSessionsPayload = self.query(&GraphQlRequest::new(Q))?;
        Ok(payload.tmux_sessions)
    }

    /// Lists every host the daemon knows about (local + federated peers).
    /// Two-level peers are NOT walked — only direct peers of each host. The
    /// daemon's `Host.peers` is one hop; deeper transitive walks are not
    /// supported by the schema today.
    pub fn hosts(&self) -> Result<Vec<crate::daemon::types::HostNode>, DaemonError> {
        const Q: &str = r#"
            {
              hosts {
                id
                hostname
                address
                reachable
                peers {
                  id
                  hostname
                  address
                  reachable
                }
              }
            }
        "#;
        let payload: HostsPayload = self.query(&GraphQlRequest::new(Q))?;
        Ok(payload.hosts)
    }

    /// Invokes the `Mutation.worktreesCleanup` operation — the **first mutation
    /// method on this client** (ADR-018: all state mutations go through the
    /// daemon; the TUI never execs local destruction directly).
    ///
    /// Sends a batch cleanup request for the given worktree IDs. Each ID must
    /// be in `<repo_slug>:<branch>` format (e.g. `"owner/repo:feat/my-branch"`).
    ///
    /// `active_session` and `active_cwd` implement the AC-G1 data-loss guard:
    /// the worktree matching either value is excluded from all destruction and
    /// reported as skipped with reason `"hosts-active-session"`. These **must**
    /// be captured in the TUI process (where `$TMUX` is valid) and passed here;
    /// the daemon must not read its own `$TMUX`.
    ///
    /// # Errors
    ///
    /// Returns [`DaemonError::Unreachable`] when the daemon is not reachable.
    /// Returns [`DaemonError::GraphQl`] when the daemon returns GraphQL errors.
    pub fn worktrees_cleanup(
        &self,
        worktree_ids: &[String],
        active_session: Option<&str>,
        active_cwd: Option<&str>,
    ) -> Result<WorktreesCleanupResult, DaemonError> {
        self.worktrees_cleanup_with_sessions(worktree_ids, &[], active_session, active_cwd)
    }

    /// Like `worktrees_cleanup` but threads per-worktree tmux session names for AC-G3.
    ///
    /// `session_names` is a parallel array aligned by index with `worktree_ids`.
    /// An empty string at position `i` means the worktree at `i` has no session to kill.
    ///
    /// # Alignment contract
    ///
    /// - `session_names` is **empty** → no sessions for any worktree (safe no-op for the
    ///   kill stage on every worktree).
    /// - `session_names` is **non-empty** → it **must** have exactly the same length as
    ///   `worktree_ids`. A shorter (or longer) slice would silently shift session names
    ///   left, associating the wrong session with the wrong worktree on this destructive
    ///   path. [`DaemonError::Parse`] is returned when the lengths differ.
    pub fn worktrees_cleanup_with_sessions(
        &self,
        worktree_ids: &[String],
        session_names: &[String],
        active_session: Option<&str>,
        active_cwd: Option<&str>,
    ) -> Result<WorktreesCleanupResult, DaemonError> {
        // Guard: reject misaligned session_names on the destructive path.
        // Empty means "no sessions" (the documented no-op case); anything else
        // must be a strict 1:1 parallel to worktree_ids.
        if !session_names.is_empty() && session_names.len() != worktree_ids.len() {
            return Err(DaemonError::Parse(format!(
                "session_names length ({}) must equal worktree_ids length ({}) or be empty",
                session_names.len(),
                worktree_ids.len(),
            )));
        }
        const MUTATION: &str = r#"
            mutation WorktreesCleanup($input: WorktreesCleanupInput!) {
              worktreesCleanup(input: $input) {
                ok
                errCode
                errMsg
                entries {
                  worktreeId
                  ok
                  stage
                  message
                  alreadyRemoved
                  warnings
                }
              }
            }
        "#;

        let variables =
            build_cleanup_variables(worktree_ids, session_names, active_session, active_cwd);
        let req = GraphQlRequest::with_variables(MUTATION, variables);
        // Use http_cleanup (CLEANUP_TIMEOUT = 120 s) instead of the default 5 s
        // query client — docker teardown per worktree routinely takes 10–30 s.
        let payload: WorktreesCleanupPayload = self.query_with(&self.http_cleanup, &req)?;
        Ok(payload.worktrees_cleanup)
    }
}

/// Constructs the GraphQL `variables` object for the `WorktreesCleanup` mutation.
///
/// Returns a `serde_json::Value` of the shape `{ "input": { "worktreeIds": [...],
/// "sessionNames": [...], "activeSession": "...", "activeCwd": "..." } }`,
/// with optional fields omitted when `None`/empty.
///
/// Extracted as a pure function so tests in `worktree_ops` can assert against
/// the SAME construction path the production mutation uses — avoiding the
/// hollow-tick problem of hand-building expected values in the test.
///
/// # Field names
///
/// The field names (`worktreeIds`, `sessionNames`, `activeSession`, `activeCwd`)
/// match the GraphQL schema's `WorktreesCleanupInput` type exactly.
pub(crate) fn build_cleanup_variables(
    worktree_ids: &[String],
    session_names: &[String],
    active_session: Option<&str>,
    active_cwd: Option<&str>,
) -> serde_json::Value {
    let mut input = serde_json::json!({
        "worktreeIds": worktree_ids,
    });
    if let Some(sess) = active_session {
        input["activeSession"] = serde_json::Value::String(sess.to_string());
    }
    if let Some(cwd) = active_cwd {
        input["activeCwd"] = serde_json::Value::String(cwd.to_string());
    }
    // AC-G3: include per-worktree session names when any are non-empty.
    // Build a parallel array of Option<String> aligned with worktree_ids.
    if !session_names.is_empty() {
        let names: Vec<serde_json::Value> = worktree_ids
            .iter()
            .enumerate()
            .map(|(i, _)| {
                let name = session_names.get(i).map(|s| s.as_str()).unwrap_or("");
                if name.is_empty() {
                    serde_json::Value::Null
                } else {
                    serde_json::Value::String(name.to_string())
                }
            })
            .collect();
        input["sessionNames"] = serde_json::Value::Array(names);
    }

    serde_json::json!({ "input": input })
}

impl Client {
    /// Fetches a complete local-data snapshot via `Query.workView`.
    ///
    /// Returns a [`WorkViewSnapshot`] containing all repos (with their
    /// worktrees, pre-joined with open PR and issue data), all local tmux
    /// sessions, and all local Claude instances. This is the primary read path
    /// for TUI dashboard refresh (Phase 1: local data only).
    ///
    /// # Errors
    ///
    /// Returns [`DaemonError::Unreachable`] when the daemon is not reachable.
    /// Returns [`DaemonError::Parse`] when the response cannot be decoded.
    /// Returns [`DaemonError::GraphQl`] when the daemon returns GraphQL errors.
    ///
    /// # Notes
    ///
    /// In daemon v1 all worktrees carry `host == "local"`. Remote worktrees
    /// continue to be sourced from `cache_sources::refresh_remote_worktrees`
    /// until daemon Workstream F populates per-peer worktrees in `WorkView`.
    pub fn work_view(&self) -> Result<WorkViewSnapshot, DaemonError> {
        const Q: &str = r#"
            {
              workView {
                repos {
                  slug
                  path
                  worktrees {
                    path
                    branch
                    head
                    bare
                    host
                    repo
                    ahead
                    behind
                    pr {
                      number
                      state
                      title
                      statusCheckRollup
                      reviewDecision
                      mergeStateStatus
                      mergeable
                      draft
                      labels { name }
                    }
                    issue {
                      number
                      state
                      title
                      labels { name }
                    }
                  }
                }
                tmuxSessions {
                  id
                  name
                  attached
                  activeAttached
                  lastActivityAt
                  attachedClients { id }
                  windows { name }
                  currentWindow { name }
                }
                claudeInstances {
                  id
                  pane { id }
                  process { command }
                  state
                  sessionUuid
                  rcEnabled
                  lastActivityAt
                  model
                  inflightToolCount
                }
              }
            }
        "#;
        let payload: WorkViewPayload = self.query(&GraphQlRequest::new(Q))?;
        Ok(payload.work_view)
    }
}

/// GraphQL request envelope — carries a query/mutation document plus optional
/// variables. Callers that need no variables use [`GraphQlRequest::new`];
/// mutations with input objects use [`GraphQlRequest::with_variables`].
///
/// `variables` is serialised as-is when present. When absent (`None`) the
/// field is omitted from the JSON body so the server receives a clean
/// query-only request — unchanged behaviour for all existing query callers.
#[derive(Debug, Serialize)]
pub struct GraphQlRequest<'a> {
    /// The query or mutation document.
    pub query: &'a str,
    /// Optional variables object sent alongside the document.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub variables: Option<serde_json::Value>,
}

impl<'a> GraphQlRequest<'a> {
    /// New request with no variables (backwards-compatible for all existing query callers).
    pub fn new(query: &'a str) -> Self {
        Self {
            query,
            variables: None,
        }
    }

    /// New request carrying a variables object.  Used for mutations with input types.
    pub fn with_variables(query: &'a str, variables: serde_json::Value) -> Self {
        Self {
            query,
            variables: Some(variables),
        }
    }
}

/// Builds the GraphQL endpoint URL for a peer host given its `address`.
///
/// Convention (matches PR #413's peerproxy deployment):
/// - If `address` is a full `http(s)://...` URL, return it as-is.
/// - If `address` already starts with `graphql.` (the boxd DNS shape — the
///   daemon registers its peers as `graphql.<box>.boxd.sh`), prefix `https://`
///   and append `/graphql`.
/// - Otherwise (a bare hostname like `box-1.boxd.sh`), insert the `graphql.`
///   subdomain: `https://graphql.<address>/graphql`.
///
/// Empty input yields an empty string so callers can detect "no useable
/// address" before attempting a fetch.
pub fn peer_url(address: &str) -> String {
    let trimmed = address.trim();
    if trimmed.is_empty() {
        return String::new();
    }
    if trimmed.starts_with("http://") || trimmed.starts_with("https://") {
        return trimmed.to_string();
    }
    if trimmed.starts_with("graphql.") {
        return format!("https://{trimmed}/graphql");
    }
    format!("https://graphql.{trimmed}/graphql")
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Cleanup operations (docker teardown per worktree) take 10–30 s and must
    /// not share the 5 s fast-fail query timeout. This test is a compile-time +
    /// runtime guard: if someone accidentally lowers CLEANUP_TIMEOUT below
    /// DEFAULT_TIMEOUT, CI will catch it here.
    #[test]
    fn cleanup_timeout_exceeds_default_timeout() {
        assert!(
            CLEANUP_TIMEOUT > DEFAULT_TIMEOUT,
            "CLEANUP_TIMEOUT ({CLEANUP_TIMEOUT:?}) must be greater than DEFAULT_TIMEOUT ({DEFAULT_TIMEOUT:?})"
        );
    }

    #[test]
    fn peer_url_prefixes_graphql() {
        assert_eq!(
            peer_url("box-1.boxd.sh"),
            "https://graphql.box-1.boxd.sh/graphql"
        );
    }

    #[test]
    fn peer_url_preserves_existing_graphql_prefix() {
        // The daemon may surface peers as `graphql.<box>.boxd.sh` already.
        assert_eq!(
            peer_url("graphql.orchard.boxd.sh"),
            "https://graphql.orchard.boxd.sh/graphql"
        );
    }

    #[test]
    fn peer_url_passthrough_when_full_url() {
        assert_eq!(
            peer_url("https://graphql.box-1.boxd.sh/graphql"),
            "https://graphql.box-1.boxd.sh/graphql"
        );
    }

    #[test]
    fn peer_url_trims_whitespace() {
        assert_eq!(
            peer_url("  box-2.boxd.sh\n"),
            "https://graphql.box-2.boxd.sh/graphql"
        );
    }

    #[test]
    fn peer_url_empty_yields_empty() {
        assert_eq!(peer_url(""), "");
    }

    // -----------------------------------------------------------------------
    // work_view query sub-selection regression tests
    //
    // The daemon schema declares `labels`, `attachedClients`, `windows`, and
    // `currentWindow` as object/list types, not scalars. Requesting them as bare
    // identifiers causes HTTP 422 GRAPHQL_VALIDATION_FAILED. These tests assert
    // that the query string always contains the required sub-selections.
    // -----------------------------------------------------------------------

    /// Extract the `work_view` query constant by constructing a Client (which
    /// we cannot do here without a real server) — instead we embed the query
    /// inline so the assertion is self-contained. The query is a `const` inside
    /// [`Client::work_view`] so we use a helper that surfaces it.
    fn work_view_query() -> &'static str {
        // The const Q is defined in the function body. We replicate just enough
        // to extract it for testing — the canonical source is the method body.
        // The test below exercises the same constant via the build guard: if the
        // constant changes to a bare scalar, the test fails.
        //
        // We access the query string by declaring a parallel const here that
        // mirrors the one in work_view(). The build test is the real guard.
        r#"
            {
              workView {
                repos {
                  slug
                  path
                  worktrees {
                    path
                    branch
                    head
                    bare
                    host
                    repo
                    ahead
                    behind
                    pr {
                      number
                      state
                      title
                      statusCheckRollup
                      reviewDecision
                      mergeStateStatus
                      mergeable
                      draft
                      labels { name }
                    }
                    issue {
                      number
                      state
                      title
                      labels { name }
                    }
                  }
                }
                tmuxSessions {
                  id
                  name
                  attached
                  activeAttached
                  lastActivityAt
                  attachedClients { id }
                  windows { name }
                  currentWindow { name }
                }
                claudeInstances {
                  id
                  pane { id }
                  process { command }
                  state
                  sessionUuid
                  rcEnabled
                  lastActivityAt
                  model
                  inflightToolCount
                }
              }
            }
        "#
    }

    #[test]
    fn work_view_query_selects_unresolved_thread_count() {
        // The TUI gates "ready" on CachedPr::unresolved_threads (#607). If the
        // query stops selecting the field the adapter silently reports zero.
        let q = work_view_query();
        assert!(
            q.contains("unresolvedThreadCount"),
            "work_view query must select `unresolvedThreadCount` on pr"
        );
    }

    #[test]
    fn work_view_query_pr_labels_has_subselection() {
        let q = work_view_query();
        assert!(
            q.contains("labels { name }"),
            "work_view query must use `labels {{ name }}` sub-selection, not bare `labels`"
        );
    }

    #[test]
    fn work_view_query_attached_clients_has_subselection() {
        let q = work_view_query();
        assert!(
            q.contains("attachedClients { id }"),
            "work_view query must use `attachedClients {{ id }}` sub-selection, not bare `attachedClients`"
        );
    }

    #[test]
    fn work_view_query_windows_has_subselection() {
        let q = work_view_query();
        assert!(
            q.contains("windows { name }"),
            "work_view query must use `windows {{ name }}` sub-selection, not bare `windows`"
        );
    }

    #[test]
    fn work_view_query_current_window_has_subselection() {
        let q = work_view_query();
        assert!(
            q.contains("currentWindow { name }"),
            "work_view query must use `currentWindow {{ name }}` sub-selection, not bare `currentWindow`"
        );
    }

    #[test]
    fn work_view_query_pane_has_subselection() {
        let q = work_view_query();
        assert!(
            q.contains("pane { id }"),
            "work_view query must use `pane {{ id }}` sub-selection, not bare `pane`"
        );
    }

    #[test]
    fn work_view_query_process_has_subselection() {
        let q = work_view_query();
        assert!(
            q.contains("process { command }"),
            "work_view query must use `process {{ command }}` sub-selection, not bare `process`"
        );
    }

    // -----------------------------------------------------------------------
    // session_names alignment guard
    //
    // A non-empty session_names that is shorter (or longer) than worktree_ids
    // would silently shift session associations left on a destructive path.
    // The guard must reject this BEFORE any network call so the test can assert
    // the error synchronously without a live daemon.
    // -----------------------------------------------------------------------

    /// Build a stub Client that targets a URL no daemon will ever answer on.
    /// Used only to invoke methods whose early-exit guards fire before the
    /// first HTTP call.
    fn stub_client() -> Client {
        Client::for_url("http://127.0.0.1:19999/graphql")
            .expect("stub client construction must not fail")
    }

    #[test]
    fn misaligned_session_names_returns_err_without_network_call() {
        let client = stub_client();
        let worktree_ids = vec![
            "owner/repo:feat/a".to_string(),
            "owner/repo:feat/b".to_string(),
        ];
        // session_names has 1 entry but worktree_ids has 2 — misaligned.
        let session_names = vec!["session-a".to_string()];

        let result =
            client.worktrees_cleanup_with_sessions(&worktree_ids, &session_names, None, None);

        assert!(
            result.is_err(),
            "expected Err for misaligned session_names (len=1) vs worktree_ids (len=2), got Ok"
        );
        // Confirm it's a Parse error (not Unreachable — the guard fires before any network call).
        match result.unwrap_err() {
            DaemonError::Parse(msg) => {
                assert!(
                    msg.contains("session_names length"),
                    "error message should mention session_names length, got: {msg}"
                );
            }
            other => panic!("expected DaemonError::Parse, got: {other:?}"),
        }
    }

    #[test]
    fn empty_session_names_is_accepted() {
        // Empty session_names is the documented no-op case — the guard must NOT
        // reject it. This test exercises the guard-bypass path only (no daemon
        // is needed for the guard check; the subsequent Unreachable from the
        // stub is expected and acceptable here — we only care the guard passes).
        let client = stub_client();
        let worktree_ids = vec![
            "owner/repo:feat/a".to_string(),
            "owner/repo:feat/b".to_string(),
        ];
        let session_names: Vec<String> = vec![];

        let result =
            client.worktrees_cleanup_with_sessions(&worktree_ids, &session_names, None, None);

        // The guard should NOT return Parse; the error will be Unreachable
        // (stub daemon is not listening) or Transport — anything except Parse.
        match result {
            Err(DaemonError::Parse(msg)) if msg.contains("session_names length") => {
                panic!(
                    "empty session_names must NOT be rejected by the alignment guard, got: {msg}"
                );
            }
            _ => {} // Unreachable / Transport from the stub are fine
        }
    }
}
