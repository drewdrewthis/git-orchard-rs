#!/usr/bin/env python3
"""Minimal POST-only HTTP server replaying a canned GraphQL response body.

Stands in for the orchard daemon at the network boundary, so pane-labels.sh
exercises its real curl call and its real JSON parse instead of having those
stubbed out. This is a fake wire peer, not a double for anything this repo
owns.

Usage: fake-daemon.py <response.json> <port-file>

Binds 127.0.0.1 on an ephemeral port and writes the chosen port to
<port-file> once listening, so the caller can poll for readiness.
"""

import http.server
import sys


class Handler(http.server.BaseHTTPRequestHandler):
    """Answers any POST with the canned body; everything else is 405."""

    def do_POST(self):  # noqa: N802 - name fixed by BaseHTTPRequestHandler
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(BODY)))
        self.end_headers()
        self.wfile.write(BODY)

    def log_message(self, *args):
        """Silence the default stderr access log — bats captures stderr."""


if __name__ == "__main__":
    with open(sys.argv[1], "rb") as fh:
        BODY = fh.read()
    server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    with open(sys.argv[2], "w") as fh:
        fh.write(str(server.server_port))
    server.serve_forever()
