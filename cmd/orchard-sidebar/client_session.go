package main

import (
	"strconv"
	"strings"
)

// parseListClients parses the output of
//
//	tmux list-clients -F "#{client_activity} #{?@orchard_sidebar_width,#{@orchard_sidebar_width},0} #{client_session}"
//
// into the number of attached clients, the session of the most recently
// active one, and the shared width option (0 if unset/unreadable).
//
// strings.Split(strings.TrimSpace(out), "\n") on empty input returns
// []string{""} -- one element, not zero -- so a line must parse as a real
// client before it is counted. Counting split elements unconditionally
// reported clients == 1 for a genuinely empty (all-detached) read, defeating
// the m.clients == 0 detached-resize branch it exists to drive (#736;
// review finding on #745, discussion_r3918434325).
func parseListClients(out []byte) (clients int, session string, width int) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0, "", 0
	}
	best, bestAct := "", int64(-1)
	for _, ln := range strings.Split(trimmed, "\n") {
		if ln == "" {
			continue
		}
		act, rest, ok := strings.Cut(ln, " ")
		if !ok {
			continue
		}
		ws, name, ok := strings.Cut(rest, " ")
		if !ok || name == "" {
			continue
		}
		n, err := strconv.ParseInt(act, 10, 64)
		if err != nil {
			continue
		}
		clients++
		if w, err := strconv.Atoi(ws); err == nil && w > 0 {
			width = w
		}
		if n > bestAct {
			best, bestAct = name, n
		}
	}
	return clients, best, width
}
