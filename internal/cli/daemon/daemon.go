// Package daemon hosts the `orchard daemon {start,stop,status}` cobra
// subcommand group. It owns the daemon lifecycle from the user's POV;
// the actual GraphQL server lives in internal/server.
//
// Lifecycle model (v1, intentionally simple):
//   - `start` writes a pidfile, runs the server in the foreground, traps
//     SIGINT/SIGTERM for graceful shutdown, removes the pidfile on exit.
//   - `stop` reads the pidfile and sends SIGTERM to that pid.
//   - `status` reads the pidfile, probes /health if the pid is alive,
//     and prints a one-line summary.
//
// No double-fork, no PID 1 daemonisation, no syslog. launchd / systemd
// (scripts/init/) own backgrounding when the user wants it.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/orchardist/internal/orchpaths"
	"github.com/drewdrewthis/orchardist/internal/server"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeaccount"
	"github.com/drewdrewthis/orchardist/internal/server/providers/claudeprojects"
	configprovider "github.com/drewdrewthis/orchardist/internal/server/providers/config"
	"github.com/drewdrewthis/orchardist/internal/server/providers/gh"
	gitprovider "github.com/drewdrewthis/orchardist/internal/server/providers/git"
	"github.com/drewdrewthis/orchardist/internal/server/providers/hostservice"
	"github.com/drewdrewthis/orchardist/internal/server/providers/peerproxy"
	"github.com/drewdrewthis/orchardist/internal/server/providers/ps"
	"github.com/drewdrewthis/orchardist/internal/server/providers/repodiscovery"
	"github.com/drewdrewthis/orchardist/internal/server/providers/tmux"
)

// Command returns the `daemon` subcommand group rooted with three leaves.
// version is the binary version baked via -ldflags; it is surfaced through
// Query.version in the GraphQL schema (AC2, #417).
func Command(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the orchard GraphQL daemon",
		Long:  "Start, stop, or query the local orchard daemon listening on " + server.DefaultAddr + ".",
	}
	cmd.AddCommand(startCmd(version), stopCmd(), statusCmd())
	return cmd
}

func startCmd(version string) *cobra.Command {
	var addr string
	var logLevel string
	c := &cobra.Command{
		Use:   "start",
		Short: "Run the orchard daemon in the foreground",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd.Context(), addr, version, logLevel)
		},
	}
	c.Flags().StringVar(&addr, "addr", server.DefaultAddr, "host:port to bind")
	// Default is deliberately "" rather than "info": resolveLogLevel must be
	// able to tell "operator said nothing" (consult ORCHARD_LOG_LEVEL) from
	// "operator asked for info" (which must win over the env var).
	c.Flags().StringVar(&logLevel, "log-level", "", logLevelUsage)
	return c
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Send SIGTERM to the running orchard daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStop()
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the orchard daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.OutOrStdout())
		},
	}
}

// runStart is the foreground daemon entry point. It honours SIGINT and
// SIGTERM by cancelling the server context for a clean shutdown.
// version is the binary version injected via -ldflags; defaults to "dev".
// logLevel is the raw --log-level flag value ("" when unset, in which case
// ORCHARD_LOG_LEVEL is consulted).
func runStart(parentCtx context.Context, addr string, version string, logLevel string) error {
	// Resolve the level before anything else can log, and fail fast on a bad
	// value rather than starting a daemon at a verbosity nobody asked for.
	level, err := resolveLogLevel(logLevel, os.Getenv(EnvLogLevel))
	if err != nil {
		return err
	}

	pidPath, err := orchpaths.PidFile()
	if err != nil {
		return fmt.Errorf("resolve pidfile: %w", err)
	}
	if err := ensureParentDir(pidPath); err != nil {
		return err
	}
	if pid, err := readPid(pidPath); err == nil {
		// The pidfile names a process. Only honour it as "already running"
		// when that PID is alive AND genuinely an orchard-daemon. After a
		// crash that skipped `defer os.Remove(pidPath)`, the OS may have
		// recycled the PID to an unrelated process — a bare liveness probe
		// then falsely reports "already running" and refuses to start
		// (issue #665, a P0 outage). When the live PID is NOT the daemon,
		// the pidfile is stale: warn, reclaim it, and proceed to start.
		if !shouldTreatPidfileAsStale(pid, processAlive, processIsDaemon) {
			return fmt.Errorf("orchard daemon already running (pid %d, pidfile %s)", pid, pidPath)
		}
		if processAlive(pid) {
			fmt.Fprintf(os.Stderr, "orchard: stale pidfile %s names live non-daemon pid %d; reclaiming\n", pidPath, pid)
		}
	}
	if err := writePid(pidPath, os.Getpid()); err != nil {
		return fmt.Errorf("write pidfile: %w", err)
	}
	defer os.Remove(pidPath)

	ctx, cancel := signal.NotifyContext(parentCtx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Install the leveled logger as slog's default too: provider code that
	// reaches for slog.Default() must see the same level, otherwise
	// --log-level would only reach the call sites handed this logger.
	logger := newDaemonLogger(os.Stderr, level)
	slog.SetDefault(logger)

	cfgPath, err := orchpaths.ConfigFile()
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	configProvider := configprovider.NewProvider(
		configprovider.NewJSONFileAdapter(cfgPath, logger),
		logger,
	)
	if err := configProvider.Start(ctx); err != nil {
		// Emit a migration hint when the new config is absent but the legacy
		// ~/.config/orchard/config.json still exists. The legacy file is never
		// read — the hint is informational only. The stat happens once here,
		// not in ConfigFile() or any other path helper.
		legacyPath, legacyExists, _ := orchpaths.LegacyConfigFile()
		if legacyExists {
			fmt.Fprintf(os.Stderr, "orchard: %s\n",
				orchpaths.MigrationHintMessage(legacyPath, cfgPath))
		} else {
			fmt.Fprintf(os.Stderr, "orchard: config not found at %s\n", cfgPath)
		}
		return fmt.Errorf("start config provider: %w", err)
	}
	defer func() { _ = configProvider.Stop() }()

	psProvider := ps.New(ps.NewAdapter("local"), logger)
	tmuxProvider := tmux.New(tmux.NewAdapter(localHostID()), logger)
	claudeProjectsRoot := claudeprojectsRoot()
	claudeProjectsProvider := claudeprojects.New(claudeProjectsRoot, "local", logger)

	// Hydrate the claudeprojects cache before discovery runs so the
	// claudeprojects source can contribute repo roots from the first
	// boot tick (issue #527). Failures here are non-fatal — the
	// discoverer simply gets a smaller initial union.
	if err := claudeProjectsProvider.Start(ctx); err != nil {
		logger.Warn("claudeprojects: pre-start failed; discovery may miss conversation cwds at boot", "err", err)
	}

	// Repo-discovery union (issue #527): configured repos + tmux pane
	// CWDs + Claude conversation CWDs, with phantom guard and
	// excluded[] filter. The git provider takes this as its
	// project-lister so worktree enumeration covers every discovered
	// repo, not just config.json entries.
	repoDiscoverer := repodiscovery.NewProvider(configProvider, configProvider, logger).
		AddSource("tmux", repodiscovery.NewTmuxSource()).
		AddSource("claudeprojects", repodiscovery.NewClaudeProjectsSource(claudeProjectsProvider))

	gitProvider, err := buildGitProvider(ctx, repoDiscoverer, logger)
	if err != nil {
		return fmt.Errorf("build git provider: %w", err)
	}

	// Hot-reload: when ~/.orchard/config.json changes, re-list repos and
	// converge the git provider via ApplyProjects (issue #571). Failures
	// to subscribe are non-fatal — the daemon still serves the boot-time
	// project set.
	gitHotReload := newGitConfigSubscriber(repoDiscoverer, gitProvider, logger)
	gitHotReload.start(ctx, configProvider.Subscribe(ctx))
	defer gitHotReload.close()

	claudeAccountProvider := claudeaccount.New("local", logger)

	hsvc, hsvcErr := buildHostServiceProvider(ctx)
	if hsvcErr != nil {
		fmt.Fprintf(os.Stderr, "orchard: hostservice unavailable: %v\n", hsvcErr)
	}

	// Wire the gh provider unconditionally. Auth failures (gh CLI
	// missing, user not logged in) are non-fatal: the provider remembers
	// the error and surfaces it as a per-field GraphQL error on
	// `pullRequests`/`issues`/`workflowRuns`, while sibling fields keep
	// resolving (ADR-011 §6 / §12). We probe auth here purely so a
	// missing setup is loud at boot time — matching the hostservice
	// fallback pattern above.
	ghProvider := gh.New(logger, os.Getenv(gh.EnvAPIBaseURL))
	if err := ghProvider.AuthError(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "orchard: gh unavailable: %v\n", err)
	}

	fedCfg, err := peerproxy.LoadFederationConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load federation config: %w", err)
	}
	peerProvider := peerproxy.NewProvider(fedCfg, logger)
	if err := peerProvider.Start(ctx); err != nil {
		return fmt.Errorf("peerproxy start: %w", err)
	}
	defer func() { _ = peerProvider.Stop() }()

	peerWatcher := peerproxy.NewConfigWatcher(cfgPath, peerProvider, logger)
	if err := peerWatcher.Start(ctx); err != nil {
		// Non-fatal: log and continue without hot-reload.
		logger.Warn("peerproxy: config watcher unavailable; hot-reload disabled", "err", err)
	} else {
		defer func() { _ = peerWatcher.Close() }()
	}

	localEvents := peerproxy.NewLocalInvalidator()

	opts := []server.Option{
		server.WithVersion(version),
		server.WithRepos(repoDiscoverer),
		server.WithGit(gitProvider),
		server.WithPS(psProvider),
		server.WithTmux(tmuxProvider),
		server.WithClaudeProjects(claudeProjectsProvider),
		server.WithConversationsJSONL(claudeProjectsProvider, claudeProjectsRoot),
		server.WithClaudeAccount(claudeAccountProvider),
		server.WithGh(ghProvider),
		server.WithPeerProxy(peerProvider),
		server.WithLocalEvents(localEvents),
		server.WithGitMutations(orchardScriptsRoot()),
	}
	if hsvc != nil {
		opts = append(opts, server.WithHostService(hsvc))
	}

	srv := server.New(addr, logger, opts...)
	return srv.Run(ctx)
}

// localHostID returns the host id for tmux nodes. v1 stays neutral.
func localHostID() tmux.HostID {
	return tmux.HostID("local")
}

// buildGitProvider constructs the git provider and registers every
// project from the supplied lister. The git provider owns a watcher
// per project; resolvers query it for `Project.worktrees` and node-id
// lookups.
//
// The lister parameter is a resolvers.ReposLister so callers can pass
// either the raw configured provider (legacy) or the repodiscovery
// union (issue #527). The git provider must learn about every repo
// the resolver layer can surface, otherwise worktree lookups on
// discovered repos return empty.
//
// Per-project AddProject failures (e.g. an fsnotify watcher couldn't be
// installed) are logged and skipped so the daemon still boots — the
// affected project simply won't surface live worktree updates until it
// is reconfigured.
func buildGitProvider(ctx context.Context, lister gitProjectLister, logger *slog.Logger) (*gitprovider.Provider, error) {
	gp := gitprovider.NewProvider(logger)
	repos, err := lister.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	for _, p := range repos {
		if err := gp.AddProject(gitprovider.Project{ID: string(p.ID), Dir: p.Path}); err != nil {
			logger.Warn("git: skipping repo, watcher unavailable",
				"repo", p.ID, "path", p.Path, "err", err)
		}
	}
	return gp, nil
}

// gitProjectLister is the local narrow contract `buildGitProvider`
// needs. Both [configprovider.Provider] and [repodiscovery.Provider]
// satisfy it.
type gitProjectLister interface {
	List(ctx context.Context) ([]configprovider.Repo, error)
}

// buildHostServiceProvider resolves the local machine id, reads the
// services watchlist from config, and constructs the hostservice provider.
func buildHostServiceProvider(ctx context.Context) (*hostservice.Provider, error) {
	hostID, err := hostservice.LocalHostID(ctx)
	if err != nil {
		return nil, fmt.Errorf("read machine identity: %w", err)
	}
	cfgPath, err := orchpaths.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	services, err := hostservice.LoadServicesFromConfig(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load services watchlist: %w", err)
	}
	return hostservice.New(hostID, services), nil
}

// runStop reads the pidfile and signals the running daemon.
func runStop() error {
	pidPath, err := orchpaths.PidFile()
	if err != nil {
		return err
	}
	pid, err := readPid(pidPath)
	if err != nil {
		return fmt.Errorf("daemon not running (no pidfile at %s)", pidPath)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process gone — clear the stale pidfile and report success.
		if errors.Is(err, os.ErrProcessDone) || strings.Contains(err.Error(), "process already finished") {
			_ = os.Remove(pidPath)
			return fmt.Errorf("daemon already stopped (cleared stale pidfile)")
		}
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	// Wait briefly for the process to exit and remove its own pidfile.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon (pid %d) did not exit within 5s", pid)
}

// runStatus prints a one-line summary suitable for human eyes and shell
// scripting. Exit code 0 when up, non-zero when down (cobra does this
// automatically when RunE returns an error).
func runStatus(w io.Writer) error {
	pidPath, err := orchpaths.PidFile()
	if err != nil {
		return err
	}
	pid, err := readPid(pidPath)
	if err != nil {
		return fmt.Errorf("orchard daemon: down (no pidfile at %s)", pidPath)
	}
	if !processAlive(pid) {
		return fmt.Errorf("orchard daemon: down (stale pidfile, pid %d not alive)", pid)
	}
	uptime, healthErr := probeHealth()
	if healthErr != nil {
		fmt.Fprintf(w, "orchard daemon: up (pid %d) — health probe failed: %v\n", pid, healthErr)
		return nil
	}
	fmt.Fprintf(w, "orchard daemon: up (pid %d, uptime %ds)\n", pid, uptime)
	return nil
}

// probeHealth fetches /health and returns the reported uptime in seconds.
//
// Issue #405: Go's HTTP client resolves `localhost` via the system
// resolver, which on IPv6-enabled Linux boxes returns `::1` first.
// The daemon's default listener binds 127.0.0.1 (IPv4 only), so the
// probe sees `dial tcp [::1]:7777: connection refused` and `daemon
// status` reports a false negative even when the daemon is healthy.
// We rewrite a leading `localhost` to `127.0.0.1` so the probe
// targets the same address space the listener uses.
func probeHealth() (int64, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + probeAddr(server.DefaultAddr) + "/health")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		Status  string `json:"status"`
		UptimeS int64  `json:"uptimeS"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, fmt.Errorf("decode: %w", err)
	}
	return payload.UptimeS, nil
}

// probeAddr rewrites a `host:port` so the health probe doesn't go
// through the system resolver. Specifically: a leading `localhost`
// becomes `127.0.0.1`. Any other host (explicit `127.0.0.1`, an
// `--addr 0.0.0.0:8000` override, an FQDN, an IP literal) is returned
// unchanged. This is the only place we need this rewrite — production
// HTTP clients can resolve normally.
func probeAddr(addr string) string {
	if strings.HasPrefix(addr, "localhost:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "localhost:")
	}
	if addr == "localhost" {
		return "127.0.0.1"
	}
	return addr
}

// claudeprojectsRoot returns the directory the daemon should watch for
// Claude Code transcripts. CLAUDE_PROJECTS_ROOT overrides; default is
// ~/.claude/projects.
func claudeprojectsRoot() string {
	if v := os.Getenv("CLAUDE_PROJECTS_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".claude/projects"
	}
	return home + "/.claude/projects"
}

// orchardScriptsRoot returns the absolute path to the scripts/ directory.
// Resolution order (first candidate whose git/worktree-remove.sh exists wins):
//
//  1. ORCHARD_SCRIPTS_ROOT env var — operator override, highest priority.
//  2. <binDir>/../share/orchard/scripts — installed layout:
//     /usr/local/bin/orchard-daemon → /usr/local/share/orchard/scripts
//     (populated by `make install-scripts`).
//  3. <binDir>/../scripts — dev/checkout layout:
//     bin/orchard-daemon → scripts/ at the repo root.
//  4. "scripts" — relative to cwd, last resort for unit tests.
func orchardScriptsRoot() string {
	if v := os.Getenv("ORCHARD_SCRIPTS_ROOT"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		return "scripts"
	}
	binDir := pathDir(exe)
	candidates := []string{
		// Installed layout: /usr/local/share/orchard/scripts
		binDir + "/../share/orchard/scripts",
		// Dev/checkout layout: scripts/ relative to the repo root
		binDir + "/../scripts",
	}
	for _, c := range candidates {
		if scriptsRootExists(c) {
			return c
		}
	}
	return "scripts"
}

// scriptsRootExists reports whether dir contains git/worktree-remove.sh,
// which is the sentinel file used to confirm a scripts root is populated.
func scriptsRootExists(dir string) bool {
	_, err := os.Stat(dir + "/git/worktree-remove.sh")
	return err == nil
}

func ensureParentDir(path string) error {
	dir := pathDir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return nil
}

func pathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

func writePid(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o644)
}

func readPid(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("malformed pidfile: %w", err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("malformed pidfile: pid %d not positive", pid)
	}
	return pid, nil
}

// processAlive sends signal 0, which performs error checking but does
// not actually deliver a signal. Standard Unix idiom for "is pid alive?"
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// daemonProcessName is the executable basename of the daemon binary
// (see cmd/orchard-daemon). It is what /proc/<pid>/comm reports and what
// /proc/<pid>/cmdline starts with for a running daemon.
const daemonProcessName = "orchard-daemon"

// procNameReader returns a best-effort identifier for the process at pid
// — on Linux the contents of /proc/<pid>/comm and /proc/<pid>/cmdline.
// ok is false when the platform offers no introspection (no /proc), so
// callers can fall back conservatively. It is a package var so tests can
// inject a fake without depending on the real /proc.
var procNameReader = readProcName

// processIsDaemon reports whether the (assumed-alive) process at pid is
// genuinely an orchard-daemon. On Linux this inspects /proc; where the
// process name cannot be determined (non-Linux, or /proc unreadable) it
// falls back to the old liveness-only behaviour — assuming the PID is the
// daemon — so we never silently double-start on platforms we cannot
// introspect.
func processIsDaemon(pid int) bool {
	name, ok := procNameReader(pid)
	if !ok {
		// Conservative fallback: cannot introspect, so treat a live PID as
		// the daemon (preserves the pre-#665 double-start guard).
		return true
	}
	return strings.Contains(name, daemonProcessName)
}

// shouldTreatPidfileAsStale decides what runStart's pidfile gate should
// do, factored out of runStart so the three-way decision is unit-testable
// without a pidfile, a signal trap, or an addr binding. It composes the
// liveness probe (alive) with the daemon-identity probe (isDaemon):
//
//   - dead PID                  → stale (proceed to start)
//   - live PID, not the daemon  → stale (PID reuse; reclaim and start) — issue #665
//   - live PID, is the daemon   → NOT stale (genuine "already running")
//
// alive and isDaemon are injected so tests can drive every branch
// deterministically.
func shouldTreatPidfileAsStale(pid int, alive func(int) bool, isDaemon func(int) bool) bool {
	if !alive(pid) {
		return true
	}
	return !isDaemon(pid)
}

// readProcName reads the Linux /proc entries that identify a process.
// It concatenates /proc/<pid>/comm and a space-joined /proc/<pid>/cmdline
// so a substring match against daemonProcessName catches both the
// (15-char-truncated) comm name and a path-prefixed argv[0]. ok is false
// when neither file can be read — including on platforms without /proc,
// where the open simply fails.
func readProcName(pid int) (name string, ok bool) {
	var parts []string
	if comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err == nil {
		parts = append(parts, strings.TrimSpace(string(comm)))
	}
	if cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		// cmdline args are NUL-separated; join with spaces for matching.
		parts = append(parts, strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " "))
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, " "), true
}
