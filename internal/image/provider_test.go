package image

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestNewImageProviderGeminiModelPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		override  string // provider override string (e.g. "gemini:custom-model")
		cfgModel  string // image.gemini.model config value
		wantModel string
	}{
		{"override model wins", "gemini:custom-model", "config-model", "custom-model"},
		{"config model used when no override", "gemini", "config-model", "config-model"},
		{"default model when both empty", "gemini", "", geminiDefaultModel},
		{"empty override falls back to config", "", "config-model", "config-model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Image.Gemini.APIKey = "test-key"
			cfg.Image.Gemini.Model = tt.cfgModel

			p, err := NewImageProvider(cfg, tt.override)
			if err != nil {
				t.Fatalf("NewImageProvider failed: %v", err)
			}

			gp, ok := p.(*GeminiProvider)
			if !ok {
				t.Fatalf("expected *GeminiProvider, got %T", p)
			}
			if gp.model != tt.wantModel {
				t.Errorf("model=%q, want %q", gp.model, tt.wantModel)
			}
		})
	}
}

func TestNewImageProviderOpenAIModelPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		override  string
		cfgModel  string
		wantModel string
	}{
		{"override model wins", "openai:gpt-image-2", "gpt-image-1-mini", "gpt-image-2"},
		{"config model used when no override", "openai", "gpt-image-1.5", "gpt-image-1.5"},
		{"default model when both empty", "openai", "", openaiDefaultModel},
		{"empty override falls back to config", "", "gpt-image-2", "gpt-image-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Image.Provider = "openai"
			cfg.Image.OpenAI.APIKey = "test-key"
			cfg.Image.OpenAI.Model = tt.cfgModel

			p, err := NewImageProvider(cfg, tt.override)
			if err != nil {
				t.Fatalf("NewImageProvider failed: %v", err)
			}

			op, ok := p.(*OpenAIProvider)
			if !ok {
				t.Fatalf("expected *OpenAIProvider, got %T", p)
			}
			if op.model != tt.wantModel {
				t.Errorf("model=%q, want %q", op.model, tt.wantModel)
			}
		})
	}
}

func TestNewImageProviderGeminiSizePrecedence(t *testing.T) {
	cfg := &config.Config{}
	cfg.Image.Gemini.APIKey = "test-key"
	cfg.Image.Gemini.ImageSize = "2K"

	p, err := NewImageProvider(cfg, "gemini")
	if err != nil {
		t.Fatalf("NewImageProvider failed: %v", err)
	}

	gp := p.(*GeminiProvider)
	if gp.defaultSize != "2K" {
		t.Errorf("defaultSize=%q, want %q", gp.defaultSize, "2K")
	}
}

func TestNewImageProviderGrokAliasRemainsXAIAPIKeyProvider(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root+"/config")
	cfg := &config.Config{}
	cfg.Image.XAI.APIKey = "test-key"
	cfg.Image.XAI.Model = "grok-2-image-1212"
	provider, err := NewImageProvider(cfg, "grok")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*XAIProvider); !ok {
		t.Fatalf("image provider grok created %T, want *XAIProvider", provider)
	}
}

func TestValidateImageOptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		valid   func(string) error
		wantErr bool
	}{
		{"empty quality", "", ValidateQuality, false},
		{"max quality", "max", ValidateQuality, false},
		{"bad quality", "ultra", ValidateQuality, true},
		{"transparent background", "transparent", ValidateBackground, false},
		{"bad background", "checkerboard", ValidateBackground, true},
		{"empty ratio", "", ValidateAspectRatio, false},
		{"landscape ratio", "16:9", ValidateAspectRatio, false},
		{"wide ratio", "4:1", ValidateAspectRatio, false},
		{"zero ratio", "16:0", ValidateAspectRatio, true},
		{"malformed ratio", "wide", ValidateAspectRatio, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.valid(tc.value); (err != nil) != tc.wantErr {
				t.Fatalf("validation error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateSize(t *testing.T) {
	tests := []struct {
		size    string
		wantErr bool
	}{
		{"", false},
		{"1K", false},
		{"2K", false},
		{"4K", false},
		{"2k", true},
		{"512px", true},
		{"2048", true},
		{"large", true},
	}
	for _, tt := range tests {
		name := tt.size
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			err := ValidateSize(tt.size)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSize(%q) error=%v, wantErr=%v", tt.size, err, tt.wantErr)
			}
		})
	}
}
