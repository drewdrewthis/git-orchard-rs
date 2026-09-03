# Service units for the orchard daemon

| File | Platform |
|------|----------|
| `com.gitorchard.orchard.plist.template` + `launchd-install.sh` | macOS (launchd) |
| `orchard.service` | Linux (systemd user unit) |

## macOS

```sh
scripts/init/launchd-install.sh
launchctl load -w ~/Library/LaunchAgents/com.gitorchard.orchard.plist
```

The plist ships as a **template**. Do not `cp` it into `~/Library/LaunchAgents`
yourself: launchd does not expand `~` or environment variables in
`StandardOutPath` / `StandardErrorPath`, so the log paths must be absolute at
install time. `launchd-install.sh` substitutes `__ORCHARD_STATE_DIR__` and
creates that directory — launchd opens the redirect targets *before* exec'ing
the daemon, so a missing parent directory yields no log and no error.

| Flag | Effect |
|------|--------|
| `--print` | render to stdout, install nothing |
| `--dest <dir>` | install somewhere other than `~/Library/LaunchAgents` |
| `--json` | L2 envelope: `{"ok":true,"data":{"path","stateDir","outLog","errLog"}}` |

## Linux

```sh
mkdir -p ~/.config/systemd/user
cp scripts/init/orchard.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now orchard.service
```

Logs go to the journal: `journalctl --user -u orchard.service -f`.

## Logs

| Platform | Location |
|----------|----------|
| macOS | `$XDG_STATE_HOME/orchard/` or `~/.local/state/orchard/` — `orchard.out.log`, `orchard.err.log` |
| Linux | systemd journal |

The state directory resolves the same way `internal/orchpaths.StateDir` does,
so logs sit beside the daemon's pidfile.

## Verbosity

Default is `info`. Raise or lower it with either:

- `--log-level debug|info|warn|error` appended to `daemon start`
- `ORCHARD_LOG_LEVEL=debug` in the environment (the flag wins when both are set)

At `info` the daemon logs one line per outbound GitHub API call
(`gh: api call`, with `kind`, `repo`, `duration_ms`, `rate_limit_remaining`) and
warns on rate-limit cooldowns and stale-cache fallbacks. Counting the
`gh: api call` lines over a window is the call-volume audit.
