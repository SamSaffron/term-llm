package cmd

import (
	"context"
	"strings"
)

// serveSessionNamespaceKeyType keys the per-client session namespace stored on a
// request context. The namespace isolates all session IDs and response-chaining
// state when several independently-authenticated clients share one serveServer
// (the capability proxy). It is empty for ordinary serve mode, which makes every
// helper below a no-op there so existing single-tenant behavior is unchanged.
type serveSessionNamespaceKeyType struct{}

var serveSessionNamespaceKey serveSessionNamespaceKeyType

// withServeSessionNamespace returns a context scoped to ns. An empty ns leaves
// the context unchanged.
func withServeSessionNamespace(ctx context.Context, ns string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(ns) == "" {
		return ctx
	}
	return context.WithValue(ctx, serveSessionNamespaceKey, ns)
}

// serveSessionNamespace returns the namespace bound to ctx, or "" when none is
// set (ordinary serve mode).
func serveSessionNamespace(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	ns, _ := ctx.Value(serveSessionNamespaceKey).(string)
	return ns
}

// namespaceSessionID prefixes a raw session ID with ns so different clients can
// never address the same session/runtime. It is idempotent: a value already in
// the namespace is returned unchanged so IDs handed back to a client (via the
// x-session-id header) round-trip correctly on the next request. An empty ns or
// empty sessionID is returned unchanged.
func namespaceSessionID(ns, sessionID string) string {
	if ns == "" || sessionID == "" {
		return sessionID
	}
	if strings.HasPrefix(sessionID, ns) {
		return sessionID
	}
	return ns + sessionID
}

// sessionIDInNamespace reports whether sessionID is owned by ns. It is the
// authority for cross-client isolation checks: a session/response resolved from
// a shared server-side map must belong to the caller's namespace before it may
// be used. An empty ns (ordinary serve mode) owns everything.
func sessionIDInNamespace(ns, sessionID string) bool {
	if ns == "" {
		return true
	}
	return strings.HasPrefix(sessionID, ns)
}

// proxySessionNamespace builds the opaque namespace prefix for a proxy client.
// The clientID is the caller's own identifier, so embedding it in session IDs
// leaks nothing across clients while guaranteeing per-client separation. The
// double underscore terminator keeps the prefix sanitize-safe and unambiguous.
func proxySessionNamespace(clientID string) string {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return ""
	}
	return "pxc_" + clientID + "__"
}
