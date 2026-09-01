package config

import "testing"

func TestCommitMessageAgentDefaultAndValidation(t *testing.T) {
	if got := (&Config{}).Commit.EffectiveMessageAgent(); got != "commit-message" {
		t.Fatalf("default message agent = %q", got)
	}
	cfg := &Config{Commit: CommitConfig{MessageAgent: "my-writer"}}
	if err := cfg.ValidateCommit(); err != nil || cfg.Commit.EffectiveMessageAgent() != "my-writer" {
		t.Fatalf("custom message agent rejected: %v", err)
	}
	for _, name := range []string{"../escape", "nested/agent", "bad\nname"} {
		if err := (&Config{Commit: CommitConfig{MessageAgent: name}}).ValidateCommit(); err == nil {
			t.Fatalf("unsafe message agent %q accepted", name)
		}
	}
	if got := GetDefaults()["commit.message_agent"]; got != "commit-message" {
		t.Fatalf("schema default = %#v", got)
	}
}
