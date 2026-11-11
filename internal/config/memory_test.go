package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMemoryDeclarativePolicyRoundTripAndSensitiveSchema(t *testing.T) {
	cfg, err := loadGrepRerankYAML(t, "memory:\n  secret_patterns:\n    - 'synthetic-local-[0-9]+'\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"synthetic-local-[0-9]+"}
	if !reflect.DeepEqual(cfg.Memory.SecretPatterns, want) {
		t.Fatal("typed detector list not decoded")
	}
	data, err := yaml.Marshal(map[string]any{"memory": cfg.Memory})
	if err != nil {
		t.Fatal(err)
	}
	round, err := loadGrepRerankYAML(t, string(data))
	if err != nil || !reflect.DeepEqual(round.Memory.SecretPatterns, want) {
		t.Fatal("typed policy round trip failed")
	}
	found := false
	for _, spec := range ConfigKeySpecs() {
		if spec.Path == "memory.secret_patterns" {
			found = true
			if !spec.Sensitive || !spec.RedactExplicit {
				t.Fatal("literal detector values may leak through config display")
			}
		}
	}
	if !found {
		t.Fatal("native detector key absent from schema")
	}
}

func TestMemoryPolicyRejectsUnknownAndWeaklyCoercedSettings(t *testing.T) {
	for _, input := range []string{
		"memory: false\n",
		"memory:\n  enabled: false\n",
		"memory:\n  command: sensitive-literal\n",
		"memory:\n  secret_patterns: sensitive-literal\n",
		"memory:\n  secret_patterns: [123]\n",
	} {
		if _, err := loadGrepRerankYAML(t, input); err == nil || strings.Contains(err.Error(), "sensitive-literal") {
			t.Fatal("invalid declarative policy accepted or disclosed")
		}
	}
}
