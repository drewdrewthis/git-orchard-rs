use std::path::PathBuf;
use std::process::Command;

use crate::logger::LOG;
use crate::types::{OrchardConfig, RemoteConfig};

/// Loads `orchard.json` from the `.git` directory of the current repository.
/// Supports the new `{ "remote": {...} }` format and the legacy `{ "remotes": [{...}] }` format.
/// Returns an empty `OrchardConfig` on any error.
pub fn load_config() -> OrchardConfig {
    match git_absolute_dir() {
        Ok(dir) => load_config_from_dir(&dir),
        Err(_) => OrchardConfig::default(),
    }
}

// Reads orchard.json from `dir`.
fn load_config_from_dir(dir: &str) -> OrchardConfig {
    let path = PathBuf::from(dir).join("orchard.json");
    let data = match std::fs::read(&path) {
        Ok(d) => d,
        Err(_) => {
            LOG.info("config: no orchard.json found");
            return OrchardConfig::default();
        }
    };
    parse_config(&data, &path.to_string_lossy())
}

// Unmarshals raw JSON bytes into an OrchardConfig.
fn parse_config(data: &[u8], path: &str) -> OrchardConfig {
    #[derive(serde::Deserialize)]
    struct LegacyEntry {
        host: String,
        #[serde(rename = "repoPath")]
        repo_path: String,
        #[serde(default)]
        shell: String,
    }

    #[derive(serde::Deserialize)]
    struct RawConfig {
        remote: Option<RemoteConfig>,
        #[serde(default)]
        remotes: Vec<LegacyEntry>,
    }

    let raw: RawConfig = match serde_json::from_slice(data) {
        Ok(r) => r,
        Err(error) => {
            LOG.warn(&format!("config: failed to parse orchard.json: {}", error));
            return OrchardConfig::default();
        }
    };

    // New format takes precedence.
    if let Some(remote) = raw.remote {
        LOG.info(&format!("config: loaded remote {} from {}", remote.host, path));
        return OrchardConfig { remote: Some(remote) };
    }

    // Legacy format: use the first entry that has both host and repoPath.
    for entry in raw.remotes {
        if entry.host.is_empty() || entry.repo_path.is_empty() {
            continue;
        }
        let shell = if entry.shell.is_empty() {
            "ssh".to_string()
        } else {
            entry.shell
        };
        LOG.info(&format!("config: migrated remote {} from legacy format", entry.host));
        return OrchardConfig {
            remote: Some(RemoteConfig {
                host: entry.host,
                repo_path: entry.repo_path,
                shell,
            }),
        };
    }

    OrchardConfig::default()
}

// Runs `git rev-parse --absolute-git-dir` and returns the path.
fn git_absolute_dir() -> anyhow::Result<String> {
    let out = Command::new("git")
        .args(["rev-parse", "--absolute-git-dir"])
        .output()?;
    Ok(String::from_utf8_lossy(&out.stdout).trim().to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn write_temp(json: &str) -> NamedTempFile {
        let mut f = NamedTempFile::new().unwrap();
        f.write_all(json.as_bytes()).unwrap();
        f
    }

    fn load_from_file(path: &str) -> OrchardConfig {
        let data = std::fs::read(path).unwrap();
        parse_config(&data, path)
    }

    #[test]
    fn new_format_remote() {
        let f = write_temp(r#"{"remote":{"host":"myhost","repoPath":"/srv/repo","shell":"ssh"}}"#);
        let cfg = load_from_file(f.path().to_str().unwrap());
        let remote = cfg.remote.unwrap();
        assert_eq!(remote.host, "myhost");
        assert_eq!(remote.repo_path, "/srv/repo");
        assert_eq!(remote.shell, "ssh");
    }

    #[test]
    fn legacy_format_remotes_first_valid_entry() {
        let f = write_temp(
            r#"{"remotes":[{"host":"h1","repoPath":"/p1"},{"host":"h2","repoPath":"/p2"}]}"#,
        );
        let cfg = load_from_file(f.path().to_str().unwrap());
        let remote = cfg.remote.unwrap();
        assert_eq!(remote.host, "h1");
        assert_eq!(remote.repo_path, "/p1");
        assert_eq!(remote.shell, "ssh"); // default
    }

    #[test]
    fn legacy_format_skips_incomplete_entries() {
        let f = write_temp(r#"{"remotes":[{"host":"","repoPath":"/p"},{"host":"h2","repoPath":"/p2"}]}"#);
        let cfg = load_from_file(f.path().to_str().unwrap());
        let remote = cfg.remote.unwrap();
        assert_eq!(remote.host, "h2");
    }

    #[test]
    fn new_format_takes_precedence_over_legacy() {
        let f = write_temp(
            r#"{"remote":{"host":"new","repoPath":"/new","shell":"mosh"},"remotes":[{"host":"old","repoPath":"/old"}]}"#,
        );
        let cfg = load_from_file(f.path().to_str().unwrap());
        assert_eq!(cfg.remote.unwrap().host, "new");
    }

    #[test]
    fn empty_json_returns_default() {
        let f = write_temp("{}");
        let cfg = load_from_file(f.path().to_str().unwrap());
        assert!(cfg.remote.is_none());
    }

    #[test]
    fn invalid_json_returns_default() {
        let f = write_temp("not json");
        let cfg = load_from_file(f.path().to_str().unwrap());
        assert!(cfg.remote.is_none());
    }
}
