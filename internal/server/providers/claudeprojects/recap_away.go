package claudeprojects

import (
	"bytes"
	"encoding/json"
	"strings"
)

// RecapSource identifies which transcript record kind produced a recap.
// Values mirror the GraphQL `RecapSource` enum wire form so the mapping
// in Provider.ToGraphQL is a plain conversion.
type RecapSource string

const (
	// RecapSourceCommand is an explicit `/recap` slash-command output.
	RecapSourceCommand RecapSource = "RECAP_COMMAND"
	// RecapSourceAwaySummary is an autonomous `away_summary` system record
	// Claude Code writes between sessions without a slash command.
	RecapSourceAwaySummary RecapSource = "AWAY_SUMMARY"
)

// recapResult is the recap text paired with the record kind it came from.
type recapResult struct {
	Text   string
	Source RecapSource
}

// awaySummarySubtype is the `subtype` Claude Code stamps on the system
// records it writes autonomously as a background recap.
const awaySummarySubtype = "away_summary"

// parseSystemAwaySummary returns the recap text carried in a system
// `away_summary` record's `content`, or nil when the line is not such a
// record. Shape:
//
//	{"type":"system","subtype":"away_summary","content":"...","timestamp":"...","uuid":"..."}
//
// The content is the recap verbatim (no wrapper tags), clipped to
// maxRecapBytes to bound memory the same way /recap output is.
func parseSystemAwaySummary(line []byte) *string {
	if len(line) == 0 || !bytes.Contains(line, []byte(`"type":"system"`)) {
		return nil
	}
	var s systemRecord
	if err := json.Unmarshal(line, &s); err != nil {
		return nil
	}
	if s.Type != "system" || s.Subtype != awaySummarySubtype {
		return nil
	}
	text := strings.TrimSpace(s.Content)
	if text == "" {
		return nil
	}
	if len(text) > maxRecapBytes {
		text = text[:maxRecapBytes]
	}
	return &text
}
