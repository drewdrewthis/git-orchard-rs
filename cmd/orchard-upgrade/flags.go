package main

import (
	"flag"
	"fmt"
	"io"
	"regexp"
)

// Options is one invocation's resolved configuration.
type Options struct {
	Check       bool   // report current/latest and change nothing
	DryRun      bool   // download and verify, then report without installing
	Target      string // release tag to install; empty means the latest
	Prefix      string // install directory; empty means the running binary's own
	ShowVersion bool   // print this binary's version
}

const usage = `orchard upgrade — download, verify and replace the installed orchard binaries.

Usage:
  orchard upgrade [flags]

Flags:
  --check              report the current and latest versions, change nothing
  --dry-run            download and verify, then report what would change
  --version            print this binary's version
  --version vX.Y.Z     install exactly that release (including a downgrade)
  --prefix DIR         install into DIR instead of the directory this binary is in

Environment:
  ORCHARD_RELEASE_REPO   owner/repo, or a base URL for a fixture API
`

// versionValue carries --version's two meanings. Bare, it prints this
// binary's version — the contract every orchard binary shares. With a value
// it pins the release to install. IsBoolFlag is what makes the bare form
// legal; the space-separated `--version vX.Y.Z` spelling arrives as a
// positional and is recovered in parseArgs.
type versionValue struct {
	value string
	set   bool
}

func (v *versionValue) String() string   { return v.value }
func (v *versionValue) IsBoolFlag() bool { return true }
func (v *versionValue) Set(s string) error {
	v.value, v.set = s, true
	return nil
}

var versionLike = regexp.MustCompile(`^v?[0-9]+\.[0-9]+`)

// parseArgs turns argv (without the program name) into Options.
func parseArgs(argv []string, out io.Writer) (Options, error) {
	var opts Options
	var ver versionValue

	fs := flag.NewFlagSet("orchard upgrade", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.BoolVar(&opts.Check, "check", false, "report versions and change nothing")
	fs.BoolVar(&opts.DryRun, "dry-run", false, "verify without installing")
	fs.StringVar(&opts.Prefix, "prefix", "", "install directory")
	fs.Var(&ver, "version", "print this binary's version, or pin a release")
	fs.Usage = func() { fmt.Fprint(out, usage) }
	if err := fs.Parse(argv); err != nil {
		return opts, err
	}

	rest := fs.Args()
	switch {
	case !ver.set:
	case ver.value != "true":
		opts.Target = ver.value // --version=vX.Y.Z
	case len(rest) > 0 && versionLike.MatchString(rest[0]):
		// --version vX.Y.Z. A bool flag does not consume the next argument,
		// and flag.Parse stops at the first non-flag one, so the pin and
		// everything after it arrive here unparsed. Take the pin, then hand
		// the remainder back to the same flag set.
		opts.Target = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return opts, err
		}
		rest = fs.Args()
	default:
		opts.ShowVersion = true // bare --version
	}

	if len(rest) > 0 {
		return opts, fmt.Errorf("unexpected argument %q", rest[0])
	}
	if opts.Check && opts.DryRun {
		return opts, fmt.Errorf("--check and --dry-run are alternatives: --check skips the download, --dry-run performs it")
	}
	return opts, nil
}
