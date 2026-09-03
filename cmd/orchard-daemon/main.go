// Command orchard-daemon is the daemon + read-side query binary for orchard.
//
// Per ADR-013, this binary is dispatched from the user-facing `orchard`
// dispatcher: `orchard daemon start` execs `orchard-daemon daemon start`,
// `orchard query ...` execs `orchard-daemon query ...`, etc. Users do not
// invoke `orchard-daemon` directly. The cobra `Use:` field below is set to
// `orchard-daemon` so help output reflects the dispatched form correctly.
//
// Subcommand groups live under internal/cli/.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/orchardist/internal/cli/config"
	"github.com/drewdrewthis/orchardist/internal/cli/daemon"
	"github.com/drewdrewthis/orchardist/internal/cli/query"
	"github.com/drewdrewthis/orchardist/internal/release"
)

// version is overridden via -ldflags at release time.
var version = "dev"

func main() {
	// --revision answers before cobra parses: it is not a cobra flag, and the
	// shared helper keeps every suite binary's output identical (orchardist#787).
	if release.HandleRevisionFlag(os.Args[1:], os.Stdout) {
		os.Exit(0)
	}

	root := &cobra.Command{
		Use:           "orchard-daemon",
		Short:         "orchard-daemon — query and cache layer over local developer state",
		Long:          "orchard-daemon is the read-only join layer over git, tmux, claude, processes, and federated peers. See ADR-011 for the design. Typically dispatched as `orchard daemon ...` per ADR-013.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(daemon.Command(version))
	root.AddCommand(config.Command())
	root.AddCommand(query.Command())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "orchard-daemon:", err)
		os.Exit(1)
	}
}
