package cmd

import (
	"slices"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestSupportedModelListProviderTypesIncludesCLIProviders(t *testing.T) {
	got := supportedModelListProviderTypes()
	for _, want := range []string{"cursor-bin", "grok-bin"} {
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

func TestBuiltinProviderMetaGrokBin(t *testing.T) {
	meta, ok := builtinProviderMeta["grok-bin"]
	if !ok {
		t.Fatal("grok-bin provider metadata missing")
	}
	if meta.requiresKey || meta.credential != "oauth" || !meta.supportsListModels {
		t.Fatalf("grok-bin metadata = %+v, want OAuth listing without required API key", meta)
	}
}

func TestModelListSupportedTypesIncludesGrokBin(t *testing.T) {
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
