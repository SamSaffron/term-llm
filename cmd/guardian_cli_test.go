package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestGuardianClassifyConfigRenderingAndCompletion(t *testing.T) {
	if got := formatDefaultValue(config.GetDefaults()["guardian.classify.min_confidence"]); got != "0.15" {
		t.Fatalf("default render: %s", got)
	}
	if !valueMatchesDefault("0.5", 0.5) {
		t.Fatal("float default comparison")
	}
	cfg := &config.Config{Classify: config.ClassifyConfig{Providers: map[string]config.ClassifyProviderConfig{"alias": {Type: "typesafe"}}}}
	if got := capabilityConfigValueCompletions(cfg, "guardian.backend", ""); strings.Join(got, ",") != "llm,classify" {
		t.Fatalf("backend: %v", got)
	}
	if got := capabilityConfigValueCompletions(cfg, "guardian.classify.provider", "a"); strings.Join(got, ",") != "alias" {
		t.Fatalf("provider: %v", got)
	}
}

func TestGuardianFallbackConfigKeysAndCompletion(t *testing.T) {
	for _, key := range []string{"guardian.fallback.provider", "guardian.fallback.model", "guardian.fallback.log_path"} {
		if !config.IsKnownKey(key) {
			t.Fatalf("known key missing %s", key)
		}
		if _, ok := config.GetDefaults()[key]; ok {
			t.Fatalf("%s must not carry a persisted default", key)
		}
	}
	if got := configValueCompletions("guardian.fallback.provider", "chat"); !slices.Contains(got, "chatgpt") {
		t.Fatalf("guardian.fallback.provider completions = %v, want provider names", got)
	}
}
