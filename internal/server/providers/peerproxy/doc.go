// Package peerproxy implements the federation provider — the layer that
// turns a remote orchard daemon into a backend just like git, tmux or
// ps. Per ADR-011 §7, federation is not a special protocol: the local
// daemon talks GraphQL to the remote daemon and the resolvers cannot
// tell the difference between a local provider and the proxy.
//
// Layering (see ADR-011 §2):
//
//	provider.go — Provider[NodeID, Node]; resolver-facing surface,
//	              fans out to per-peer adapters.
//	adapter.go  — one Adapter per peer; owns its Subscribe loop and
//	              fans the watcher channel out as InvalidationEvent.
//	client.go   — websocket + HTTP transport; speaks the
//	              graphql-transport-ws subprotocol for subscriptions
//	              and POSTs JSON for one-shot queries. HTTPS/WSS
//	              enabled per-peer via `tls: true` in config.
//	keepalive.go — read-deadline policy for the peer websocket: the
//	              silence budget and the control-frame handlers that
//	              re-arm it, so a half-open socket errors out into the
//	              reconnect path instead of parking a read forever.
//	config.go   — loads peer addresses from
//	              ~/.orchard/config.json. Read-only.
//
// Auth: peer authentication is delegated to the transport. For TLS-enabled
// peers (e.g. boxd-fronted endpoints), the transport-level allowlist on
// the boxd subdomain is the security boundary. For plaintext peers, the
// LAN itself is the boundary. The daemon does not implement an
// application-level bearer-secret guard — that approach was removed in
// issue #412 because the operational complexity (CLI auth lockout,
// fsnotify reload edge cases) outweighed the security gain over TLS.
//
// Failure model: the adapter never silently swallows errors. A failed
// websocket open marks the peer unreachable, the next dashboard query
// reflects `reachable: false`, and Subscribe() retries on a coarse
// backoff. Cross-host node lookups bubble the underlying network error
// to the resolver.
package peerproxy
