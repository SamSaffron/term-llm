package usage

import (
	"fmt"
	"os"
	"testing"
)

// Pricing tests assert concrete rates, so they must never read the cached model
// directory a developer's machine happens to hold.
func TestMain(m *testing.M) {
	cacheHome, err := os.MkdirTemp("", "term-llm-usage-test-cache-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp XDG_CACHE_HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_CACHE_HOME", cacheHome); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_CACHE_HOME: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(cacheHome)
	os.Exit(code)
}
