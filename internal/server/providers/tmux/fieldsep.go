// fieldsep.go owns the tmux `-F` wire-format contract: the separator the
// adapter writes into every format string and splits every response row on.
//
// It lives in its own file, and is exported, because sibling packages stub
// the tmux CommandRunner and must emit rows in exactly this shape. When the
// separator lived only as an unexported const inside adapter.go, every stub
// hardcoded its own copy of the byte — and #662's change silently
// invalidated all of them at once (issues #664, #712).

package tmux

// FieldSep is the field separator used in `-F` format strings and when
// splitting tmux's response rows.
//
// MUST be a printable character that tmux's format engine emits verbatim.
// The previous choice — U+0001 (SOH) — does NOT round-trip on tmux 3.x:
// the engine renders a raw control byte in a -F template as its 4-char
// octal escape (`\001`), so a row like `name\001id\001…` came back as the
// literal text `name\001id\001…` with zero real separators. listAll then
// saw field-count==1, failed its `!= listAllFieldCount` guard, and dropped
// every row — surfacing as empty tmuxSessions / claudeInstances even though
// `tmux list-panes -a` clearly returned data. (The old comment claimed this
// was "verified against tmux 3.5+ on macOS"; the regression bites tmux 3.4
// on Linux.)
//
// TAB is tmux's conventional scripting delimiter and round-trips faithfully.
// The tmux metadata we read (session/window names, indexes, integers, pids,
// dimensions, pane_current_command, pane_title) does not contain tabs in
// practice — same risk posture the SOH choice assumed, with a separator that
// actually works.
//
// Exported so test stubs in other packages build their fake rows from the
// adapter's own value instead of a private copy that can drift out of sync.
const FieldSep = "\t"

// fieldSep is the in-package alias the adapter's format strings and parsers
// use. Kept so the hot paths read as package-local rather than exported API.
const fieldSep = FieldSep

// ListAllFieldCount is the number of fields in a listAllFormat row. Exported
// alongside FieldSep so cross-package stubs can assert their fake rows are
// the width the parser demands rather than being silently dropped.
const ListAllFieldCount = listAllFieldCount
