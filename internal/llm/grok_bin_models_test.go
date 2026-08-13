package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRefreshGrokBinModelsIfStale(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	original := grokModelsCommandOutput
	t.Cleanup(func() { grokModelsCommandOutput = original })

	calls := 0
	grokModelsCommandOutput = func(context.Context, *GrokBinProvider, string) ([]byte, error) {
		calls++
		return []byte("Default model: grok-4.6\nAvailable models:\n  * grok-4.6 (default)\n"), nil
	}

	if err := RefreshGrokBinModelsIfStale(context.Background(), "", nil); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if err := RefreshGrokBinModelsIfStale(context.Background(), "", nil); err != nil {
		t.Fatalf("fresh-cache refresh: %v", err)
	}
	if calls != 1 {
		t.Fatalf("model command calls = %d, want 1", calls)
	}
	if ids := GetCachedGrokBinModels(); !containsModelID(ids, "grok-4.6") {
		t.Fatalf("cached models = %v, want grok-4.6", ids)
	}
}

func TestGrokBinModelListCommandEnvDoesNotMutateProviderHome(t *testing.T) {
	p := NewGrokBinProvider("", nil)
	providerHome := t.TempDir()
	listingHome := t.TempDir()
	p.grokHome = providerHome

	env := envSliceMap(p.buildCommandEnvForHome(listingHome))
	if got := env["GROK_HOME"]; got != listingHome {
		t.Fatalf("GROK_HOME = %q, want listing home %q", got, listingHome)
	}
	if p.grokHome != providerHome {
		t.Fatalf("provider grokHome = %q, want unchanged %q", p.grokHome, providerHome)
	}
}

func TestParseGrokModelsOutput(t *testing.T) {
	output := "You are logged in with grok.com.\n\nDefault model: grok-4.6\n\nAvailable models:\n  * grok-4.6 (default)\n  - grok-4.5\n  - grok-composer-2.5-fast\n"
	got := parseGrokModelsOutput(output)
	want := []string{"grok-4.6", "grok-4.5", "grok-composer-2.5-fast"}
	if len(got) != len(want) {
		t.Fatalf("parseGrokModelsOutput IDs = %v, want %v", modelIDs(got), want)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("parseGrokModelsOutput[%d] = %q, want %q", i, got[i].ID, id)
		}
	}
}

func TestParseGrokModelsOutputIgnoresBannerNoise(t *testing.T) {
	output := "You are using XAI_API_KEY.\ntracing initialized\nAvailable models:\n  - grok-4.6\nnot a model line\n  * grok-4.5 (default)\n"
	got := modelIDs(parseGrokModelsOutput(output))
	want := []string{"grok-4.6", "grok-4.5"}
	if !equalSlice(got, want) {
		t.Fatalf("parseGrokModelsOutput = %v, want %v", got, want)
	}
}

func TestExpandGrokBinListedModelsAddsEffortAliases(t *testing.T) {
	got := modelIDs(expandGrokBinListedModels([]ModelInfo{
		{ID: "grok-4.6"},
		{ID: "grok-composer-2.5-fast"},
	}))
	want := []string{
		"grok-4.6", "grok-4.6-low", "grok-4.6-medium", "grok-4.6-high", "grok-4.6-xhigh",
		"grok-composer-2.5-fast",
	}
	if !equalSlice(got, want) {
		t.Fatalf("expandGrokBinListedModels = %v, want %v", got, want)
	}
}

func TestProviderModelIDsPrefersGrokBinCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	curated := ProviderModelIDs("grok-bin")
	if len(curated) == 0 || !containsModelID(curated, "grok-4.6") {
		t.Fatalf("expected curated grok-bin fallback including grok-4.6, got %v", curated)
	}

	RefreshGrokBinCacheSync([]ModelInfo{
		{ID: "grok-4.6"},
		{ID: "grok-4.5"},
	})

	ids := ProviderModelIDs("grok-bin")
	want := []string{
		"grok-4.6", "grok-4.6-low", "grok-4.6-medium", "grok-4.6-high", "grok-4.6-xhigh",
		"grok-4.5", "grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "grok-4.5-xhigh",
	}
	if !equalSlice(ids, want) {
		t.Fatalf("ProviderModelIDs(grok-bin) = %v, want %v", ids, want)
	}
	if containsModelID(ids, "grok-composer-2.5-fast") {
		t.Fatalf("cached grok-bin list unexpectedly kept curated-only model: %v", ids)
	}
}

func TestGrokBinListModelsParsesAndCaches(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binDir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "models" ]; then
  printf '%s\n' 'You are logged in with grok.com.' '' 'Default model: grok-4.6' '' 'Available models:' '  * grok-4.6 (default)' '  - grok-4.5'
  exit 0
fi
echo "unexpected args: $*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "grok"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	grokModelsCommandOutput = func(context.Context, *GrokBinProvider, string) ([]byte, error) {
		return []byte("You are logged in with grok.com.\n\nDefault model: grok-4.6\n\nAvailable models:\n  * grok-4.6 (default)\n  - grok-4.5\n"), nil
	}
	t.Cleanup(func() { grokModelsCommandOutput = defaultGrokModelsCommandOutput })

	models, err := NewGrokBinProvider("", nil).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	wantLive := []string{"grok-4.6", "grok-4.5"}
	if !equalSlice(modelIDs(models), wantLive) {
		t.Fatalf("ListModels IDs = %v, want %v", modelIDs(models), wantLive)
	}
	wantCached := []string{
		"grok-4.6", "grok-4.6-low", "grok-4.6-medium", "grok-4.6-high", "grok-4.6-xhigh",
		"grok-4.5", "grok-4.5-low", "grok-4.5-medium", "grok-4.5-high", "grok-4.5-xhigh",
	}
	if cached := GetCachedGrokBinModels(); !equalSlice(cached, wantCached) {
		t.Fatalf("cached IDs = %v, want %v", cached, wantCached)
	}
	if picker := ProviderModelIDs("grok-bin"); !equalSlice(picker, wantCached) {
		t.Fatalf("ProviderModelIDs = %v, want %v", picker, wantCached)
	}
}

func modelIDs(models []ModelInfo) []string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}
