package llm

import (
	"context"
	"testing"
)

func TestParseAgyModelsOutput(t *testing.T) {
	output := "gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n" +
		"gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)\n" +
		"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n" +
		"claude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)\n" +
		"gpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n"

	models := parseAgyModelsOutput(output)
	wantIDs := []string{
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-low",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	}
	if got := modelIDs(models); !equalSlice(got, wantIDs) {
		t.Fatalf("parseAgyModelsOutput IDs = %v, want %v", got, wantIDs)
	}
	if got := models[3].DisplayName; got != "Claude Sonnet 4.6 (Thinking)" {
		t.Fatalf("DisplayName = %q, want %q", got, "Claude Sonnet 4.6 (Thinking)")
	}
}

func TestAgyBinListModelsParsesAndCaches(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	original := agyModelsCommandOutput
	t.Cleanup(func() { agyModelsCommandOutput = original })
	agyModelsCommandOutput = func(context.Context, *AgyBinProvider) ([]byte, error) {
		return []byte("gemini-3.8-flash-high\tGemini 3.8 Flash (High)\nclaude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)\n"), nil
	}

	models, err := NewAgyBinProvider("", nil).ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"gemini-3.8-flash-high", "claude-sonnet-4-6"}
	if got := modelIDs(models); !equalSlice(got, want) {
		t.Fatalf("ListModels IDs = %v, want %v", got, want)
	}
	if got := GetCachedAgyBinModels(); !equalSlice(got, want) {
		t.Fatalf("cached IDs = %v, want %v", got, want)
	}
	if got := ProviderModelIDs("agy-bin"); !equalSlice(got, want) {
		t.Fatalf("ProviderModelIDs = %v, want %v", got, want)
	}
}

func TestRefreshAgyBinModelsIfStaleReusesFreshCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	original := agyModelsCommandOutput
	t.Cleanup(func() { agyModelsCommandOutput = original })

	calls := 0
	agyModelsCommandOutput = func(context.Context, *AgyBinProvider) ([]byte, error) {
		calls++
		return []byte("gemini-3.8-flash-high\tGemini 3.8 Flash (High)\n"), nil
	}

	for range 2 {
		if err := RefreshAgyBinModelsIfStale(context.Background(), "", nil); err != nil {
			t.Fatalf("RefreshAgyBinModelsIfStale: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("agy models calls = %d, want 1", calls)
	}
}
