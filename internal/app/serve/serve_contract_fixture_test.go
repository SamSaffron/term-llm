package serve

import (
	"os"
	"path/filepath"
	"testing"
)

func writeServeContractConfig(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	for key, subdir := range map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache"} {
		t.Setenv(key, filepath.Join(home, subdir))
	}
	t.Setenv("TERM_LLM_SERVE_TOKEN", "")
	t.Setenv("TERM_LLM_SERVE_PUBLIC_URL", "")
	dir := filepath.Join(home, "config", "term-llm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
