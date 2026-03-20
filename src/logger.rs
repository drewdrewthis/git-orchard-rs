use std::collections::HashMap;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::PathBuf;
use std::sync::{LazyLock, Mutex};
use std::time::{Duration, Instant};

const MAX_SIZE_BYTES: u64 = 10 * 1024 * 1024; // 10 MB

/// A file-based logger that rotates at 10 MB.
pub struct Logger {
    path: PathBuf,
    timers: Mutex<HashMap<String, Instant>>,
}

impl Logger {
    /// Creates a new `Logger` writing to `dir/filename`.
    /// The directory is created if it does not exist.
    pub fn new(dir: &str, filename: &str) -> Self {
        let path = PathBuf::from(dir).join(filename);
        // Best-effort; write calls will silently fail if this fails.
        let _ = fs::create_dir_all(dir);
        Logger {
            path,
            timers: Mutex::new(HashMap::new()),
        }
    }

    /// Logs a message at INFO level.
    pub fn info(&self, msg: &str) {
        self.write("INFO", msg);
    }

    /// Logs a message at WARN level.
    pub fn warn(&self, msg: &str) {
        self.write("WARN", msg);
    }

    /// Records the start time for `label`.
    pub fn time(&self, label: &str) {
        if let Ok(mut timers) = self.timers.lock() {
            timers.insert(label.to_string(), Instant::now());
        }
    }

    /// Logs elapsed time since the matching `time()` call.
    /// Reports 0 if `time()` was never called for this label.
    pub fn time_end(&self, label: &str) {
        let elapsed = {
            match self.timers.lock() {
                Ok(mut timers) => {
                    timers.remove(label).map(|start| start.elapsed()).unwrap_or(Duration::ZERO)
                }
                Err(_) => Duration::ZERO,
            }
        };
        self.write("TIME", &format!("{label}: {:?}", elapsed));
    }

    fn write(&self, level: &str, msg: &str) {
        let now = chrono::Utc::now().format("%Y-%m-%dT%H:%M:%SZ");
        let line = format!("[{now}] [{level}] {msg}\n");

        // Open in append mode each time; cheap on most OS due to O_APPEND.
        let mut file = match OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)
        {
            Ok(f) => f,
            Err(_) => return,
        };

        let _ = file.write_all(line.as_bytes());

        // Rotate if over threshold.
        if let Ok(meta) = file.metadata() {
            if meta.len() >= MAX_SIZE_BYTES {
                drop(file);
                let rotated = self.path.with_extension("log.1");
                let _ = fs::rename(&self.path, rotated);
            }
        }
    }
}

/// Global default logger writing to `~/.local/state/git-orchard/debug.log`.
pub static LOG: LazyLock<Logger> = LazyLock::new(|| {
    let dir = dirs::home_dir()
        .unwrap_or_else(std::env::temp_dir)
        .join(".local")
        .join("state")
        .join("git-orchard");
    Logger::new(&dir.to_string_lossy(), "debug.log")
});

/// RAII guard that calls `LOG.time_end` when dropped.
/// Used by the `timed!` macro to ensure time_end is called even on early returns.
pub struct TimingGuard<'a> {
    label: &'a str,
}

impl<'a> TimingGuard<'a> {
    pub fn new(label: &'a str) -> Self {
        Self { label }
    }
}

impl<'a> Drop for TimingGuard<'a> {
    fn drop(&mut self) {
        LOG.time_end(self.label);
    }
}

/// Times a block and logs it. Returns the block's value.
/// Calls `LOG.time` before and `LOG.time_end` after (even on early return).
///
/// # Example
/// ```ignore
/// let result = timed!("phase:git", {
///     git::list_worktrees().unwrap_or_default()
/// });
/// ```
#[macro_export]
macro_rules! timed {
    ($label:expr, $body:expr) => {{
        $crate::logger::LOG.time($label);
        let _guard = $crate::logger::TimingGuard::new($label);
        $body
    }};
}
