package claudeprojects

import (
	"bytes"
	"encoding/json"
)

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
	return clipRecap(s.Content)
}
