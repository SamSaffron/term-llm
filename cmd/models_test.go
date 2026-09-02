package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/spf13/viper"
)

func TestSupportedModelListProviderTypesIncludesCLIProviders(t *testing.T) {
	got := supportedModelListProviderTypes()
	for _, want := range []string{"agy-bin", "cursor-bin", "grok-bin"} {
		if !slices.Contains(got, want) {
			t.Fatalf("supported provider types %v missing %q", got, want)
		}
	}
	if !slices.IsSorted(got) {
		t.Fatalf("supported provider types are not sorted: %v", got)
	}
}

func TestModelListSupportedTypesIncludesSambaNova(t *testing.T) {
	if !modelListSupportedTypes[config.ProviderTypeSambaNova] {
		t.Fatal("SambaNova should be wired for dynamic model listing")
	}
}

func TestModelListSupportedTypesIncludesNearAI(t *testing.T) {
	if !modelListSupportedTypes[config.ProviderTypeNearAI] {
		t.Fatal("NEAR AI should be wired for dynamic model listing")
	}
}

func TestModelListSupportedTypesIncludesOllama(t *testing.T) {
	if !modelListSupportedTypes[config.ProviderTypeOllama] {
		t.Fatal("Ollama should be wired for dynamic model listing")
	}
}

func TestRunModelsQueriesAgyCLIAndCachesCatalog(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	binDir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "models" ]; then
  printf '%b\n' \
    'gemini-3.8-flash-high\tGemini 3.8 Flash (High)' \
    'gemini-3.8-flash-medium\tGemini 3.8 Flash (Medium)' \
    'gemini-3.8-flash-low\tGemini 3.8 Flash (Low)' \
    'claude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)' \
    'gpt-oss-120b-medium\tGPT-OSS 120B (Medium)'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(filepath.Join(binDir, "agy"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	oldProvider, oldJSON := modelsProvider, modelsJSON
	modelsProvider, modelsJSON = "agy-bin", true
	t.Cleanup(func() {
		modelsProvider, modelsJSON = oldProvider, oldJSON
	})

	readOut, writeOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = writeOut
	err = runModels(modelsCmd, nil)
	_ = writeOut.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("runModels: %v", err)
	}
	defer readOut.Close()

	var models []llm.ModelInfo
	if err := json.NewDecoder(readOut).Decode(&models); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	want := []string{
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-low",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("listed models = %v, want %v", got, want)
	}
	if cached := llm.GetCachedAgyBinModels(); !slices.Equal(cached, want) {
		t.Fatalf("cached models = %v, want %v", cached, want)
	}
}

func TestRunModelsQueriesConfiguredOllamaEndpoint(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"remote-only:latest"},{"name":"embedding:4b"}]}`)
		case "/api/show":
			fmt.Fprint(w, `{"capabilities":["completion"]}`)
		default:
			t.Errorf("requested unexpected path %q", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	configHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configYAML := fmt.Sprintf("default_provider: ollama\nproviders:\n  ollama:\n    type: ollama\n    base_url: %s\n    model: remote-only:latest\n", server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	oldProvider, oldJSON := modelsProvider, modelsJSON
	modelsProvider, modelsJSON = "ollama", true
	t.Cleanup(func() {
		modelsProvider, modelsJSON = oldProvider, oldJSON
	})

	readOut, writeOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = writeOut
	err = runModels(modelsCmd, nil)
	_ = writeOut.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("runModels: %v", err)
	}
	var models []llm.ModelInfo
	if err := json.NewDecoder(readOut).Decode(&models); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	_ = readOut.Close()

	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	want := []string{"remote-only:latest", "embedding:4b"}
	if !slices.Equal(got, want) {
		t.Fatalf("listed models = %v, want %v", got, want)
	}
	if slices.Contains(got, "qwen2.5-coder:7b") {
		t.Fatalf("listed models unexpectedly contain static fallback: %v", got)
	}
}

func TestModelListSupportedTypesIncludesGrok(t *testing.T) {
	isolateGrokCmdTestEnv(t)
	if !modelListSupportedTypes[config.ProviderTypeGrok] {
		t.Fatal("grok should be wired for authenticated dynamic model listing")
	}
}

func TestBuiltinProviderMetaGrok(t *testing.T) {
	isolateGrokCmdTestEnv(t)
	meta, ok := builtinProviderMeta["grok"]
	if !ok {
		t.Fatal("grok provider metadata missing")
	}
	if meta.requiresKey || meta.credential != "oauth" || !meta.supportsListModels {
		t.Fatalf("grok metadata = %+v", meta)
	}
}

func TestBuiltinProviderMetaGrokBin(t *testing.T) {
	isolateGrokCmdTestEnv(t)
	meta, ok := builtinProviderMeta["grok-bin"]
	if !ok {
		t.Fatal("grok-bin provider metadata missing")
	}
	if meta.requiresKey || meta.credential != "oauth" || !meta.supportsListModels {
		t.Fatalf("grok-bin metadata = %+v, want OAuth listing without required API key", meta)
	}
}

func TestModelListSupportedTypesIncludesGrokBin(t *testing.T) {
	isolateGrokCmdTestEnv(t)
	if !modelListSupportedTypes[config.ProviderTypeGrokBin] {
		t.Fatal("grok-bin should be wired for dynamic model listing")
	}
}

func TestModelListSupportedTypesIncludesCursorBin(t *testing.T) {
	if !modelListSupportedTypes[config.ProviderTypeCursorBin] {
		t.Fatal("cursor-bin should be wired for dynamic model listing")
	}
}

func TestBuiltinProviderMetaCursorBin(t *testing.T) {
	meta, ok := builtinProviderMeta["cursor-bin"]
	if !ok {
		t.Fatal("cursor-bin provider metadata missing")
	}
	if meta.requiresKey || meta.credential != "oauth" || !meta.supportsListModels {
		t.Fatalf("cursor-bin metadata = %+v", meta)
	}
}

func TestBuiltinProviderMetaSambaNovaSupportsListModels(t *testing.T) {
	meta, ok := builtinProviderMeta["sambanova"]
	if !ok {
		t.Fatal("SambaNova provider metadata missing")
	}
	if !meta.supportsListModels {
		t.Fatal("SambaNova should advertise model listing support")
	}
	if meta.envVar != "SAMBANOVA_API_KEY" {
		t.Fatalf("SambaNova env var = %q, want SAMBANOVA_API_KEY", meta.envVar)
	}
}

func TestBuiltinProviderMetaNearAISupportsListModels(t *testing.T) {
	meta, ok := builtinProviderMeta["nearai"]
	if !ok {
		t.Fatal("NEAR AI provider metadata missing")
	}
	if !meta.supportsListModels {
		t.Fatal("NEAR AI should advertise model listing support")
	}
	if meta.envVar != "NEARAI_API_KEY" {
		t.Fatalf("NEAR AI env var = %q, want NEARAI_API_KEY", meta.envVar)
	}
}
