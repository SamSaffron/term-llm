package config

import (
	"gopkg.in/yaml.v3"
	"reflect"
	"strings"
	"testing"
)

func TestMemoryOwnerPolicyRoundTrip(t *testing.T) {
	for _, input := range []string{"", "memory: {}\n", "memory:\n  secrets:\n    disable: []\n    patterns: []\n  exclude_paths: []\n", "memory:\n  secrets:\n    disable: [cli-user]\n", "memory:\n  secrets:\n    patterns: ['synthetic_private_[A-Z0-9]+']\n", "memory:\n  secrets:\n    disable: [all]\n    patterns: ['synthetic_private_[A-Z0-9]+']\n", "memory:\n  secrets:\n    disable: [all]\n", "memory:\n  exclude_paths: ['fragments/credentials/**']\n"} {
		cfg, err := loadGrepRerankYAML(t, input)
		if err != nil {
			t.Fatal(err)
		}
		data, err := yaml.Marshal(map[string]any{"memory": cfg.Memory})
		if err != nil {
			t.Fatal(err)
		}
		round, err := loadGrepRerankYAML(t, string(data))
		if err != nil || !reflect.DeepEqual(round.Memory, cfg.Memory) {
			t.Fatal("owner policy round trip changed meaning")
		}
	}
	for _, spec := range ConfigKeySpecs() {
		if spec.Path == "memory.secrets.patterns" && (!spec.Sensitive || !spec.RedactExplicit) {
			t.Fatal("pattern literals not sensitive")
		}
	}
}
func TestMemoryPolicyStrictTypesAndUnknownOptions(t *testing.T) {
	for _, input := range []string{"memory: false\n", "memory:\n  enabled: false\n", "memory:\n  command: sensitive-literal\n", "memory:\n  secrets:\n    patterns: sensitive-literal\n", "memory:\n  secrets:\n    patterns: [123]\n", "memory:\n  secrets:\n    disable: true\n"} {
		if _, err := loadGrepRerankYAML(t, input); err == nil || strings.Contains(err.Error(), "sensitive-literal") {
			t.Fatal("invalid owner policy accepted or literal disclosed")
		}
	}
	if _, err := loadGrepRerankYAML(t, "memory:\n  secrets:\n    typo: []\n"); err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatal("unknown key diagnostic not actionable")
	}
}
func TestMemoryOwnerPolicyNotFromInheritedEnvironment(t *testing.T) {
	t.Setenv("TERM_LLM_MEMORY_SECRETS_DISABLE", "all")
	t.Setenv("TERM_LLM_MEMORY_SECRETS_PATTERNS", ".*")
	cfg, err := loadGrepRerankYAML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Memory.Secrets.Disable) != 0 || len(cfg.Memory.Secrets.Patterns) != 0 {
		t.Fatal("inherited environment selected policy")
	}
}
