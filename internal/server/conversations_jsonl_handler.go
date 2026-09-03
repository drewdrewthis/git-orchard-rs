package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PathLookup is the narrow interface the handler uses to resolve a
// sessionUuid to an on-disk path. *claudeprojects.Provider satisfies
// this interface via its PathForSessionUUID method; tests inject stubs.
type PathLookup interface {
	// PathForSessionUUID returns the absolute on-disk path for the
	// conversation identified by uuid. Returns ("", false) when the
	// uuid is not known.
	PathForSessionUUID(ctx context.Context, uuid string) (string, bool)
}

// NewConversationsJSONLHandler returns an http.Handler serving
//
//	GET /v1/conversations/:sessionUuid/jsonl
//
// The handler streams the file via http.ServeContent so Range,
// If-None-Match, and Last-Modified are handled by the standard library.
// A weak ETag (format W/"<size>-<mtime-ns>") is computed from os.Stat
// and set before handing off to ServeContent so the stdlib conditional
// logic applies it.
//
// root is the configured projects root. It is canonicalised per request
// via filepath.EvalSymlinks — not once at construction — because the root
// may not exist when the daemon boots and may later appear as a symlink;
// a root pinned at boot would then disagree with every symlink-resolved
// candidate and 404 the whole endpoint (#513). Any path returned by the
// PathLookup is validated to be a descendant of that canonical root using
// filepath.Rel — the classic HasPrefix-bypass bug is avoided by design.
//
// Errors:
//   - 405 for any method other than GET or HEAD.
//   - 404 when the sessionUuid is unknown to lookup.
//   - 404 when the resolved path is not a descendant of the projects root.
//   - 404 when the file is not found on disk (fs.ErrNotExist).
//   - 500 for any other OS error.
func NewConversationsJSONLHandler(lookup PathLookup, root string, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}

	// Resolve once at construction for the boot-time diagnostic only; the
	// value each request validates against is recomputed in cleanRoot().
	if _, err := resolveRoot(root); err != nil {
		logger.Warn("conversations jsonl: projects root does not resolve yet, will retry per request",
			"root", root, "err", err)
	}

	return &conversationsJSONLHandler{
		lookup: lookup,
		root:   root,
		logger: logger,
	}
}

type conversationsJSONLHandler struct {
	lookup PathLookup
	root   string
	logger *slog.Logger
}

// resolveRoot returns the canonical, symlink-resolved form of root.
// Fails when the root does not exist yet or is a dangling symlink.
func resolveRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		// filepath.Abs only fails when os.Getwd fails — extremely unusual.
		return "", fmt.Errorf("filepath.Abs(%q): %w", root, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("EvalSymlinks(%q): %w", abs, err)
	}
	return resolved, nil
}

// cleanRoot returns the canonical root this request validates against.
//
// Resolved per request rather than cached at construction: the daemon must
// boot before ~/.claude/projects exists (fresh install), and that directory
// may later appear as a symlink to storage elsewhere. A root pinned at boot
// stays unresolved while every candidate is symlink-resolved, so
// filepath.Rel reports every legitimate transcript as outside the root and
// the endpoint 404s until restart (#513).
//
// EvalSymlinks costs one lstat per path component and the endpoint goes on
// to stream a multi-MB transcript, so the per-request cost is noise.
//
// When the root still does not resolve we fall back to Clean(Abs) — the
// same fresh-install tolerance the constructor had. Logged at Debug, not
// Warn: the constructor already warned once, and this runs per request.
func (h *conversationsJSONLHandler) cleanRoot() string {
	resolved, err := resolveRoot(h.root)
	if err == nil {
		return resolved
	}
	h.logger.Debug("conversations jsonl: projects root does not resolve, falling back to Clean(Abs)",
		"root", h.root, "err", err)
	abs, absErr := filepath.Abs(h.root)
	if absErr != nil {
		return filepath.Clean(h.root)
	}
	return filepath.Clean(abs)
}

// validatePath checks that candidate resolves to a path that is a descendant
// of cleanRoot. It resolves symlinks on the candidate path (belt-and-braces:
// a symlink inside the root pointing outside is still rejected) and uses
// filepath.Rel to avoid the HasPrefix-bypass bug.
//
// Returns the symlink-resolved path on success, or a non-nil error (which the
// caller maps to 404 — the exact reason is intentionally not exposed to HTTP
// clients). The caller MUST open the returned resolved path, not the input
// candidate, to avoid a TOCTOU window where a symlink is rewritten between
// EvalSymlinks and os.Open.
func validatePath(cleanRoot, candidate string) (string, error) {
	// Step 1: clean without symlink resolution first, so we can use the
	// cleaned path as input to EvalSymlinks.
	cleaned := filepath.Clean(candidate)

	// Step 2: resolve symlinks so a symlink inside root that points outside
	// is caught.
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		// File doesn't exist, is a dangling symlink, or permission denied.
		// Treat as a validation failure (caller will surface as 404).
		return "", fmt.Errorf("EvalSymlinks(%q): %w", cleaned, err)
	}

	// Step 3: use filepath.Rel — NOT strings.HasPrefix — to check descent.
	// HasPrefix has the classic bypass bug: /foo/bar HasPrefix /foo is true,
	// but /foobar HasPrefix /foo is also true if you use string prefix alone.
	// filepath.Rel("/a/b", "/a/bc") → "../bc" (starts with ".."), which we reject.
	rel, err := filepath.Rel(cleanRoot, resolved)
	if err != nil {
		return "", fmt.Errorf("filepath.Rel(%q, %q): %w", cleanRoot, resolved, err)
	}
	// rel starts with ".." when resolved is not under cleanRoot.
	// rel == ".." is the exact-parent case, also rejected.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is not under root %q (rel=%q)", resolved, cleanRoot, rel)
	}
	return resolved, nil
}

func (h *conversationsJSONLHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only GET and HEAD are supported. http.ServeContent handles HEAD
	// transparently, so we only need to reject other methods here.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse :sessionUuid from the URL path.
	// Expected form: /v1/conversations/<uuid>/jsonl
	uuid, ok := parseSessionUUID(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Resolve uuid → filesystem path via the provider.
	path, ok := h.lookup.PathForSessionUUID(r.Context(), uuid)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Belt-and-braces: validate that the resolved path is a descendant of
	// the configured root. This guards against a misbehaving provider and
	// against symlinks that point outside the root. Uses filepath.Rel, not
	// strings.HasPrefix, to avoid the classic prefix-bypass bug. Returns
	// the symlink-resolved path so we open exactly what we validated —
	// closes the TOCTOU window where a symlink could be rewritten between
	// EvalSymlinks and os.Open.
	cleanRoot := h.cleanRoot()
	resolved, err := validatePath(cleanRoot, path)
	if err != nil {
		h.logger.Warn("conversations jsonl: path validation rejected candidate",
			"cleanRoot", cleanRoot, "candidate", path, "err", err)
		http.NotFound(w, r)
		return
	}

	// Open the symlink-resolved path. Map fs.ErrNotExist → 404; everything else → 500.
	f, err := os.Open(resolved) //nolint:gosec // resolved is validated against cleanRoot above
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		h.logger.Error("conversations jsonl: open failed", "path", path, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	// Stat for size + mtime — both needed for ETag and ServeContent.
	stat, err := f.Stat()
	if err != nil {
		h.logger.Error("conversations jsonl: stat failed", "path", path, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Compute a weak ETag from size and mtime nanoseconds. A weak ETag
	// is appropriate here: two responses with the same size+mtime are
	// semantically equivalent even if we can't guarantee byte-level
	// identity (e.g. if the OS rounds mtime). The format mirrors what
	// many file servers use: W/"<size>-<mtime-ns>".
	etag := fmt.Sprintf(`W/"%d-%d"`, stat.Size(), stat.ModTime().UnixNano())

	// Set Content-Type and ETag BEFORE calling ServeContent. The stdlib
	// will not override a pre-set Content-Type; setting ETag here lets
	// ServeContent use it for If-None-Match conditional GET evaluation.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("ETag", etag)

	// Optional `?lastN=<int>` mode — return only the last N newline-
	// terminated records. Lets the GUI render an instant transcript from
	// a multi-MB JSONL without pulling the whole thing. Disables Range/
	// 304 caching (the server-side computed slice is dynamic).
	if lastN, ok := parseLastN(r.URL.Query().Get("lastN")); ok {
		serveLastN(w, f, stat, lastN, h.logger)
		return
	}

	// ServeContent handles Range, If-Modified-Since, If-None-Match,
	// Last-Modified, Content-Length, and 206/304/416 status codes.
	// The empty name ("") prevents the stdlib from sniffing or
	// overriding the Content-Type we set above.
	http.ServeContent(w, r, "", stat.ModTime(), f)
}

// parseLastN parses the ?lastN= query parameter. Returns (n, true) when n
// is a positive int <= maxLastN; otherwise (0, false) so the caller falls
// back to full-file streaming. Capped to keep a single request from
// allocating an unbounded buffer.
const maxLastN = 5000

func parseLastN(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	if n > maxLastN {
		n = maxLastN
	}
	return n, true
}

// serveLastN reads the file's tail looking for the start of the last N
// newline-terminated records, then streams from that offset to EOF.
//
// Strategy: read the file backwards in 64KB chunks, counting newlines.
// Stop when N+1 newlines have been seen (the +1 anchors us to the start
// of the Nth-from-last record), or when we hit BOF (file has fewer than
// N records — serve the whole thing). Worst case: short transcripts
// land in the first chunk; long ones touch only the tail bytes needed
// to find N record boundaries.
func serveLastN(w http.ResponseWriter, f *os.File, stat os.FileInfo, n int, logger *slog.Logger) {
	const chunk = 64 * 1024
	size := stat.Size()
	if size == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Walk backwards counting newlines until we've found n+1 of them or
	// reached the start of the file. The +1 ensures we land at a record
	// boundary (skip the trailing newline of the (N+1)th-from-last
	// record) rather than mid-record.
	buf := make([]byte, chunk)
	target := n + 1
	seen := 0
	pos := size
	startOffset := int64(0)
	for pos > 0 {
		readSize := int64(chunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		if _, err := f.ReadAt(buf[:readSize], pos); err != nil {
			logger.Error("conversations jsonl: tail read failed", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		// Walk this chunk backwards counting newlines.
		for i := int(readSize) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				seen++
				if seen == target {
					// One past this newline is the start of the last N records.
					startOffset = pos + int64(i) + 1
					goto found
				}
			}
		}
	}
	// Hit BOF without seeing target newlines — file has fewer than N
	// records. Serve from offset 0.
	startOffset = 0

found:
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		logger.Error("conversations jsonl: seek failed", "offset", startOffset, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Orchard-LastN", strconv.Itoa(n))
	w.Header().Set("X-Orchard-StartOffset", strconv.FormatInt(startOffset, 10))
	if _, err := io.Copy(w, f); err != nil {
		// Client disconnect / write error — log at debug, don't try to
		// send a status code (headers already flushed).
		logger.Debug("conversations jsonl: tail copy aborted", "err", err)
	}
}

// parseSessionUUID extracts the :sessionUuid segment from a path of
// the form /v1/conversations/<uuid>/jsonl. Returns ("", false) when
// the path does not match the expected pattern.
//
// The uuid is treated as an opaque lookup key — it is never joined
// onto the filesystem. Path-traversal characters in the uuid segment
// are harmless because they are only passed to the PathLookup interface
// (which returns a path from its own cache), never used to construct a
// file path directly.
func parseSessionUUID(urlPath string) (string, bool) {
	const prefix = "/v1/conversations/"
	const suffix = "/jsonl"

	rest, ok := strings.CutPrefix(urlPath, prefix)
	if !ok {
		return "", false
	}
	uuid, ok := strings.CutSuffix(rest, suffix)
	if !ok {
		return "", false
	}
	if uuid == "" {
		return "", false
	}
	return uuid, true
}
