package agents

import (
	"strings"
	"testing"
)

func TestLockSessionRequiresAllowedModels(t *testing.T) {
	a := &Agent{Name: "private", LockSession: true}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "lock_session") {
		t.Fatalf("Validate() = %v, want lock_session error", err)
	}
	a.AllowedModels = []string{"debug:allowed"}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestCustomToolEnvRejectsReservedSessionContext(t *testing.T) {
	for _, key := range []string{EnvSessionID, EnvPrivateSession, EnvUploadsJSON, "term_llm_private_session"} {
		a := &Agent{Name: "tools", Tools: ToolsConfig{Custom: []CustomToolDef{{
			Name: "lookup", Description: "d", Script: "s.sh", Env: map[string]string{key: "forged"},
		}}}}
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("%s: Validate() = %v, want reserved error", key, err)
		}
	}
	a := &Agent{Name: "tools", Tools: ToolsConfig{Custom: []CustomToolDef{{
		Name: "lookup", Description: "d", Script: "s.sh", Env: map[string]string{"TERM_LLM_OTHER": "ok"},
	}}}}
	if err := a.Validate(); err != nil {
		t.Fatalf("ordinary env rejected: %v", err)
	}
}
