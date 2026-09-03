//go:build darwin

package hostservice

// defaultServicesPerOS is the macOS default watchlist.
//
// `com.gitorchard.orchard` is the Label in
// `scripts/init/com.gitorchard.orchard.plist.template`, rendered and installed
// by `scripts/init/launchd-install.sh` — the daemon's own launchd unit. The
// `com.orchard.*` Labels match the
// orchardist tooling installed by `~/.claude/orchardist/scripts/orchardist`
// (see ~/.claude/orchardist/references/per-machine-startup.md). The list
// is intentionally narrow so a fresh install reports `state: active` for
// the daemon itself and `state: not_installed` for the orchardist Labels
// when they're absent.
var defaultServicesPerOS = []string{
	"com.gitorchard.orchard",
	"com.orchard.orchardist",
	"com.orchard.contracts-tick",
}
