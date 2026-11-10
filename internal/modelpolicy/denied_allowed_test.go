package modelpolicy

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestDeniedAllowedNarrowsProviderWildcard(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"debug": {AllowedModels: []string{"selected", "shared", "provider-only"}},
	}}
	err := (Policy{}).With("agent", []string{"debug:*"}).CheckWithConfig(cfg, "agent", "other", "blocked")
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CheckWithConfig() = %v, want denial", err)
	}
	if want := []string{"debug:selected", "debug:shared", "debug:provider-only"}; !slices.Equal(denied.Allowed, want) {
		t.Fatalf("DeniedError.Allowed = %v, want %v", denied.Allowed, want)
	}
	if strings.Contains(err.Error(), "debug:blocked-catalog") {
		t.Fatalf("denial advertised a blocked catalog: %v", err)
	}
}

func TestDeniedAllowedDropsBlockedExplicitEntries(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"debug": {AllowedModels: []string{"selected", "shared"}},
	}}
	err := (Policy{}).With("agent", []string{"debug:agent-only", "debug:shared"}).CheckWithConfig(cfg, "agent", "debug", "selected")
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CheckWithConfig() = %v, want denial", err)
	}
	if want := []string{"debug:shared"}; !slices.Equal(denied.Allowed, want) {
		t.Fatalf("DeniedError.Allowed = %v, want %v", denied.Allowed, want)
	}
}

func TestDeniedAllowedReportsIntersectionOfBaseRule(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"custom": {
		AllowedModels: []string{"selected", "upstream/model-high", "upstream/model2"},
		ModelConfigs: []config.ProviderModelConfig{{
			ID: "upstream/model", Alias: "friendly", ReasoningEfforts: []string{"low", "high"},
		}},
	}}}
	err := (Policy{}).With("agent", []string{"custom:upstream/model"}).CheckWithConfig(cfg, "agent", "custom", "selected")
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CheckWithConfig() = %v, want denial", err)
	}
	if want := []string{"custom:upstream/model-high"}; !slices.Equal(denied.Allowed, want) {
		t.Fatalf("DeniedError.Allowed = %v, want %v", denied.Allowed, want)
	}
}

func TestDeniedAllowedNeverEmpty(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"debug": {AllowedModels: []string{"selected", "shared"}},
	}}
	err := (Policy{}).With("agent", []string{"debug:agent-only"}).CheckWithConfig(cfg, "agent", "debug", "selected")
	var denied *DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CheckWithConfig() = %v, want denial", err)
	}
	if len(denied.Allowed) != 1 || !strings.Contains(denied.Allowed[0], "none") {
		t.Fatalf("DeniedError.Allowed = %v, want a non-empty explanation", denied.Allowed)
	}
}
