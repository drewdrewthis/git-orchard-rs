package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/drewdrewthis/orchardist/internal/release"
)

// upgrade runs one invocation against a resolved install directory.
func upgrade(ctx context.Context, opts Options, dir, current string, out io.Writer) error {
	client := release.NewClient()

	// A person's --version pin ("1.1.0", "v1.1.0") is not itself a real
	// release-please tag ("orchard-v1.1.0") — normalize it once here, before
	// it reaches Resolve/ByTag (directly, or via check's --check path),
	// rather than in flags.go: this is the one place that turns a parsed
	// Option into a call against the release package, and flags.go has no
	// business knowing the release package's tag shape.
	opts.Target = release.NormalizeTag(opts.Target)

	if opts.Check {
		return check(ctx, client, opts.Target, current, out)
	}

	triple, err := release.HostTriple()
	if err != nil {
		return err
	}

	// The writability probe runs BEFORE the download, not after: failing on
	// the last step of a 40MB fetch, having already made the user wait, is
	// the same refusal delivered as badly as possible.
	if !opts.DryRun {
		if err := release.Writable(dir); err != nil {
			return fmt.Errorf("%w — re-run with a writable --prefix, or with the privileges that own %s", err, dir)
		}
	}

	// One lookup, not two: an unpinned upgrade needs the latest release both
	// to decide whether to act and to download from.
	rel, err := release.Resolve(ctx, client, opts.Target)
	if err != nil {
		return err
	}
	if opts.Target == "" && !release.IsNewer(rel.Version(), current) {
		fmt.Fprintf(out, "orchard %s is already the latest release.\n", current)
		return nil
	}

	suite, err := release.FetchSuiteFromRelease(ctx, client, rel, triple)
	if err != nil {
		return err
	}
	plan := suite.Plan(dir, exists)
	if len(plan) == 0 {
		return fmt.Errorf("no orchard binaries found in %s — nothing to upgrade (install with scripts/install.sh)", dir)
	}

	if opts.DryRun {
		fmt.Fprintf(out, "would install orchard %s (from %s) into %s:\n", suite.Version, suite.Asset, dir)
		for _, item := range plan {
			fmt.Fprintf(out, "  %s\n", filepath.Base(item.Path))
		}
		return nil
	}

	results, err := release.ReplaceAll(plan)
	if err != nil {
		return err
	}
	invalidateUpdateCheckCache()
	fmt.Fprintf(out, "orchard %s in %s:\n", suite.Version, dir)
	unchanged := 0
	for _, r := range results {
		fmt.Fprintf(out, "  %s: %s\n", filepath.Base(r.Path), r.Action)
		if r.Action == release.ActionUnchanged {
			unchanged++
		}
	}
	if unchanged == len(results) {
		fmt.Fprintf(out, "already up to date (%d unchanged)\n", unchanged)
	}
	return nil
}

// check reports the current and available versions without writing anything —
// not a download, not a temp file, not the update-check cache.
func check(ctx context.Context, client *release.Client, target, current string, out io.Writer) error {
	rel, err := release.Resolve(ctx, client, target)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "current: %s\n", current)
	if current == release.DevVersion {
		fmt.Fprintf(out, "  (current version is a dev build; comparison is not meaningful)\n")
	}
	fmt.Fprintf(out, "latest:  %s\n", rel.Version())
	if release.IsNewer(rel.Version(), current) {
		fmt.Fprintf(out, "an update is available — run: orchard upgrade\n")
	} else {
		fmt.Fprintf(out, "up to date\n")
	}
	return nil
}

// exists reports whether path is a regular file, which is what "this binary
// is installed here" means. A directory or a dangling symlink is neither
// installed nor replaceable.
func exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// invalidateUpdateCheckCache clears the update-check cache after a real
// install, so `orchard shell doctor` and the sidebar never read back a check
// the just-replaced binary wrote (see release.LoadCheckFor,
// release.InvalidateCheck). Best-effort and silent: an unresolvable state
// dir is the same "nothing to clean up" case InvalidateCheck already treats
// a missing file as, and neither should fail an otherwise successful
// upgrade.
func invalidateUpdateCheckCache() {
	if path, err := release.CheckPath(); err == nil {
		release.InvalidateCheck(path)
	}
}
