use std::collections::HashMap;
use std::process::Command;
use std::sync::{Arc, Condvar, Mutex, OnceLock};

use anyhow::anyhow;
use regex::Regex;

use crate::logger::LOG;
use crate::timed;
use crate::types::{ChecksStatus, IssueState, PrInfo, ReviewDecision};

// ---------------------------------------------------------------------------
// Repo cache
// ---------------------------------------------------------------------------

// We store a String-encoded error so the value is Sync.
static REPO_CACHE: OnceLock<Result<(String, String), String>> = OnceLock::new();

/// Returns `(owner, name)` for the current GitHub repository.
/// The result is cached after the first successful call.
pub fn get_repo() -> anyhow::Result<(String, String)> {
    let cached = REPO_CACHE.get_or_init(|| {
        let out = Command::new("gh")
            .args(["repo", "view", "--json", "owner,name"])
            .output()
            .map_err(|e| e.to_string())?;
        let v: serde_json::Value =
            serde_json::from_slice(&out.stdout).map_err(|e| e.to_string())?;
        let owner = v["owner"]["login"]
            .as_str()
            .ok_or_else(|| "missing owner.login".to_string())?
            .to_string();
        let name = v["name"]
            .as_str()
            .ok_or_else(|| "missing name".to_string())?
            .to_string();
        Ok((owner, name))
    });
    match cached {
        Ok(pair) => Ok(pair.clone()),
        Err(e) => Err(anyhow!("{}", e)),
    }
}

// ---------------------------------------------------------------------------
// gh availability
// ---------------------------------------------------------------------------

/// Reports whether the `gh` CLI is authenticated and available.
pub fn is_gh_available() -> bool {
    Command::new("gh")
        .args(["auth", "status"])
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

// ---------------------------------------------------------------------------
// Simple counting semaphore (no external dependencies)
// ---------------------------------------------------------------------------

struct Semaphore {
    inner: Arc<(Mutex<usize>, Condvar)>,
}

impl Semaphore {
    fn new(n: usize) -> Self {
        Self {
            inner: Arc::new((Mutex::new(n), Condvar::new())),
        }
    }

    fn acquire(&self) -> SemaphoreGuard {
        let (lock, cvar) = &*self.inner;
        let mut count = lock.lock().unwrap();
        while *count == 0 {
            count = cvar.wait(count).unwrap();
        }
        *count -= 1;
        SemaphoreGuard {
            inner: Arc::clone(&self.inner),
        }
    }
}

struct SemaphoreGuard {
    inner: Arc<(Mutex<usize>, Condvar)>,
}

impl Drop for SemaphoreGuard {
    fn drop(&mut self) {
        let (lock, cvar) = &*self.inner;
        let mut count = lock.lock().unwrap();
        *count += 1;
        cvar.notify_one();
    }
}

// ---------------------------------------------------------------------------
// PR fetching
// ---------------------------------------------------------------------------

/// Fetches PR info for the given branches.
/// First fetches all open PRs in one request, then resolves missing branches
/// concurrently with a limit of 5 threads.
pub fn get_all_prs(branches: &[String]) -> HashMap<String, PrInfo> {
    if branches.is_empty() {
        return HashMap::new();
    }
    timed!("getAllPrs", {
        get_all_prs_inner(branches)
    })
}

fn get_all_prs_inner(branches: &[String]) -> HashMap<String, PrInfo> {
    let mut result: HashMap<String, PrInfo> = HashMap::new();

    if let Ok(open_prs) = fetch_open_prs() {
        result.extend(open_prs);
    }

    let missing: Vec<String> = branches
        .iter()
        .filter(|b| !result.contains_key(*b))
        .cloned()
        .collect();

    if !missing.is_empty() {
        LOG.info(&format!("getAllPrs: looking up {} missing branches", missing.len()));

        let sem = Arc::new(Semaphore::new(5));
        let collected: Arc<Mutex<HashMap<String, PrInfo>>> = Arc::new(Mutex::new(HashMap::new()));

        let handles: Vec<_> = missing
            .into_iter()
            .map(|branch| {
                let sem = Arc::clone(&sem);
                let collected = Arc::clone(&collected);
                std::thread::spawn(move || {
                    let _guard = sem.acquire();
                    if let Some(pr) = fetch_pr_for_branch(&branch) {
                        collected.lock().unwrap().insert(branch, pr);
                    }
                })
            })
            .collect();

        for h in handles {
            let _ = h.join();
        }

        let fetched = collected.lock().unwrap();
        result.extend(fetched.clone());
    }

    LOG.info(&format!("getAllPrs: {} PRs", result.len()));
    result
}

fn fetch_open_prs() -> anyhow::Result<HashMap<String, PrInfo>> {
    let out = Command::new("gh")
        .args([
            "pr",
            "list",
            "--state",
            "open",
            "--json",
            "headRefName,number,state,title,url,reviewDecision",
            "--limit",
            "300",
        ])
        .output()?;

    let raws: Vec<serde_json::Value> = serde_json::from_slice(&out.stdout)?;
    let mut map = HashMap::with_capacity(raws.len());
    for r in &raws {
        if let Some(pr) = raw_to_pr_info(r) {
            let branch = r["headRefName"]
                .as_str()
                .unwrap_or_default()
                .to_string();
            map.insert(branch, pr);
        }
    }
    Ok(map)
}

fn fetch_pr_for_branch(branch: &str) -> Option<PrInfo> {
    let out = Command::new("gh")
        .args([
            "pr",
            "list",
            "--head",
            branch,
            "--state",
            "all",
            "--json",
            "headRefName,number,state,title,url,reviewDecision",
            "--limit",
            "1",
        ])
        .output()
        .ok()?;

    let raws: Vec<serde_json::Value> = serde_json::from_slice(&out.stdout).ok()?;
    raws.into_iter().next().and_then(|r| raw_to_pr_info(&r))
}

fn raw_to_pr_info(r: &serde_json::Value) -> Option<PrInfo> {
    let number = r["number"].as_u64()? as u32;
    let state = r["state"].as_str().unwrap_or("open").to_lowercase();
    let title = r["title"].as_str().unwrap_or("").to_string();
    let url = r["url"].as_str().unwrap_or("").to_string();
    let review_decision_str = r["reviewDecision"].as_str().unwrap_or("");
    let review_decision = parse_review_decision(review_decision_str);

    Some(PrInfo {
        number,
        state,
        title,
        url,
        review_decision,
        unresolved_threads: 0,
        checks_status: ChecksStatus::None,
        has_conflicts: false,
    })
}

fn parse_review_decision(s: &str) -> ReviewDecision {
    match s {
        "APPROVED" => ReviewDecision::Approved,
        "CHANGES_REQUESTED" => ReviewDecision::ChangesRequested,
        "REVIEW_REQUIRED" => ReviewDecision::ReviewRequired,
        _ => ReviewDecision::None,
    }
}

// ---------------------------------------------------------------------------
// PR enrichment
// ---------------------------------------------------------------------------

/// Fetches detailed GraphQL data for up to 25 open PRs and updates `pr_map` in-place.
pub fn enrich_pr_details(pr_map: &mut HashMap<String, PrInfo>) {
    timed!("enrichPrDetails", {
        enrich_pr_details_inner(pr_map);
    })
}

fn enrich_pr_details_inner(pr_map: &mut HashMap<String, PrInfo>) {
    let (owner, repo) = match get_repo() {
        Ok(pair) => pair,
        Err(err) => {
            LOG.warn(&format!("enrichPrDetails failed: {}", err));
            return;
        }
    };

    let entries: Vec<(String, u32)> = pr_map
        .iter()
        .filter(|(_, pr)| pr.state == "open")
        .take(25)
        .map(|(branch, pr)| (branch.clone(), pr.number))
        .collect();

    if entries.is_empty() {
        return;
    }

    let query = build_enrich_query(&entries);

    let out = match Command::new("gh")
        .args([
            "api",
            "graphql",
            "-f",
            &format!("query={}", query),
            "-f",
            &format!("owner={}", owner),
            "-f",
            &format!("name={}", repo),
        ])
        .output()
    {
        Ok(o) => o,
        Err(err) => {
            LOG.warn(&format!("enrichPrDetails failed: {}", err));
            return;
        }
    };

    let raw: serde_json::Value = match serde_json::from_slice(&out.stdout) {
        Ok(v) => v,
        Err(err) => {
            LOG.warn(&format!("enrichPrDetails failed: {}", err));
            return;
        }
    };

    let repo_obj = match raw["data"]["repository"].as_object() {
        Some(o) => o.clone(),
        None => return,
    };

    // Reverse map: number -> branch
    let number_to_branch: HashMap<u32, String> = entries
        .iter()
        .map(|(branch, num)| (*num, branch.clone()))
        .collect();

    for node_val in repo_obj.values() {
        let number = match node_val["number"].as_u64() {
            Some(n) => n as u32,
            None => continue,
        };
        let branch = match number_to_branch.get(&number) {
            Some(b) => b.clone(),
            None => continue,
        };
        let pr = match pr_map.get_mut(&branch) {
            Some(p) => p,
            None => continue,
        };

        pr.has_conflicts = node_val["mergeable"].as_str() == Some("CONFLICTING");

        let decision_str = node_val["reviewDecision"].as_str().unwrap_or("");
        let review_nodes = node_val["latestReviews"]["nodes"]
            .as_array()
            .cloned()
            .unwrap_or_default();
        pr.review_decision = derive_review_decision(decision_str, &review_nodes);

        let thread_nodes = node_val["reviewThreads"]["nodes"]
            .as_array()
            .cloned()
            .unwrap_or_default();
        pr.unresolved_threads = thread_nodes
            .iter()
            .filter(|t| t["isResolved"].as_bool() == Some(false))
            .count() as u32;

        let check_contexts: Vec<serde_json::Value> = node_val["commits"]["nodes"]
            .as_array()
            .and_then(|nodes| nodes.first())
            .and_then(|n| {
                n["commit"]["statusCheckRollup"]["contexts"]["nodes"].as_array()
            })
            .cloned()
            .unwrap_or_default();
        pr.checks_status = derive_checks_status(&check_contexts);
    }
}

fn build_enrich_query(entries: &[(String, u32)]) -> String {
    let mut q = String::from(
        "query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n",
    );
    for (i, (_, number)) in entries.iter().enumerate() {
        q.push_str(&format!(
            "    pr{i}: pullRequest(number: {number}) {{\n\
                   number\n\
                   mergeable\n\
                   reviewDecision\n\
                   latestReviews(last: 10) {{ nodes {{ state }} }}\n\
                   reviewThreads(last: 100) {{ nodes {{ isResolved }} }}\n\
                   commits(last: 1) {{ nodes {{ commit {{ statusCheckRollup {{ contexts(last: 100) {{ nodes {{ __typename ... on CheckRun {{ conclusion status }} ... on StatusContext {{ state }} }} }} }} }} }} }}\n\
                 }}\n"
        ));
    }
    q.push_str("  }\n}");
    q
}

// ---------------------------------------------------------------------------
// Review decision derivation
// ---------------------------------------------------------------------------

/// Derives the effective review decision. If `decision` is non-empty it is used
/// directly; otherwise it is derived from individual reviews (CHANGES_REQUESTED
/// takes priority over APPROVED).
pub fn derive_review_decision(
    decision: &str,
    reviews: &[serde_json::Value],
) -> ReviewDecision {
    if !decision.is_empty() {
        return parse_review_decision(decision);
    }
    let mut has_approved = false;
    for r in reviews {
        match r["state"].as_str() {
            Some("CHANGES_REQUESTED") => return ReviewDecision::ChangesRequested,
            Some("APPROVED") => has_approved = true,
            _ => {}
        }
    }
    if has_approved {
        ReviewDecision::Approved
    } else {
        ReviewDecision::None
    }
}

// ---------------------------------------------------------------------------
// Checks status derivation
// ---------------------------------------------------------------------------

/// Aggregates a slice of check context nodes into a single `ChecksStatus`.
pub fn derive_checks_status(contexts: &[serde_json::Value]) -> ChecksStatus {
    if contexts.is_empty() {
        return ChecksStatus::None;
    }

    let mut has_pending = false;
    for c in contexts {
        match c["__typename"].as_str() {
            Some("CheckRun") => {
                if c["status"].as_str() != Some("COMPLETED") {
                    has_pending = true;
                    continue;
                }
                match c["conclusion"].as_str() {
                    Some("FAILURE") | Some("TIMED_OUT") | Some("CANCELLED") => {
                        return ChecksStatus::Fail;
                    }
                    _ => {}
                }
            }
            Some("StatusContext") => match c["state"].as_str() {
                Some("FAILURE") | Some("ERROR") => return ChecksStatus::Fail,
                Some("PENDING") => has_pending = true,
                _ => {}
            },
            _ => {}
        }
    }

    if has_pending {
        ChecksStatus::Pending
    } else {
        ChecksStatus::Pass
    }
}

// ---------------------------------------------------------------------------
// Issue number extraction
// ---------------------------------------------------------------------------

fn issue_keyword_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"(?i)issue[/\-]?(\d+)").unwrap())
}

fn leading_number_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"^(\d+)-").unwrap())
}

fn embedded_number_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"-(\d+)(?:-|$)").unwrap())
}

fn strip_prefix_re() -> &'static Regex {
    static RE: OnceLock<Regex> = OnceLock::new();
    RE.get_or_init(|| Regex::new(r"^[a-zA-Z][a-zA-Z0-9]*[/_]").unwrap())
}

/// Attempts to extract a GitHub issue number from a branch name.
/// Strips common prefixes (e.g. `feat/`, `fix/`) before matching.
pub fn extract_issue_number(branch: &str) -> Option<u32> {
    let stripped = strip_prefix_re().replace(branch, "").into_owned();

    // Keyword pattern on original and stripped.
    for candidate in &[branch, stripped.as_str()] {
        if let Some(caps) = issue_keyword_re().captures(candidate) {
            if let Ok(n) = caps[1].parse::<u32>() {
                if n >= 1 {
                    return Some(n);
                }
            }
        }
    }

    // Leading number (>= 100) on stripped.
    if let Some(caps) = leading_number_re().captures(&stripped) {
        if let Ok(n) = caps[1].parse::<u32>() {
            if n >= 100 {
                return Some(n);
            }
        }
    }

    // Embedded number (>= 100) on stripped.
    if let Some(caps) = embedded_number_re().captures(&stripped) {
        if let Ok(n) = caps[1].parse::<u32>() {
            if n >= 100 {
                return Some(n);
            }
        }
    }

    None
}

// ---------------------------------------------------------------------------
// Issue states
// ---------------------------------------------------------------------------

/// Fetches the open/closed/completed state for up to 25 issues via a batched
/// GraphQL query.
pub fn get_issue_states(numbers: &[u32]) -> HashMap<u32, IssueState> {
    if numbers.is_empty() {
        return HashMap::new();
    }
    timed!("getIssueStates", {
        get_issue_states_inner(numbers)
    })
}

fn get_issue_states_inner(numbers: &[u32]) -> HashMap<u32, IssueState> {
    let (owner, repo) = match get_repo() {
        Ok(pair) => pair,
        Err(_) => return HashMap::new(),
    };

    let limit = if numbers.len() > 25 {
        &numbers[..25]
    } else {
        numbers
    };

    let mut q = String::from(
        "query($owner: String!, $name: String!) {\n  repository(owner: $owner, name: $name) {\n",
    );
    for (i, num) in limit.iter().enumerate() {
        q.push_str(&format!(
            "    issue{i}: issue(number: {num}) {{ number state stateReason }}\n"
        ));
    }
    q.push_str("  }\n}");

    let out = match Command::new("gh")
        .args([
            "api",
            "graphql",
            "-f",
            &format!("query={}", q),
            "-f",
            &format!("owner={}", owner),
            "-f",
            &format!("name={}", repo),
        ])
        .output()
    {
        Ok(o) => o,
        Err(_) => return HashMap::new(),
    };

    let raw: serde_json::Value = match serde_json::from_slice(&out.stdout) {
        Ok(v) => v,
        Err(_) => return HashMap::new(),
    };

    let mut result = HashMap::new();
    if let Some(repo_obj) = raw["data"]["repository"].as_object() {
        for node_val in repo_obj.values() {
            let number = match node_val["number"].as_u64() {
                Some(n) => n as u32,
                None => continue,
            };
            let state = node_val["state"].as_str().unwrap_or("");
            let state_reason = node_val["stateReason"].as_str().unwrap_or("");
            let issue_state = if state == "OPEN" {
                IssueState::Open
            } else if state_reason == "COMPLETED" {
                IssueState::Completed
            } else {
                IssueState::Closed
            };
            result.insert(number, issue_state);
        }
    }
    LOG.info(&format!("getIssueStates: {} issues resolved", result.len()));
    result
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    // --- derive_review_decision ---

    #[test]
    fn review_decision_uses_explicit_value_when_non_empty() {
        let result = derive_review_decision("APPROVED", &[]);
        assert_eq!(result, ReviewDecision::Approved);
    }

    #[test]
    fn review_decision_changes_requested_when_explicit() {
        let result = derive_review_decision("CHANGES_REQUESTED", &[]);
        assert_eq!(result, ReviewDecision::ChangesRequested);
    }

    #[test]
    fn review_decision_derives_changes_requested_from_reviews() {
        let reviews = vec![
            json!({"state": "APPROVED"}),
            json!({"state": "CHANGES_REQUESTED"}),
        ];
        let result = derive_review_decision("", &reviews);
        assert_eq!(result, ReviewDecision::ChangesRequested);
    }

    #[test]
    fn review_decision_derives_approved_from_reviews() {
        let reviews = vec![json!({"state": "APPROVED"})];
        let result = derive_review_decision("", &reviews);
        assert_eq!(result, ReviewDecision::Approved);
    }

    #[test]
    fn review_decision_returns_none_when_no_reviews() {
        let result = derive_review_decision("", &[]);
        assert_eq!(result, ReviewDecision::None);
    }

    // --- derive_checks_status ---

    #[test]
    fn checks_status_returns_none_for_empty_contexts() {
        assert_eq!(derive_checks_status(&[]), ChecksStatus::None);
    }

    #[test]
    fn checks_status_fails_on_check_run_failure() {
        let contexts = vec![json!({
            "__typename": "CheckRun",
            "status": "COMPLETED",
            "conclusion": "FAILURE"
        })];
        assert_eq!(derive_checks_status(&contexts), ChecksStatus::Fail);
    }

    #[test]
    fn checks_status_fails_on_status_context_error() {
        let contexts = vec![json!({
            "__typename": "StatusContext",
            "state": "ERROR"
        })];
        assert_eq!(derive_checks_status(&contexts), ChecksStatus::Fail);
    }

    #[test]
    fn checks_status_pending_when_check_run_not_completed() {
        let contexts = vec![json!({
            "__typename": "CheckRun",
            "status": "IN_PROGRESS",
            "conclusion": null
        })];
        assert_eq!(derive_checks_status(&contexts), ChecksStatus::Pending);
    }

    #[test]
    fn checks_status_pass_when_all_completed_success() {
        let contexts = vec![json!({
            "__typename": "CheckRun",
            "status": "COMPLETED",
            "conclusion": "SUCCESS"
        })];
        assert_eq!(derive_checks_status(&contexts), ChecksStatus::Pass);
    }

    // --- extract_issue_number ---

    #[test]
    fn extracts_issue_keyword_with_slash() {
        assert_eq!(extract_issue_number("issue/42"), Some(42));
    }

    #[test]
    fn extracts_issue_keyword_case_insensitive() {
        assert_eq!(extract_issue_number("feat/Issue-123-some-thing"), Some(123));
    }

    #[test]
    fn extracts_leading_number_above_100() {
        assert_eq!(extract_issue_number("feat/200-my-feature"), Some(200));
    }

    #[test]
    fn extracts_embedded_number_above_100() {
        assert_eq!(extract_issue_number("fix/something-150-desc"), Some(150));
    }

    #[test]
    fn returns_none_for_small_leading_number() {
        assert_eq!(extract_issue_number("feat/42-small"), None);
    }

    #[test]
    fn returns_none_for_plain_branch() {
        assert_eq!(extract_issue_number("main"), None);
    }
}
