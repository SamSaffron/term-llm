package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadGrepRerankYAML(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if yaml != "" {
		dir, err := GetConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return Load()
}

func TestGrepRerankDefaults(t *testing.T) {
	cfg, err := loadGrepRerankYAML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	want := GrepRerankConfig{Enabled: true, Threshold: 0.5, TimeoutMS: 2000, MaxCandidates: 150}
	if cfg.Tools.GrepRerank != want {
		t.Fatalf("defaults = %+v, want %+v", cfg.Tools.GrepRerank, want)
	}
	if !IsKnownKey("tools.grep_rerank.provider") {
		t.Fatal("tools.grep_rerank.provider not a known key")
	}
	if _, ok := GetDefaults()["tools.grep_rerank.provider"]; ok {
		t.Fatal("provider must not carry a persisted default")
	}
}

func TestGrepRerankLoadsOverridesAndExplicitFalse(t *testing.T) {
	cfg, err := loadGrepRerankYAML(t, "tools:\n  grep_rerank:\n    enabled: false\n    provider: nonexistent\n    threshold: 0.25\n    timeout_ms: 500\n    max_candidates: 40\n")
	if err != nil {
		t.Fatalf("unknown provider must not fail load: %v", err)
	}
	want := GrepRerankConfig{Enabled: false, Provider: "nonexistent", Threshold: 0.25, TimeoutMS: 500, MaxCandidates: 40}
	if cfg.Tools.GrepRerank != want {
		t.Fatalf("got %+v, want %+v", cfg.Tools.GrepRerank, want)
	}
	data, err := yaml.Marshal(map[string]any{"tools": map[string]any{"grep_rerank": cfg.Tools.GrepRerank}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enabled: false") {
		t.Fatalf("explicit false lost during serialization: %s", data)
	}
	roundTrip, err := loadGrepRerankYAML(t, string(data))
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.Tools.GrepRerank != want {
		t.Fatalf("round trip = %+v, want %+v", roundTrip.Tools.GrepRerank, want)
	}
}

func TestGrepRerankLoadRejectsInvalidRanges(t *testing.T) {
	if _, err := loadGrepRerankYAML(t, "tools:\n  grep_rerank:\n    timeout_ms: 20000\n"); err == nil || !strings.Contains(err.Error(), "tools.grep_rerank.timeout_ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestGrepRerankValidate(t *testing.T) {
	valid := GrepRerankConfig{Enabled: true, Threshold: 0.5, TimeoutMS: 2000, MaxCandidates: 150}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*GrepRerankConfig){
		"nan threshold":      func(c *GrepRerankConfig) { c.Threshold = math.NaN() },
		"inf threshold":      func(c *GrepRerankConfig) { c.Threshold = math.Inf(1) },
		"negative threshold": func(c *GrepRerankConfig) { c.Threshold = -0.1 },
		"high threshold":     func(c *GrepRerankConfig) { c.Threshold = 1.1 },
		"zero timeout":       func(c *GrepRerankConfig) { c.TimeoutMS = 0 },
		"huge timeout":       func(c *GrepRerankConfig) { c.TimeoutMS = 10001 },
		"one candidate":      func(c *GrepRerankConfig) { c.MaxCandidates = 1 },
		"many candidates":    func(c *GrepRerankConfig) { c.MaxCandidates = 501 },
	}
	for name, mutate := range cases {
		c := valid
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	edge := valid
	edge.Threshold, edge.TimeoutMS, edge.MaxCandidates = 1, 10000, 2
	if err := edge.Validate(); err != nil {
		t.Fatalf("upper/lower bounds: %v", err)
	}
	edge.Threshold, edge.MaxCandidates = 0, 500
	if err := edge.Validate(); err != nil {
		t.Fatalf("bounds: %v", err)
	}
	// Unknown provider never invalidates the config.
	edge.Provider = "does-not-exist"
	if err := edge.Validate(); err != nil {
		t.Fatalf("provider: %v", err)
	}
}
