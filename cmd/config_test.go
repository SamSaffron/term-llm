package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfigSet_AtomicWriteFailureLeavesExistingConfigUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions behave differently on Windows")
	}

	xdgConfigHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdgConfigHome)

	configDir := filepath.Join(xdgConfigHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}

	configPath := filepath.Join(configDir, "config.yaml")
	original := "default_provider: anthropic\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatalf("write original config: %v", err)
	}

	if err := os.Chmod(configDir, 0o555); err != nil {
		t.Fatalf("chmod config dir read-only: %v", err)
	}
	defer func() {
		if err := os.Chmod(configDir, 0o755); err != nil {
			t.Fatalf("restore config dir permissions: %v", err)
		}
	}()

	err := configSet(nil, []string{"default_provider", "openai"})
	if err == nil {
		t.Fatal("expected configSet to fail when atomic temp-file creation is blocked")
	}
	if !strings.Contains(err.Error(), "create temp file") {
		t.Fatalf("error = %v, want create temp file", err)
	}

	got, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatalf("read original config: %v", readErr)
	}
	if string(got) != original {
		t.Fatalf("config changed on failed write: got %q want %q", got, original)
	}
}
