package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// The two outer.conf copies must both forward the reorder chords and stay
// byte-identical: behavior forks by install path otherwise. AC13.
func TestOuterConfReorderBindsAndIdentical(t *testing.T) {
	const cmdConf = "../orchard-shell/outer.conf"
	const scriptConf = "../../scripts/outer-shell/outer.conf"
	a, err := os.ReadFile(cmdConf)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(scriptConf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("%s and %s differ; they must be byte-identical", cmdConf, scriptConf)
	}
	for _, bind := range []string{
		"bind -n M-S-Up send-keys -t 0.0 S-Up",
		"bind -n M-S-Down send-keys -t 0.0 S-Down",
	} {
		if !strings.Contains(string(a), bind) {
			t.Errorf("outer.conf is missing the reorder bind: %q", bind)
		}
	}
}
