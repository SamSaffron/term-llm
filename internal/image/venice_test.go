package image

import "testing"

func TestNewVeniceProviderDefaults(t *testing.T) {
	provider := NewVeniceProvider("api-key", "", "")
	if provider.model != veniceDefaultModel {
		t.Errorf("expected default model %q, got %q", veniceDefaultModel, provider.model)
	}
	if provider.resolution != veniceDefaultResolution {
		t.Errorf("expected default resolution %q, got %q", veniceDefaultResolution, provider.resolution)
	}
}

func TestNewVeniceProviderCustom(t *testing.T) {
	provider := NewVeniceProvider("key", "flux-2-max", "4K")
	if provider.model != "flux-2-max" {
		t.Errorf("expected model %q, got %q", "flux-2-max", provider.model)
	}
	if provider.resolution != "4K" {
		t.Errorf("expected resolution %q, got %q", "4K", provider.resolution)
	}
}

func TestVeniceProviderCapabilities(t *testing.T) {
	provider := NewVeniceProvider("key", "", "")
	if !provider.SupportsEdit() {
		t.Error("expected SupportsEdit() = true")
	}
	if !provider.SupportsMultiImage() {
		t.Error("expected SupportsMultiImage() = true")
	}
}
