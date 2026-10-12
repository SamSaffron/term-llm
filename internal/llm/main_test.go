package llm

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the model-catalog cache at a private directory so package
// tests never read (or write) the developer's real live-catalog caches, which
// several model-metadata lookups consult before curated fallbacks.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "term-llm-llm-test-cache-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("XDG_CACHE_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
