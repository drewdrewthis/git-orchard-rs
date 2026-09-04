// version.go: version is baked at release time via -ldflags
// "-X main.version=$(VERSION)" (see Makefile `sidebar` target), mirroring
// the pattern in cmd/orchard-daemon/main.go. orchard-sidebar has no cobra
// (or any flag-parsing) dependency, so the check is a manual os.Args peek —
// the same idiom main.go already uses for its `launch` subcommand.
package main

import (
	"fmt"
	"os"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// version is overridden via -ldflags at release time.
var version = "dev"

// handleVersionFlag prints the version and exits when invoked with
// --version or -v; otherwise it returns and the caller continues normal
// startup. Call this as the first line of main(), before the "launch"
// subcommand check (#747 Step 5).
//
// --revision prints the bare VCS revision instead: `orchard-shell doctor` execs
// it to compare against its own, catching a stale build that reports the same
// --version but comes from a different commit (#787 AC3).
func handleVersionFlag() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println("orchard-sidebar " + version)
		os.Exit(0)
	}
	if release.HandleRevisionFlag(os.Args[1:], os.Stdout) {
		os.Exit(0)
	}
}
