package main

import (
	"flag"
	"fmt"
	"io"
)

// runRenderConf prints the outer.conf that orchard-shell would materialise
// for a given self path and socket pair — the same substituteConf (conf.go)
// the boot path uses, exposed as a subcommand so a shell script (verify.sh)
// can render the exact same bytes rather than loading the embedded template
// raw with its @ORCHARD_SHELL@/@INNER_SOCKET@/@OUTER_SOCKET@ tokens
// unsubstituted.
func runRenderConf(argv []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("orchard-shell render-conf", flag.ContinueOnError)
	fs.SetOutput(stderr)
	self := fs.String("self", "", "path substituted for @ORCHARD_SHELL@")
	inner := fs.String("inner-socket", defaultInnerSocket, "inner tmux -L socket")
	outer := fs.String("outer-socket", defaultOuterSocket, "outer tmux -L socket")
	if err := fs.Parse(argv); err != nil {
		return 0
	}

	if _, err := stdout.Write(substituteConf(embeddedConf, *self, *inner, *outer)); err != nil {
		fmt.Fprintf(stderr, "render-conf: %v\n", err)
		return 1
	}
	return 0
}
