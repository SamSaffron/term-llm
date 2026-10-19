package opencompat

import (
	"math"
	"testing"
)

func TestNewNearAIProviderTrimsAPIKey(t *testing.T) {
	provider := NewNearAIProvider("  test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewNearAIProviderStripsBearerPrefix(t *testing.T) {
	provider := NewNearAIProvider("  Bearer test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewNearAIProviderDefaultsModel(t *testing.T) {
	provider := NewNearAIProvider("k", "")
	if provider.model != "zai-org/GLM-5.1-FP8" {
		t.Fatalf("model = %q, want zai-org/GLM-5.1-FP8", provider.model)
	}
}

func TestNearAIProviderCapabilities(t *testing.T) {
	provider := NewNearAIProvider("key", "")
	caps := provider.Capabilities()
	if caps.NativeWebSearch {
		t.Fatal("expected NativeWebSearch=false")
	}
	if caps.NativeWebFetch {
		t.Fatal("expected NativeWebFetch=false")
	}
	if !caps.ToolCalls {
		t.Fatal("expected ToolCalls=true")
	}
	if !caps.SupportsToolChoice {
		t.Fatal("expected SupportsToolChoice=true")
	}
}

func nearPriceEqual(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}
