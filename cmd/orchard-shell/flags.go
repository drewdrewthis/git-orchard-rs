package main

import (
	"flag"
	"fmt"
	"io"
)

// Defaults for every flag. The width matches outer.conf's resize hooks, which
// re-pin the sidebar to 40 (or 3 collapsed) on every outer resize — a
// different --width is honoured at boot but the hooks will pull it back on the
// next terminal resize until the persisted-width work of plan step 11 lands.
const (
	defaultInnerSocket = "default"
	defaultOuterSocket = "orchard-shell"
	defaultWidth       = 40

	// outerSessionName is fixed, not a flag. Isolation between wrappers comes
	// from the SOCKET, never the session name: two wrappers on one socket
	// would share a server and its options, which is not a second wrapper at
	// all. verify.sh relies on the same rule.
	outerSessionName = "shell"
)

// Options is one invocation's resolved configuration.
type Options struct {
	InnerSocket string // -L of the tmux server holding the user's sessions
	OuterSocket string // -L of the wrapper's own server
	Session     string // inner session to attach; empty means most-recently-attached
	Width       int    // sidebar pane width in columns
	Conf        string // outer.conf override; empty means the embedded copy
	Detach      bool   // boot the wrapper and exit without attaching
	ShowVersion bool
	Doctor      bool // the `doctor` subcommand
	DoctorJSON  bool // doctor --json
}

const usage = `orchard shell — a tmux wrapper that puts the orchard sidebar beside your session.

Usage:
  orchard shell [flags]
  orchard shell doctor [--json]

Flags:
  --inner-socket NAME   tmux -L socket holding your sessions (default %q)
  --session NAME        inner session to attach (default: most recently attached)
  --outer-socket NAME   tmux -L socket for the wrapper itself (default %q)
  --width N             sidebar width in columns (default %d)
  --conf PATH           outer tmux config to load instead of the embedded one
  --detach              boot the wrapper and exit without attaching
  --version             print the version and exit
`

// parseArgs turns argv (without the program name) into Options.
//
// `doctor` is matched positionally before flag parsing so its own --json does
// not have to be declared on the main flag set, where it would read as a
// global that most invocations ignore.
func parseArgs(argv []string, out io.Writer) (Options, error) {
	opts := Options{
		InnerSocket: defaultInnerSocket,
		OuterSocket: defaultOuterSocket,
		Width:       defaultWidth,
	}

	if len(argv) > 0 && argv[0] == "doctor" {
		opts.Doctor = true
		fs := newFlagSet("orchard shell doctor", out)
		fs.BoolVar(&opts.DoctorJSON, "json", false, "machine-readable output")
		if err := fs.Parse(argv[1:]); err != nil {
			return opts, err
		}
		return opts, rejectPositional(fs.Args())
	}

	fs := newFlagSet("orchard shell", out)
	fs.StringVar(&opts.InnerSocket, "inner-socket", opts.InnerSocket, "inner tmux -L socket")
	fs.StringVar(&opts.OuterSocket, "outer-socket", opts.OuterSocket, "outer tmux -L socket")
	fs.StringVar(&opts.Session, "session", "", "inner session to attach")
	fs.IntVar(&opts.Width, "width", opts.Width, "sidebar width in columns")
	fs.StringVar(&opts.Conf, "conf", "", "outer tmux config path")
	fs.BoolVar(&opts.Detach, "detach", false, "boot without attaching")
	fs.BoolVar(&opts.ShowVersion, "version", false, "print the version and exit")
	fs.Usage = func() { fmt.Fprintf(out, usage, defaultInnerSocket, defaultOuterSocket, defaultWidth) }
	if err := fs.Parse(argv); err != nil {
		return opts, err
	}
	if err := rejectPositional(fs.Args()); err != nil {
		return opts, err
	}
	if opts.Width < 1 {
		return opts, fmt.Errorf("--width must be at least 1, got %d", opts.Width)
	}
	return opts, nil
}

func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	return fs
}

// rejectPositional refuses leftover arguments rather than ignoring them: a
// mistyped flag that lands here would otherwise boot the wrapper with a
// silently different configuration.
func rejectPositional(rest []string) error {
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q", rest[0])
	}
	return nil
}
