package ask

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestSessionInputAskResumeUsesWorkspaceAndExplicitInputsOnce(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	// The system prompt is asserted exactly, so user-global skills (discovered
	// under HOME and CODEX_HOME) must not be appended.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "sessions.db")
	configYAML := fmt.Sprintf("default_provider: mock\nproviders:\n  mock:\n    model: mock-model\nsessions:\n  enabled: true\n  path: %q\n", path)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	include := filepath.Join(workspace, "resume.md")
	if err := os.WriteFile(include, []byte("resumed workspace include"), 0644); err != nil {
		t.Fatal(err)
	}
	store := testutil.NewSessionInputStore(t, path)
	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), CWD: workspace, Provider: "mock", ProviderKey: "mock", Model: "mock-model", Tools: "read_file", Mode: session.ModeAsk}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.SystemText("old prompt"), 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("earlier human"), 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.AssistantText("earlier answer"), 2)); err != nil {
		t.Fatal(err)
	}
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("first answer").AddTextResponse("second answer")
	t.Cleanup(appserve.SwapProcessSessionInputs(appserve.NewSessionInputCoordinator()))
	var output bytes.Buffer
	opts := Options{Resume: sess.ID, Tools: "view_image", SystemMessage: "CLI {{file:resume.md}}", Text: true, Porcelain: true, MaxTurns: 50, Changed: FlagState{Resume: true, Tools: true, System: true}}
	deps := testDependencies(&output, &output)
	deps.SessionStore.DBPath = path
	deps.NewProvider = func(*config.Config, bool) (core.Provider, error) { return provider, nil }
	for turn := 0; turn < 2; turn++ {
		if turn == 1 {
			// A later resume in the same process must not re-expand includes or use
			// a newly supplied list; explicit fresh/rebind operations invalidate it.
			if err := os.Remove(include); err != nil {
				t.Fatal(err)
			}
			opts.Tools = "shell"
		}
		if err := Run(ctx, []string{"continue"}, opts, deps); err != nil {
			t.Fatalf("runAsk: %v\n%s", err, output.String())
		}
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider turns=%d", len(provider.Requests))
	}
	for _, request := range provider.Requests {
		systemCount := 0
		for _, message := range request.Messages {
			if message.Role == core.RoleSystem {
				systemCount++
				if core.MessageText(message) != "CLI resumed workspace include" {
					t.Fatalf("wrong system: %q", core.MessageText(message))
				}
			}
		}
		if systemCount != 1 {
			t.Fatalf("system count=%d", systemCount)
		}
		found := false
		for _, tool := range request.Tools {
			if tool.Name == "view_image" {
				found = true
			}
			if tool.Name == "shell" || tool.Name == "read_file" {
				t.Fatalf("wrong executable selection: %s", tool.Name)
			}
		}
		if !found {
			t.Fatal("current explicit tool was not registered")
		}
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Tools != "view_image" || persisted.Search != sess.Search || persisted.MCP != sess.MCP || persisted.Provider != sess.Provider || persisted.Model != sess.Model {
		t.Fatalf("wrong metadata: %+v", persisted)
	}
}
