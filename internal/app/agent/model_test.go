package agent

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func fastCfg() *config.Config {
	return &config.Config{DefaultProvider: "primary", Providers: map[string]config.ProviderConfig{
		"primary": {Model: "main", FastProvider: "small", FastModel: "quick"},
		"small":   {Model: "quick"},
	}}
}

func TestResolveModelOverride(t *testing.T) {
	p, m, ok, err := ResolveModelOverride(fastCfg(), " FAST ")
	if err != nil || !ok || p != "small" || m != "quick" {
		t.Fatalf("got %q %q %v %v", p, m, ok, err)
	}
	if _, m, ok, err := ResolveModelOverride(nil, " plain "); err != nil || ok || m != "plain" {
		t.Fatalf("plain: %q %v %v", m, ok, err)
	}
	if _, _, ok, err := ResolveModelOverride(nil, "fast"); !ok || err == nil {
		t.Fatal("nil config should error")
	}
	if _, _, _, err := ResolveModelOverride(&config.Config{}, "fast"); err == nil {
		t.Fatal("missing provider should error")
	}
	cfg := &config.Config{DefaultProvider: "x", Providers: map[string]config.ProviderConfig{"x": {Model: "m"}}}
	if _, _, _, err := ResolveModelOverride(cfg, "fast"); err == nil {
		t.Fatal("no fast model should error")
	}
}

func TestApplyModelOverrideFast(t *testing.T) {
	cfg := fastCfg()
	if err := ApplyModelOverride(cfg, "fast"); err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultProvider != "small" {
		t.Fatalf("provider = %q", cfg.DefaultProvider)
	}
}
