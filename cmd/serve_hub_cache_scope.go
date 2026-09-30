package cmd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var hubCacheIdentityMu sync.Mutex

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
	if identity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("term-llm-hub-cache-scope/v1\x00" + identity + "\x00" + authMode))
	return hex.EncodeToString(sum[:12])
}

// A small metadata sidecar belongs to the Hub's node state, independently of
// node edits. Unavailable or corrupt state disables browser persistence. The
// exclusive create prevents a second process from replacing the identity.
func hubCacheIdentity(path string) string {
	hubCacheIdentityMu.Lock()
	defer hubCacheIdentityMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		id, state := readHubCacheIdentity(path)
		switch state {
		case hubIdentityValid:
			return id
		case hubIdentityCorrupt:
			// A damaged sidecar only rotates the browser cache scope; heal it once.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return ""
			}
		case hubIdentityUnreadable:
			return ""
		}
		if id, created := createHubCacheIdentity(path); created {
			return id
		}
		// Another process won the exclusive create: read its identity.
	}
	return ""
}

type hubIdentityState int

const (
	hubIdentityMissing hubIdentityState = iota
	hubIdentityValid
	hubIdentityCorrupt
	hubIdentityUnreadable
)

func readHubCacheIdentity(path string) (string, hubIdentityState) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", hubIdentityMissing
	}
	if err != nil {
		return "", hubIdentityUnreadable
	}
	id := strings.TrimSpace(string(data))
	if decoded, err := hex.DecodeString(id); err == nil && len(decoded) == 32 {
		return id, hubIdentityValid
	}
	return "", hubIdentityCorrupt
}

func createHubCacheIdentity(path string) (string, bool) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", false
	}
	id := hex.EncodeToString(random[:])
	// Write a complete temp file, then publish it with an exclusive hard link so
	// a concurrent reader never observes (and "heals") a half-written identity.
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", false
	}
	temp := file.Name()
	defer os.Remove(temp)
	_, writeErr := file.WriteString(id + "\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return "", false
	}
	if err := os.Link(temp, path); err != nil {
		return "", false
	}
	return id, true
}
