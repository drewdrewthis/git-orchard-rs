package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRenderConf(t *testing.T) {
	var stdout, stderr bytes.Buffer
	self := "/opt/orchard/bin/orchard-shell"
	inner := "test-inner"
	outer := "test-outer"

	code := runRenderConf([]string{
		"--self", self,
		"--inner-socket", inner,
		"--outer-socket", outer,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runRenderConf exit = %d, stderr = %s", code, stderr.String())
	}

	want := substituteConf(embeddedConf, self, inner, outer)
	if !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("output does not match substituteConf(...)\ngot:\n%s\nwant:\n%s", stdout.String(), want)
	}

	for _, token := range []string{"@ORCHARD_SHELL@", "@INNER_SOCKET@", "@OUTER_SOCKET@"} {
		if strings.Contains(stdout.String(), token) {
			t.Errorf("output still contains unsubstituted token %q", token)
		}
	}
}
