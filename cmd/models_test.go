package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

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
