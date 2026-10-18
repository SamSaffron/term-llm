package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var cacheIdentityMu sync.Mutex

// CacheScopeFor derives a non-secret browser cache scope from a random state
// identity and the effective auth mode. An empty identity disables caching.
func CacheScopeFor(identity, authMode string) string {
	if identity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("term-llm-hub-cache-scope/v1\x00" + identity + "\x00" + authMode))
	return hex.EncodeToString(sum[:12])
}

// A small metadata sidecar belongs to the Hub's node state, independently of
// node edits. Unavailable or corrupt state disables browser persistence. The
// exclusive create prevents a second process from replacing the identity.
func CacheIdentity(path string) string {
	cacheIdentityMu.Lock()
	defer cacheIdentityMu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		id, state := readCacheIdentity(path)
		switch state {
		case identityValid:
			return id
		case identityCorrupt:
			// A damaged sidecar only rotates the browser cache scope; heal it once.
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return ""
			}
		case identityUnreadable:
			return ""
		}
		if id, created := createCacheIdentity(path); created {
			return id
		}
		// Another process won the exclusive create: read its identity.
	}
	return ""
}

type identityState int

const (
	identityMissing identityState = iota
	identityValid
	identityCorrupt
	identityUnreadable
)

func readCacheIdentity(path string) (string, identityState) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", identityMissing
	}
	if err != nil {
		return "", identityUnreadable
	}
	id := strings.TrimSpace(string(data))
	if decoded, err := hex.DecodeString(id); err == nil && len(decoded) == 32 {
		return id, identityValid
	}
	return "", identityCorrupt
}

func createCacheIdentity(path string) (string, bool) {
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
