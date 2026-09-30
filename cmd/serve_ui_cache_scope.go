package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

// uiCacheScope returns an opaque identity for browser-persisted UI caches.
//
// It changes when the session database is replaced (store_instance_id is a
// random per-database value) or the auth mode changes, so a browser never
// shows one store's or one auth domain's cached conversations for another.
// It is derived only from non-secret values: credentials never participate.
// An empty result disables private persistence in the web UI. final is false
// when the lookup failed transiently, so callers must not cache the result.
func (s *serveServer) uiCacheScope() (scope string, final bool) {
	attention, ok := session.AsAttentionStore(s.store)
	if !ok {
		return "", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	storeID, err := attention.StoreInstanceID(ctx)
	if err != nil || storeID == "" {
		return "", false
	}
	return uiCacheScopeFor(storeID, s.uiAuthMode()), true
}

func uiCacheScopeFor(storeID, authMode string) string {
	sum := sha256.Sum256([]byte("term-llm-ui-cache-scope/v1\x00" + storeID + "\x00" + authMode))
	return hex.EncodeToString(sum[:12])
}

func (s *serveServer) uiAuthMode() string {
	switch {
	case s.browserAuth != nil:
		return "passkey"
	case s.cfg.requireAuth:
		return "bearer"
	default:
		return "none"
	}
}

// uiShellAuthorized reports whether serving the UI HTML already implies the
// viewer may read the API: passkey mode gates the whole handler, and servers
// without auth grant access to anyone who can reach them. Bearer-token
// servers serve public HTML, so cached private content must wait for an
// authenticated API response there.
func (s *serveServer) uiShellAuthorized() bool {
	return s.uiAuthMode() != "bearer"
}
