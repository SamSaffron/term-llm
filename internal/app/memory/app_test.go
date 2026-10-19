package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenStoreZeroPathsUsesEnvWithoutTouchingDefault(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	defaultPath := DefaultDBPath()
	override := filepath.Join(t.TempDir(), "env", "memory.db")
	t.Setenv("TERM_LLM_MEMORY_DB", override)
	store, err := OpenStore(Paths{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(override); err != nil {
		t.Fatalf("env memory DB was not opened: %v", err)
	}
	if _, err := os.Stat(defaultPath); !os.IsNotExist(err) {
		t.Fatalf("default memory DB was touched: %v", err)
	}
}

func TestPathsResolveNormalizesZeroValuesBeforeEnvOverride(t *testing.T) {
	override := filepath.Join(t.TempDir(), "env", "memory.db")
	t.Setenv("TERM_LLM_MEMORY_DB", override)
	if got := (Paths{}).resolve(); got != override {
		t.Fatalf("(Paths{}).resolve() = %q, want env override %q", got, override)
	}
}

func TestPathsResolveExplicitOverrideBeatsEnv(t *testing.T) {
	boundDefault := filepath.Join(t.TempDir(), "default", "memory.db")
	explicit := filepath.Join(t.TempDir(), "explicit", "memory.db")
	t.Setenv("TERM_LLM_MEMORY_DB", filepath.Join(t.TempDir(), "env.db"))
	if got := (Paths{DBPath: explicit, DefaultDBPath: boundDefault}).resolve(); got != explicit {
		t.Fatalf("explicit Paths.resolve() = %q, want %q", got, explicit)
	}
}

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
