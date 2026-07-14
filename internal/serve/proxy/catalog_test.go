package proxy

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"acme": {
				Models: []string{"m1", "m2"},
				ModelConfigs: []config.ProviderModelConfig{
					{ID: "m3", Alias: "fast"},
				},
			},
		},
	}
	return BuildCatalog(cfg)
}

func TestCatalogExportsClaudeBinAndBuiltins(t *testing.T) {
	c := testCatalog(t)
	if !c.HasProvider("claude-bin") {
		t.Fatal("expected built-in claude-bin provider in catalog")
	}
	// claude-bin must be routable over HTTP via at least its wildcard entry.
	found := false
	for _, a := range c.List() {
		if a.Provider == "claude-bin" && a.Model == WildcardModel {
			found = true
			if !a.Builtin {
				t.Fatal("claude-bin should be marked builtin")
			}
		}
	}
	if !found {
		t.Fatal("expected claude-bin/* wildcard entry in catalog listing")
	}
}

func TestCatalogResolve(t *testing.T) {
	c := testCatalog(t)

	tests := []struct {
		name         string
		in           string
		wantProvider string
		wantModel    string
		wantOK       bool
	}{
		{"canonical", "acme/m1", "acme", "m1", true},
		{"configured model", "acme/m2", "acme", "m2", true},
		{"display alias", "fast", "acme", "m3", true},
		{"colon separator", "acme:m1", "acme", "m1", true},
		{"wildcard", "acme/*", "acme", "*", true},
		{"provider qualified unknown model", "acme/brand-new", "acme", "brand-new", true},
		{"claude-bin route", "claude-bin/claude-sonnet-4-6", "claude-bin", "claude-sonnet-4-6", true},
		{"bare unknown ambiguous/none", "totally-unknown", "", "", false},
		{"empty", "  ", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := c.Resolve(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("Resolve(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.Provider != tt.wantProvider || got.Model != tt.wantModel {
				t.Fatalf("Resolve(%q) = %s/%s, want %s/%s", tt.in, got.Provider, got.Model, tt.wantProvider, tt.wantModel)
			}
		})
	}
}

func TestCatalogBareModelResolvesWhenUnambiguous(t *testing.T) {
	c := testCatalog(t)
	// "m1" is only offered by acme, so a bare request resolves.
	got, ok := c.Resolve("m1")
	if !ok || got.Provider != "acme" || got.Model != "m1" {
		t.Fatalf("Resolve(m1) = %+v ok=%v", got, ok)
	}
}
