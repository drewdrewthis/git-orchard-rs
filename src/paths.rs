/// Replaces the user's home directory prefix in `path` with `~`.
/// Returns the path unchanged if it does not start with `$HOME`.
pub fn tildify(path: &str) -> String {
    let home = match dirs::home_dir() {
        Some(h) => h,
        None => return path.to_string(),
    };

    let home_str = home.to_string_lossy();
    if home_str.is_empty() {
        return path.to_string();
    }

    if path == home_str.as_ref() {
        return "~".to_string();
    }

    let prefix = format!("{}/", home_str);
    if path.starts_with(prefix.as_str()) {
        return format!("~/{}", &path[prefix.len()..]);
    }

    path.to_string()
}

/// Truncates `path` to at most `max_width` characters (Unicode codepoints).
/// If the path is longer, prepends `…` and takes the rightmost `max_width - 1` codepoints.
pub fn truncate_left(path: &str, max_width: usize) -> String {
    let runes: Vec<char> = path.chars().collect();
    if runes.len() <= max_width {
        return path.to_string();
    }
    if max_width <= 1 {
        return "…".to_string();
    }
    let tail: String = runes[runes.len() - (max_width - 1)..].iter().collect();
    format!("…{tail}")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tildify_replaces_home_prefix() {
        let home = dirs::home_dir().unwrap();
        let path = format!("{}/projects/myrepo", home.display());
        let result = tildify(&path);
        assert_eq!(result, "~/projects/myrepo");
    }

    #[test]
    fn tildify_exact_home_dir() {
        let home = dirs::home_dir().unwrap();
        let result = tildify(&home.to_string_lossy());
        assert_eq!(result, "~");
    }

    #[test]
    fn tildify_unrelated_path_unchanged() {
        let result = tildify("/tmp/some/path");
        assert_eq!(result, "/tmp/some/path");
    }

    #[test]
    fn truncate_left_short_path_unchanged() {
        assert_eq!(truncate_left("/short", 20), "/short");
    }

    #[test]
    fn truncate_left_exact_width_unchanged() {
        assert_eq!(truncate_left("abcde", 5), "abcde");
    }

    #[test]
    fn truncate_left_over_max_width() {
        let result = truncate_left("/a/very/long/path/to/somewhere", 10);
        let chars: Vec<char> = result.chars().collect();
        assert_eq!(chars.len(), 10);
        assert_eq!(chars[0], '…');
    }

    #[test]
    fn truncate_left_max_width_one() {
        assert_eq!(truncate_left("hello", 1), "…");
    }

    #[test]
    fn truncate_left_max_width_zero() {
        assert_eq!(truncate_left("hello", 0), "…");
    }

    #[test]
    fn truncate_left_unicode() {
        // 5 multibyte codepoints
        let s = "αβγδε";
        assert_eq!(truncate_left(s, 3), "…δε");
    }
}
