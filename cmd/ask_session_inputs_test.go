package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestSessionInputAskResumeUsesWorkspaceAndExplicitInputsOnce(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
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
	store := inputTestStore(t, path)
	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), CWD: workspace, Provider: "mock", ProviderKey: "mock", Model: "mock-model", Tools: "read_file", Mode: session.ModeAsk}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.SystemText("old prompt"), 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText("earlier human"), 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.AssistantText("earlier answer"), 2)); err != nil {
		t.Fatal(err)
	}
	provider := llm.NewMockProvider("mock").AddTextResponse("first answer").AddTextResponse("second answer")
	oldFactory := newAskProvider
	newAskProvider = func(*config.Config, bool) (llm.Provider, error) { return provider, nil }
	t.Cleanup(func() { newAskProvider = oldFactory })
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })
	oldResume, oldTools, oldSystem, oldAgent := askResume, askTools, askSystemMessage, askAgent
	oldText, oldPorcelain, oldApproval, oldYolo, oldAuto := askText, askPorcelain, askApproval, askYolo, askAuto
	oldDB, oldNoSession := sessionDBPath, noSession
	askResume, askTools, askSystemMessage, askAgent = sess.ID, "view_image", "CLI {{file:resume.md}}", ""
	askText, askPorcelain = true, true
	sessionDBPath = path
	noSession = false
	t.Cleanup(func() {
		askResume, askTools, askSystemMessage, askAgent = oldResume, oldTools, oldSystem, oldAgent
		askText, askPorcelain, askApproval, askYolo, askAuto = oldText, oldPorcelain, oldApproval, oldYolo, oldAuto
		sessionDBPath, noSession = oldDB, oldNoSession
	})
	cmd := &cobra.Command{}
	for _, flag := range []string{"resume", "tools", "system"} {
		cmd.Flags().String(flag, "", "")
		if err := cmd.Flags().Set(flag, "set"); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	for turn := 0; turn < 2; turn++ {
		if turn == 1 {
			// A later resume in the same process must not re-expand includes or use
			// a newly supplied list; explicit fresh/rebind operations invalidate it.
			if err := os.Remove(include); err != nil {
				t.Fatal(err)
			}
			askTools = "shell"
		}
		if err := runAsk(cmd, []string{"continue"}); err != nil {
			t.Fatalf("runAsk: %v\n%s", err, output.String())
		}
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider turns=%d", len(provider.Requests))
	}
	for _, request := range provider.Requests {
		systemCount := 0
		for _, message := range request.Messages {
			if message.Role == llm.RoleSystem {
				systemCount++
				if llm.MessageText(message) != "CLI resumed workspace include" {
					t.Fatalf("wrong system: %q", llm.MessageText(message))
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
