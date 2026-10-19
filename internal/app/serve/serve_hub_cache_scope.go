package serve

import apphub "github.com/samsaffron/term-llm/internal/app/serve/hub"

// hubCacheScope uses only a random state identity and the effective auth mode,
// never a token, passkey credential, cookie, or other authentication secret.
func (s *hubServer) hubCacheScope(authMode string) string {
	var identity string
	if s.passkey != nil && s.passkey.store != nil {
		// The WebAuthn user handle is a random, non-secret, persisted user ID.
		identity = s.passkey.store.User().ID
	} else if s.store != nil && s.store.Path() != "" {
		identity = hubCacheIdentity(s.store.Path() + ".cache-identity")
	}
	return hubCacheScopeFor(identity, authMode)
}

func hubCacheScopeFor(identity, authMode string) string {
	return apphub.CacheScopeFor(identity, authMode)
}

func hubCacheIdentity(path string) string { return apphub.CacheIdentity(path) }
