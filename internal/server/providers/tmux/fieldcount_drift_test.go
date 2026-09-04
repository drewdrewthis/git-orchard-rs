package tmux

import (
	"strings"
	"testing"
)

// The listAll/listClients parsers drop any row whose split does not yield
// exactly the format's field count. That count is a hand-maintained const
// (ListAllFieldCount is exported API, so it cannot be a strings.Count
// expression). These guards fail the instant a format string gains or
// loses a field without its count being updated — the drift that silently
// dropped every row in #664/#712.

func TestListAllFieldCountMatchesFormat(t *testing.T) {
	if got := strings.Count(listAllFormat, fieldSep) + 1; got != listAllFieldCount {
		t.Fatalf("listAllFormat has %d fields but listAllFieldCount = %d", got, listAllFieldCount)
	}
}

func TestClientFieldCountMatchesFormat(t *testing.T) {
	if got := strings.Count(clientFormat, fieldSep) + 1; got != clientFieldCount {
		t.Fatalf("clientFormat has %d fields but clientFieldCount = %d", got, clientFieldCount)
	}
}
