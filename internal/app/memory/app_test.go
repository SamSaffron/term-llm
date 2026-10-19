package memory

import (
	"path/filepath"
	"testing"
)

// Cobra captures the default before test setup (or a process-wide XDG change).
// ResolveDBPath must compare against that captured value, not recompute it.
func TestResolveDBPathUsesBoundDefault(t *testing.T) {
	boundDefault := filepath.Join(t.TempDir(), "at-binding", "memory.db")
	override := filepath.Join(t.TempDir(), "override.db")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("TERM_LLM_MEMORY_DB", override)
	if got := ResolveDBPath(boundDefault, boundDefault); got != override {
		t.Fatalf("bound default path resolved to %q, want %q", got, override)
	}
	custom := filepath.Join(t.TempDir(), "custom.db")
	if got := ResolveDBPath(custom, boundDefault); got != custom {
		t.Fatalf("explicit --db path resolved to %q, want %q", got, custom)
	}
}
