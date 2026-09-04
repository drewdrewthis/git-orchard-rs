#!/usr/bin/env bash
set -euo pipefail

# Verify every suite tarball's binaries were built from ONE clean commit.
# The canonical Go stampable set (orchard-daemon orchard-sidebar orchard-shell
# orchard-upgrade) and the --revision-answering set (RevisionBinaries, incl.
# orchard-tui, i.e. SuiteBinaries minus the "orchard" dispatcher) both live in
# internal/release/revision.go + assets.go -- keep this list in sync with them.
# Guards #817: untracked build outputs made buildvcs stamp binaries "+dirty".
GO_BINS="orchard-daemon orchard-sidebar orchard-shell orchard-upgrade"
REVISION_BINS="orchard-daemon orchard-sidebar orchard-shell orchard-upgrade orchard-tui"

[ "$#" -ge 1 ] || { echo "usage: $0 <suite-tarball>..." >&2; exit 2; }

# Host triple (mirrors internal/release triples map): only run binaries whose
# tarball matches, since a foreign GOOS/GOARCH cannot execute here.
case "$(go env GOOS)/$(go env GOARCH)" in
  darwin/amd64) HOST_TRIPLE=x86_64-apple-darwin ;;
  darwin/arm64) HOST_TRIPLE=aarch64-apple-darwin ;;
  linux/amd64)  HOST_TRIPLE=x86_64-unknown-linux-gnu ;;
  linux/arm64)  HOST_TRIPLE=aarch64-unknown-linux-gnu ;;
  *)            HOST_TRIPLE="" ;;
esac

fail=0
expected_rev=""
for tarball in "$@"; do
  echo "== $tarball =="
  work="$(mktemp -d)"
  trap 'rm -rf "$work"' EXIT
  tar xzf "$tarball" -C "$work"
  printf '  %-18s %-14s %s\n' BINARY VCS.MODIFIED REVISION
  # Seed with the revision established by prior tarballs so a mismatch
  # ACROSS tarballs fails too, not just within one (#817 skew case).
  seen_rev="$expected_rev"
  for bin in $GO_BINS; do
    path="$work/$bin"
    [ -f "$path" ] || continue
    meta="$(go version -m "$path")"
    rev="$(printf '%s' "$meta" | grep -oE 'internal/release\.revision=[^ "]+' | head -1 | cut -d= -f2- || true)"
    modified="$(printf '%s' "$meta" | grep -oE 'vcs\.modified=(true|false)' | head -1 | cut -d= -f2- || true)"
    printf '  %-18s %-14s %s\n' "$bin" "${modified:-<none>}" "${rev:-<none>}"
    if [ -z "$rev" ]; then echo "  FAIL $bin: no release.revision ldflag" >&2; fail=1; fi
    case "$rev" in *+dirty*) echo "  FAIL $bin: revision is +dirty" >&2; fail=1 ;; esac
    if [ "$modified" = "true" ]; then echo "  FAIL $bin: vcs.modified=true" >&2; fail=1; fi
    if [ -n "$rev" ] && [ -n "$seen_rev" ] && [ "$rev" != "$seen_rev" ]; then
      echo "  FAIL $bin: revision $rev differs from $seen_rev" >&2; fail=1
    fi
    [ -n "$rev" ] && seen_rev="$rev"
  done
  [ -z "$expected_rev" ] && expected_rev="$seen_rev"

  # If this tarball is for the host platform, execute --revision too.
  if [ -n "$HOST_TRIPLE" ] && printf '%s' "$tarball" | grep -q "$HOST_TRIPLE"; then
    run_rev=""
    for bin in $REVISION_BINS; do
      path="$work/$bin"
      [ -f "$path" ] || continue
      out="$("$path" --revision 2>/dev/null || true)"
      echo "  --revision $bin: ${out:-<none>}"
      case "$out" in *+dirty*|"") echo "  FAIL $bin: --revision empty or +dirty" >&2; fail=1 ;; esac
      if [ -n "$out" ] && [ -n "$run_rev" ] && [ "$out" != "$run_rev" ]; then
        echo "  FAIL $bin: --revision $out differs from $run_rev" >&2; fail=1
      fi
      [ -n "$out" ] && run_rev="$out"
    done
  else
    # Foreign triple: can't exec these binaries here, so --revision is
    # unverified. Say so explicitly -- a bare PASSED must never hide this.
    triple="$(basename "$tarball" | grep -oE '(x86_64|aarch64)-(apple-darwin|unknown-linux-gnu)' | head -1)"
    for bin in $REVISION_BINS; do
      [ -f "$work/$bin" ] || continue
      echo "  SKIP --revision $bin (${triple:-non-host} not host arch; go version -m covers Go bins only)"
    done
  fi

  rm -rf "$work"; trap - EXIT
done

if [ "$fail" -ne 0 ]; then
  echo "revision check FAILED" >&2
  exit 1
fi
echo "revision check PASSED"
