package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeCursorListedModelID(t *testing.T) {
	tests := map[string]string{
		"auto":                     "auto-smart",
		"cursor-grok-4.5-high":     "grok-4.5-high",
		"cursor-grok-4.5-low-fast": "grok-4.5-low-fast",
		"composer-2.5":             "composer-2.5",
		"gpt-5.6-sol-high":         "gpt-5.6-sol-high",
	}
	for input, want := range tests {
		if got := normalizeCursorListedModelID(input); got != want {
			t.Fatalf("normalizeCursorListedModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProviderModelIDsPrefersCursorBinCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	curated := ProviderModelIDs("cursor-bin")
	if len(curated) == 0 || !containsModelID(curated, "auto-smart") {
		t.Fatalf("expected curated cursor-bin fallback, got %v", curated)
	}

	RefreshCursorBinCacheSync([]ModelInfo{
		{ID: "auto", DisplayName: "Auto (default)"},
		{ID: "cursor-grok-4.5-high", DisplayName: "Cursor Grok 4.5"},
		{ID: "composer-2.5-fast", DisplayName: "Composer 2.5 Fast"},
		{ID: "gpt-5.6-sol-xhigh-fast", DisplayName: "GPT-5.6 Sol Extra High Fast"},
	})

	ids := ProviderModelIDs("cursor-bin")
	want := []string{"auto-smart", "grok-4.5-high", "composer-2.5-fast", "gpt-5.6-sol-xhigh-fast"}
	if !equalSlice(ids, want) {
		t.Fatalf("ProviderModelIDs(cursor-bin) = %v, want %v", ids, want)
	}
	if containsModelID(ids, "claude-sonnet-5") {
		t.Fatalf("cached cursor-bin list unexpectedly kept curated-only model: %v", ids)
	}
}

func TestCursorBinListModelsNormalizesAndCaches(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "models" ]; then
  cat <<'EOF'
Available models

auto - Auto (default)
cursor-grok-4.5-high - Cursor Grok 4.5
composer-2.5-fast - Composer 2.5 Fast
EOF
  exit 0
fi
echo "unexpected args: $*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "cursor-agent"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	models, err := NewCursorBinProvider("", nil).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"auto-smart", "grok-4.5-high", "composer-2.5-fast"}
	got := make([]string, len(models))
	for i, m := range models {
		got[i] = m.ID
	}
	if !equalSlice(got, want) {
		t.Fatalf("ListModels IDs = %v, want %v", got, want)
	}
	if cached := GetCachedCursorBinModels(); !equalSlice(cached, want) {
		t.Fatalf("cached IDs = %v, want %v", cached, want)
	}
	if picker := ProviderModelIDs("cursor-bin"); !equalSlice(picker, want) {
		t.Fatalf("ProviderModelIDs = %v, want %v", picker, want)
	}
}
