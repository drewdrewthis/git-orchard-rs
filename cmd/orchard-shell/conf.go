package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"

	"github.com/drewdrewthis/orchardist/internal/orchpaths"
	"github.com/drewdrewthis/orchardist/internal/release"
)

// embeddedConf is the outer tmux server's config, compiled into the binary.
//
// A `-L` socket does NOT suppress loading the user's real ~/.tmux.conf, so
// every outer-socket invocation must pass `-f`; embedding the file is what
// makes that path exist on a machine that has no orchard checkout.
//
//go:embed outer.conf
var embeddedConf []byte

// confBaseName is the materialised file's name, content-hashed so an upgrade
// can never leave a stale conf behind for a new binary to load: different
// bytes, different path.
func confBaseName(data []byte) string {
	return fmt.Sprintf("outer-%s.conf", release.SHA256(data)[:12])
}

// resolveConf returns the outer.conf path every outer-socket tmux invocation
// loads with -f.
//
// An explicit --conf is used as given (and must exist — silently falling back
// to the embedded copy would hide a typo behind behaviour that looks almost
// right). Otherwise the embedded copy is materialised under the state dir.
func resolveConf(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("--conf %s: %w", override, err)
		}
		return override, nil
	}
	dir, err := orchpaths.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return materializeConf(dir, embeddedConf)
}

// materializeConf writes data to dir under its content-hashed name, and
// returns that path. An existing file with the right content is left alone.
func materializeConf(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, confBaseName(data))
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return path, nil
	}
	if err := release.Replace(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
