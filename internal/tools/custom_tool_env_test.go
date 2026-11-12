package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func envValues(env []string, name string) []string {
	var out []string
	for _, entry := range env {
		if key, value, _ := strings.Cut(entry, "="); key == name {
			out = append(out, value)
		}
	}
	return out
}

func TestCustomToolEnvironmentStripsForgedSessionContext(t *testing.T) {
	inherited := []string{"PATH=/bin", "TERM_LLM_SESSION_ID=forged", "TERM_LLM_PRIVATE_SESSION=1", "TERM_LLM_UPLOADS_JSON=[]"}
	// A definition constructed without validation still cannot forge context.
	def := agents.CustomToolDef{Name: "lookup", Env: map[string]string{"TERM_LLM_SESSION_ID": "tool-forged", "KEEP": "yes"}}
	for _, tc := range []struct {
		name        string
		ctx         context.Context
		wantSession []string
		wantPrivate []string
	}{
		{name: "unlocked ordinary run", ctx: core.ContextWithSessionID(context.Background(), "plain")},
		{name: "locked admitted run", ctx: core.ContextWithLockedSession(core.ContextWithSessionID(context.Background(), "locked"), "locked"), wantSession: []string{"locked"}, wantPrivate: []string{"1"}},
		{name: "engine session mismatch", ctx: core.ContextWithLockedSession(core.ContextWithSessionID(context.Background(), "child"), "parent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := customToolEnvironment(tc.ctx, inherited, "/agent", def)
			if got := envValues(env, agents.EnvSessionID); !slices.Equal(got, tc.wantSession) {
				t.Fatalf("session id = %v, want %v", got, tc.wantSession)
			}
			if got := envValues(env, agents.EnvPrivateSession); !slices.Equal(got, tc.wantPrivate) {
				t.Fatalf("private = %v, want %v", got, tc.wantPrivate)
			}
			if got := envValues(env, agents.EnvUploadsJSON); len(got) != 0 {
				t.Fatalf("uploads = %v, want none", got)
			}
			if !slices.Equal(envValues(env, "KEEP"), []string{"yes"}) || !slices.Equal(envValues(env, "PATH"), []string{"/bin"}) {
				t.Fatalf("ordinary env lost: %v", env)
			}
		})
	}
}

func TestCustomScriptToolExecuteReceivesOnlyHostSessionContext(t *testing.T) {
	if testing.Short() {
		t.Skip("runs a shell script")
	}
	dir := t.TempDir()
	writeScript(t, dir, "ctx.sh", "#!/bin/sh\necho \"id=${TERM_LLM_SESSION_ID:-none} private=${TERM_LLM_PRIVATE_SESSION:-none} uploads=${TERM_LLM_UPLOADS_JSON:-none}\"\n")
	t.Setenv("TERM_LLM_SESSION_ID", "inherited-forgery")
	t.Setenv("TERM_LLM_PRIVATE_SESSION", "1")
	t.Setenv("TERM_LLM_UPLOADS_JSON", "[\"forged\"]")
	tool := makeCustomTool(t, dir, agents.CustomToolDef{Name: "ctx", Description: "d", Script: "ctx.sh", Env: map[string]string{"TERM_LLM_PRIVATE_SESSION": "1"}})
	// Model arguments are only script input; they cannot set context.
	args := []byte(`{"TERM_LLM_SESSION_ID":"model-forgery"}`)

	plain, err := tool.Execute(core.ContextWithSessionID(context.Background(), "plain"), args)
	if err != nil || !strings.Contains(plain.Content, "id=none private=none uploads=none") {
		t.Fatalf("unlocked output = %q, %v", plain.Content, err)
	}
	lockedCtx := core.ContextWithLockedSession(core.ContextWithSessionID(context.Background(), "sess-1"), "sess-1")
	locked, err := tool.Execute(lockedCtx, args)
	if err != nil || !strings.Contains(locked.Content, "id=sess-1 private=1 uploads=none") {
		t.Fatalf("locked output = %q, %v", locked.Content, err)
	}
}
