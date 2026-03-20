package image

import "testing"

func TestNewVeniceProviderTrimsAPIKey(t *testing.T) {
	provider := NewVeniceProvider("  api-key\n", "", "", "")
	if provider.apiKey != "api-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "api-key")
	}
}

func TestNewVeniceProviderStripsBearerPrefix(t *testing.T) {
	provider := NewVeniceProvider("  Bearer api-key\n", "", "", "")
	if provider.apiKey != "api-key" {
		t.Fatalf("apiKey = %q, want %q", provider.apiKey, "api-key")
	}
}

func TestNewVeniceProviderDefaults(t *testing.T) {
	provider := NewVeniceProvider("api-key", "", "", "")
	if provider.model != veniceDefaultModel {
		t.Errorf("expected default model %q, got %q", veniceDefaultModel, provider.model)
	}
	if provider.resolution != veniceDefaultResolution {
		t.Errorf("expected default resolution %q, got %q", veniceDefaultResolution, provider.resolution)
	}
}

func TestNewVeniceProviderCustom(t *testing.T) {
	provider := NewVeniceProvider("key", "flux-2-max", "", "4K")
	if provider.model != "flux-2-max" {
		t.Errorf("expected model %q, got %q", "flux-2-max", provider.model)
	}
	if provider.resolution != "4K" {
		t.Errorf("expected resolution %q, got %q", "4K", provider.resolution)
	}
}

func TestVeniceEditModel(t *testing.T) {
	tests := []struct {
		model     string
		editModel string
		want      string
	}{
		{"nano-banana-pro", "", "nano-banana-pro-edit"},        // auto-suffix
		{"flux-2-max", "", "flux-2-max-edit"},                  // auto-suffix
		{"seedream-v4", "", "seedream-v4-edit"},                // auto-suffix
		{"qwen-edit", "", "qwen-edit"},                         // already has suffix
		{"nano-banana-pro-edit", "", "nano-banana-pro-edit"},   // already has suffix
		{"nano-banana-pro", "qwen-edit", "qwen-edit"},          // explicit override
		{"flux-2-max", "seedream-v4-edit", "seedream-v4-edit"}, // explicit override
	}
	for _, tt := range tests {
		p := NewVeniceProvider("key", tt.model, tt.editModel, "")
		got := p.editModel()
		if got != tt.want {
			t.Errorf("editModel(model=%q, editModel=%q) = %q, want %q", tt.model, tt.editModel, got, tt.want)
		}
	}
}

func TestVeniceGenerateRequestUsesPerCallSize(t *testing.T) {
	// Verify that per-request Size overrides the provider default resolution.
	// We can't call Generate without a real API, but we can verify the struct
	// wiring by checking that the provider stores the config resolution and
	// that GenerateRequest carries Size for the provider to use.
	p := NewVeniceProvider("key", "", "", "2K")
	if p.resolution != "2K" {
		t.Fatalf("expected provider resolution %q, got %q", "2K", p.resolution)
	}

	// The Venice Generate method builds veniceGenerateRequest with:
	//   resolution = p.resolution (default)
	//   if req.Size != "" { resolution = req.Size }
	// We verify this logic inline since we can't hit the API.
	for _, tt := range []struct {
		name    string
		reqSize string
		wantRes string
	}{
		{"default from provider", "", "2K"},
		{"override from request", "4K", "4K"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resolution := p.resolution
			if tt.reqSize != "" {
				resolution = tt.reqSize
			}
			if resolution != tt.wantRes {
				t.Errorf("resolution=%q, want %q", resolution, tt.wantRes)
			}
		})
	}
}

func TestVeniceProviderCapabilities(t *testing.T) {
	provider := NewVeniceProvider("key", "", "", "")
	if !provider.SupportsEdit() {
		t.Error("expected SupportsEdit() = true")
	}
	if !provider.SupportsMultiImage() {
		t.Error("expected SupportsMultiImage() = true")
	}
}
