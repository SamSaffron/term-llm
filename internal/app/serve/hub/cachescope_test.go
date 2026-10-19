package hub

import (
	"path/filepath"
	"testing"
)

func TestCacheScopeAndIdentity(t *testing.T) {
	if CacheScopeFor("", "none") != "" || len(CacheScopeFor("id", "none")) != 24 || CacheScopeFor("id", "none") == CacheScopeFor("id", "bearer") {
		t.Fatal("unexpected cache scope")
	}
	path := filepath.Join(t.TempDir(), "id")
	id := CacheIdentity(path)
	if len(id) != 64 || id != CacheIdentity(path) {
		t.Fatalf("identity not stable: %q", id)
	}
}
