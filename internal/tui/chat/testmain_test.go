package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestMain(m *testing.M) {
	// Project resolution canonicalizes symlinks. On macOS, keep temporary
	// project fixtures in /private/var rather than its /var alias.
	if tempRoot, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if err := os.Setenv("TMPDIR", filepath.Clean(tempRoot)); err != nil {
			fmt.Fprintf(os.Stderr, "set canonical TMPDIR: %v\n", err)
			os.Exit(1)
		}
	}
	dataHome, err := os.MkdirTemp("", "term-llm-chat-test-data-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("XDG_DATA_HOME", dataHome); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_DATA_HOME: %v\n", err)
		os.Exit(1)
	}
	// Provider pickers list only configured providers; keep that independent
	// of the developer's API keys and signed-in CLIs.
	for _, spec := range config.BuiltinProviders() {
		for _, v := range spec.EnablingEnv() {
			_ = os.Unsetenv(v)
		}
	}
	providerHasLocalCredentials = func(string) bool { return false }
	code := m.Run()
	_ = os.RemoveAll(dataHome)
	os.Exit(code)
}
