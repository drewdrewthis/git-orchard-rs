#!/usr/bin/env bash
# dist.sh — assembles dist/: per-binary tarballs, per-triple
# orchard-suite-<triple>.tar.gz, and an aggregate SHA256SUMS. Invoked by
# `make dist` (#747 Step 6).
#
# Go binaries cross-compile unconditionally (CGO_ENABLED=0, no toolchain
# dependency). Rust binaries build only for `rustup target`s already
# installed locally -- unlike CI (.github/workflows/release-please.yml),
# which builds every triple on dedicated runners. A triple's suite tarball
# is assembled only when every binary in SUITE_BINS built for it; otherwise
# that triple is skipped with a warning (per-binary tarballs for whichever
# binaries DID build are still written, so a partial local run still
# produces something usable).
#
# GOOS/GOARCH<->triple pairs and the six-binary suite roster mirror
# internal/release/assets.go (`triples`, `SuiteBinaries`) -- the Go upgrade
# client's ground truth for what a release must contain. The dispatcher's
# tarball name/contents (orchard-<triple>.tar.gz, containing one file named
# `orchard`) is unchanged: npm/install.js hardcodes it (plan AC12).
#
# Usage: scripts/dist.sh [VERSION]   (VERSION defaults to "dev")
set -euo pipefail

VERSION="${1:-dev}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"

rm -rf "$DIST"
mkdir -p "$DIST"

# "goos goarch triple" rows -- must match internal/release/assets.go's
# `triples` map.
PLATFORMS=(
  "linux amd64 x86_64-unknown-linux-gnu"
  "linux arm64 aarch64-unknown-linux-gnu"
  "darwin amd64 x86_64-apple-darwin"
  "darwin arm64 aarch64-apple-darwin"
)

GO_BINS=(orchard-daemon orchard-sidebar orchard-shell orchard-upgrade)

installed_rust_targets="$(rustup target list --installed 2>/dev/null || true)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for entry in "${PLATFORMS[@]}"; do
  read -r goos goarch triple <<<"$entry"
  echo "== $triple (GOOS=$goos GOARCH=$goarch) =="

  platform_dir="$work/$triple"
  mkdir -p "$platform_dir"
  have_go=1
  have_rust=1

  # --- Go binaries: always attempted, no toolchain dependency. ---
  for bin in "${GO_BINS[@]}"; do
    src="cmd/$bin"
    if [[ ! -d "$ROOT/$src" ]]; then
      echo "  skip $bin: $src does not exist yet"
      have_go=0
      continue
    fi
    out="$platform_dir/$bin"
    if ! (cd "$ROOT" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
        go build -ldflags "-X main.version=$VERSION" -o "$out" "./$src" 2>&1); then
      echo "  FAILED building $bin for $triple"
      have_go=0
      continue
    fi
    tarball="$DIST/$bin-$triple.tar.gz"
    tar czf "$tarball" -C "$platform_dir" "$bin"
    echo "  wrote $(basename "$tarball")"
  done

  # --- Rust binaries: only for locally installed rustup targets. ---
  if ! grep -qx "$triple" <<<"$installed_rust_targets"; then
    echo "  skip Rust binaries: rustup target $triple not installed (rustup target add $triple)"
    have_rust=0
  else
    # orchard-dispatcher package -> `orchard` binary. Tarball name and the
    # single member inside it (`orchard`) are unchanged from today.
    if (cd "$ROOT" && cargo build --release --target "$triple" -p orchard-dispatcher) \
        && cp "$ROOT/target/$triple/release/orchard" "$platform_dir/orchard"; then
      tar czf "$DIST/orchard-$triple.tar.gz" -C "$platform_dir" orchard
      echo "  wrote orchard-$triple.tar.gz"
    else
      echo "  FAILED building orchard-dispatcher for $triple"
      have_rust=0
    fi
    # orchard package -> `orchard-tui` binary.
    if (cd "$ROOT" && cargo build --release --target "$triple" -p orchard) \
        && cp "$ROOT/target/$triple/release/orchard-tui" "$platform_dir/orchard-tui"; then
      tar czf "$DIST/orchard-tui-$triple.tar.gz" -C "$platform_dir" orchard-tui
      echo "  wrote orchard-tui-$triple.tar.gz"
    else
      echo "  FAILED building orchard for $triple"
      have_rust=0
    fi
  fi

  # --- Suite tarball: only when every binary built for this triple. ---
  if [[ "$have_go" -eq 1 && "$have_rust" -eq 1 ]]; then
    suite_dir="$work/suite-$triple"
    mkdir -p "$suite_dir"
    for bin in "${GO_BINS[@]}" orchard-tui orchard; do
      cp "$platform_dir/$bin" "$suite_dir/$bin"
    done
    tar czf "$DIST/orchard-suite-$triple.tar.gz" -C "$suite_dir" .
    echo "  wrote orchard-suite-$triple.tar.gz"
  else
    echo "  WARNING: skipping orchard-suite-$triple.tar.gz (missing Go and/or Rust binaries for $triple)"
  fi
done

# --- Aggregate checksums over everything produced. ---
if compgen -G "$DIST"/*.tar.gz >/dev/null; then
  (
    cd "$DIST"
    if command -v shasum >/dev/null 2>&1; then
      shasum -a 256 -- *.tar.gz > SHA256SUMS
    else
      sha256sum -- *.tar.gz > SHA256SUMS
    fi
  )
  echo "wrote SHA256SUMS ($(wc -l < "$DIST/SHA256SUMS" | tr -d ' ') entries)"
else
  echo "no tarballs produced; skipping SHA256SUMS"
fi
