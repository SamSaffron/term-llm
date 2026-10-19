package opencompat

import (
	"testing"
)

func TestNewSambaNovaProviderTrimsAPIKey(t *testing.T) {
	provider := NewSambaNovaProvider("  test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewSambaNovaProviderStripsBearerPrefix(t *testing.T) {
	provider := NewSambaNovaProvider("  Bearer test-key\n", "")
	if provider.apiKey != "test-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "test-key")
	}
}

func TestNewSambaNovaProviderDefaultsModel(t *testing.T) {
	provider := NewSambaNovaProvider("k", "")
	if provider.model != "gpt-oss-120b" {
		t.Fatalf("model = %q, want gpt-oss-120b", provider.model)
	}
}

func TestSambaNovaProviderCapabilities(t *testing.T) {
	provider := NewSambaNovaProvider("key", "")
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
