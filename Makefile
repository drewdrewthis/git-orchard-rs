# Top-level Makefile for the polyglot orchardist repo.
#
# Helper binaries coexist behind one dispatcher (per ADR-013; #747 amendment
# added shell + upgrade):
#   - `orchard` (Rust)          — thin dispatcher; routes verbs to helpers.
#   - `orchard-tui` (Rust)      — TUI dashboard. Dispatched as `orchard tui`.
#   - `orchard-daemon` (Go)     — GraphQL daemon + read queries + config.
#                                 Dispatched as `orchard daemon ...`.
#   - `orchard-worktree` (Rust) — worktree mutation CLI. Dispatched as
#                                 `orchard worktree ...` and via bare verbs
#                                 (`orchard new`, `orchard rm`, etc.).
#   - `orchard-sidebar` (Go)    — tmux sidebar pane. Launched by orchard-shell.
#   - `orchard-shell` (Go)      — outer tmux wrapper. Dispatched as `orchard shell`.
#   - `orchard-upgrade` (Go)    — release client. Dispatched as `orchard upgrade`.
#
# All Go binaries and both Rust binaries above (orchard, orchard-tui) ship in
# the release suite (internal/release/assets.go's `SuiteBinaries`);
# orchard-worktree does not. VERSION (below) is the one version every binary
# bakes in via -ldflags — see Steps 5/6 of docs/plans/747-product-plan.md.
#
# Build artifacts:
#   bin/orchard-{daemon,sidebar,shell,upgrade}   — Go binaries
#   target/release/orchard                       — dispatcher (Rust)
#   target/release/orchard-tui                   — TUI (Rust)
#   target/release/orchard-worktree               — worktree CLI (Rust)
#   dist/                                        — `make dist`: cross-platform
#                                                   release tarballs + SHA256SUMS
#
# Install layout (after `make install`):
#   /usr/local/bin/orchard                → dispatcher (the only binary
#                                            users invoke directly)
#   /usr/local/bin/orchard-{tui,daemon,worktree,sidebar,shell,upgrade}
#                                         → helper binaries the dispatcher
#                                            execs by name

.PHONY: daemon sidebar shell upgrade generate rust dispatcher worktree-cli gui all clean \
        install install-daemon install-sidebar install-shell install-upgrade \
        install-dispatcher install-tui install-worktree-cli install-scripts \
        test test-go test-rust bats-install bats-test dist \
        check-feature-parity check-feature-parity-daemon check-feature-parity-tui check-feature-parity-gui

# Single source of truth: crates/orchard/Cargo.toml's `version` field (the
# only package release-please tracks — Go binaries ride its tag, see ADR).
# Override for a one-off build: make daemon VERSION=1.2.3
VERSION ?= $(shell awk -F'"' '/^version = / { print $$2; exit }' crates/orchard/Cargo.toml)

# REVISION is the VCS commit baked into every Go binary so doctor's
# suite-revisions check can compare builds (orchardist#803). Empty in a
# tarball with no .git falls back to the compiler's own vcs.revision stamp.
REVISION ?= $(shell git rev-parse HEAD 2>/dev/null)
GO_LDFLAGS = -X main.version=$(VERSION) -X github.com/drewdrewthis/orchardist/internal/release.revision=$(REVISION)

# Go binaries — orchard-daemon, orchard-sidebar, orchard-shell, orchard-upgrade.
# Each bakes VERSION and REVISION via -ldflags: make sidebar VERSION=1.2.3
daemon:
	go build -ldflags "$(GO_LDFLAGS)" -o bin/orchard-daemon ./cmd/orchard-daemon

sidebar:
	go build -ldflags "$(GO_LDFLAGS)" -o bin/orchard-sidebar ./cmd/orchard-sidebar

# cmd/orchard-shell — outer tmux wrapper (landing alongside this change).
shell:
	go build -ldflags "$(GO_LDFLAGS)" -o bin/orchard-shell ./cmd/orchard-shell

# cmd/orchard-upgrade — release client (landing alongside this change).
upgrade:
	go build -ldflags "$(GO_LDFLAGS)" -o bin/orchard-upgrade ./cmd/orchard-upgrade

# Generate gqlgen types/stubs from schema.graphql + gqlgen.yml.
# Generated files live under internal/server/graphql/ and are committed
# (see README — schema-first, codegen is reproducible but committing
# keeps the build hermetic in CI without forcing a gqlgen install).
generate:
	go generate ./...
	# Mirror schema.graphql into the resolvers package so go:embed can
	# bake it into the daemon binary for Query.schemaSDL (#469 F10).
	cp schema.graphql internal/server/resolvers/schema.graphql
	# Concatenate root partial + per-domain partials into the combined
	# embedded schema for daemon/daemon-self (Query.schemaSDL post-refactor).
	# daemon/daemon-self/resolver_health.go embeds this file at build time.
	cat daemon/schema.graphql daemon/*/schema.graphql > daemon/daemon-self/schema_combined.graphql

# Rust release builds — one target per crate.
rust: dispatcher
	ORCHARD_REVISION=$(REVISION) cargo build --release -p orchard
	cargo build --release -p orchard-worktree

dispatcher:
	cargo build --release -p orchard-dispatcher

worktree-cli:
	cargo build --release -p orchard-worktree

gui:
	cd crates/orchard-gui/src-tauri && cargo tauri build

all: daemon rust

install: install-dispatcher install-daemon install-sidebar install-shell install-upgrade install-tui install-worktree-cli install-scripts

install-dispatcher: dispatcher
	install -m 755 target/release/orchard /usr/local/bin/orchard

install-daemon: daemon
	install -m 755 bin/orchard-daemon /usr/local/bin/orchard-daemon

install-sidebar: sidebar
	install -m 755 bin/orchard-sidebar /usr/local/bin/orchard-sidebar

install-shell: shell
	install -m 755 bin/orchard-shell /usr/local/bin/orchard-shell

install-upgrade: upgrade
	install -m 755 bin/orchard-upgrade /usr/local/bin/orchard-upgrade

# Install shell scripts to /usr/local/share/orchard/scripts so the daemon
# can find them via orchardScriptsRoot() when running from a system install.
install-scripts:
	install -d /usr/local/share/orchard/scripts/git
	install -m 755 scripts/git/*.sh /usr/local/share/orchard/scripts/git/

install-tui: rust
	install -m 755 target/release/orchard-tui /usr/local/bin/orchard-tui

install-worktree-cli: worktree-cli
	install -m 755 target/release/orchard-worktree /usr/local/bin/orchard-worktree

clean:
	rm -rf bin/ target/ dist/

# Cross-platform release assembly: per-binary tarballs, per-triple
# orchard-suite-<triple>.tar.gz, and an aggregate SHA256SUMS under dist/.
# Go binaries always cross-compile; Rust binaries via scripts/dist.sh's
# native -> cargo-zigbuild -> cross -> docker fallback chain (a triple with
# no working method is skipped with a warning, not a failure).
# Single-target build: make dist TRIPLE=aarch64-unknown-linux-gnu
dist:
	VERSION=$(VERSION) bash scripts/dist.sh $(if $(TRIPLE),--only $(TRIPLE),)

test: test-go test-rust

test-go:
	go test ./...

test-rust:
	cargo test

bats-install:
	@command -v bats >/dev/null || (echo "Installing bats..." && brew install bats-core 2>/dev/null || npm install -g bats)

bats-test: bats-install
	bats -r scripts
	bats -r plugins/conversation-contracts

# Feature parity checks — verify scenario↔test annotation coverage.
# See docs/testing-philosophy.md for the zero-tolerance policy.
check-feature-parity: check-feature-parity-daemon check-feature-parity-tui check-feature-parity-gui

check-feature-parity-daemon:
	@bash scripts/check-feature-parity-daemon.sh

check-feature-parity-tui:
	@bash scripts/check-feature-parity-tui.sh

check-feature-parity-gui:
	@bash scripts/check-feature-parity-gui.sh
