// exec_env.go forces a UTF-8 ctype locale onto every tmux subprocess the
// adapter spawns (issue #701, defect D3).
//
// Why this is needed: tmux's client (client.c) sets its CLIENT_UTF8 flag from
// the client process's locale. When that locale is NOT UTF-8, the tmux server
// sanitizes non-printable bytes in `-F` output for that client — including the
// TAB the adapter uses as its field separator (fieldsep.go), which comes back
// as `_`. The parser then sees one field instead of ListAllFieldCount and
// drops every row, surfacing as an empty tmuxSessions/claudeInstances while
// `tmuxServer.alive` is still true.
//
// The daemon frequently runs with no UTF-8 locale in its environment: systemd
// `--user` units inherit no LANG, and macOS launchd agents are similar. Rather
// than change the wire format (SOH failed by octal-escaping, TAB fails by
// sanitization — see fieldsep.go), we fix the locale at the boundary the daemon
// controls: the child process env.

package tmux

import "strings"

// utf8Env returns env adjusted so a spawned tmux client runs under a UTF-8
// ctype locale. If the effective ctype (see effectiveCtype) is already UTF-8,
// env is returned unchanged. Otherwise any LC_ALL / LC_CTYPE entries are
// stripped and `LC_CTYPE=C.UTF-8` is appended.
//
// LC_ALL must be stripped, not merely overridden: it takes precedence over
// LC_CTYPE, so a non-UTF-8 LC_ALL (e.g. `LC_ALL=C`) would defeat an appended
// LC_CTYPE. C.UTF-8 is built into glibc/musl on modern Linux and accepted by
// macOS libc; it restores TAB fidelity and correct non-ASCII handling.
func utf8Env(env []string) []string {
	if isUTF8Ctype(effectiveCtype(env)) {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "LC_ALL" || key == "LC_CTYPE" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LC_CTYPE=C.UTF-8")
}

// effectiveCtype returns the locale value that governs character type for a
// child process: the first non-empty of LC_ALL, LC_CTYPE, LANG. This is POSIX
// category precedence — LC_ALL overrides every category, then the specific
// LC_CTYPE, then LANG as the fallback default. A later duplicate key wins,
// matching how the child's libc reads its environment.
func effectiveCtype(env []string) string {
	var lcAll, lcCtype, lang string
	for _, kv := range env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch key {
		case "LC_ALL":
			lcAll = val
		case "LC_CTYPE":
			lcCtype = val
		case "LANG":
			lang = val
		}
	}
	for _, v := range []string{lcAll, lcCtype, lang} {
		if v != "" {
			return v
		}
	}
	return ""
}

// isUTF8Ctype reports whether a locale value names a UTF-8 encoding, matching
// both `UTF-8` and `UTF8` spellings case-insensitively (e.g. en_US.UTF-8,
// C.UTF-8, C.utf8).
func isUTF8Ctype(v string) bool {
	l := strings.ToLower(v)
	return strings.Contains(l, "utf-8") || strings.Contains(l, "utf8")
}
