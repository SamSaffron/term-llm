package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestAskTypedOutputToolRunsOnCompleteAfterCentralizedRunner(t *testing.T) {

	repo := t.TempDir()
	git := exec.Command("git", "init", "-q", repo)
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	gitDir := filepath.Join(repo, ".git")
	for _, name := range []string{"COMMIT_EDITMSG", "GITGUI_MSG"} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte("stale\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	agentDir := filepath.Join(t.TempDir(), "output-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}
	agentYAML := `name: output-agent
output_tool:
  name: set_commit_message
  description: Set the commit result
  schema:
    type: object
    properties:
      message:
        type: string
      category:
        type: string
        enum: [chore, feat, fix]
    required: [message, category]
    additionalProperties: false
on_complete: |
  tee .git/COMMIT_EDITMSG > .git/GITGUI_MSG
`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	configYAML := `default_provider: mock
providers:
  mock:
    model: mock-model
sessions:
  enabled: false
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	const message = "chore: replace stale message"
	const wantOutput = `{"category":"chore","message":"chore: replace stale message"}`
	provider := llm.NewMockProvider("mock").
		WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true}).
		AddToolCall("call-1", "set_commit_message", map[string]string{"message": message, "category": "chore"})
	oldProviderFactory := newAskProvider
	newAskProvider = func(*config.Config, bool) (llm.Provider, error) { return provider, nil }
	t.Cleanup(func() { newAskProvider = oldProviderFactory })

	oldAgent, oldText, oldPorcelain := askAgent, askText, askPorcelain
	oldJSON, oldProgressive, oldFast := askJSON, askProgressive, askFast
	askAgent, askText, askPorcelain = agentDir, true, true
	askJSON, askProgressive, askFast = false, false, false
	t.Cleanup(func() {
		askAgent, askText, askPorcelain = oldAgent, oldText, oldPorcelain
		askJSON, askProgressive, askFast = oldJSON, oldProgressive, oldFast
	})

	var stdout, stderr bytes.Buffer
	oldStdout, oldStderr := askCmd.OutOrStdout(), askCmd.ErrOrStderr()
	askCmd.SetOut(&stdout)
	askCmd.SetErr(&stderr)
	t.Cleanup(func() {
		askCmd.SetOut(oldStdout)
		askCmd.SetErr(oldStderr)
	})
	if err := runAsk(askCmd, []string{"prepare a commit message"}); err != nil {
		t.Fatalf("runAsk: %v\nstderr: %s", err, stderr.String())
	}

	for _, name := range []string{"COMMIT_EDITMSG", "GITGUI_MSG"} {
		got, err := os.ReadFile(filepath.Join(gitDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != wantOutput {
			t.Errorf("%s = %q, want %q", name, got, wantOutput)
		}
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.Requests))
	}
	var outputSpec *llm.ToolSpec
	for i := range provider.Requests[0].Tools {
		if provider.Requests[0].Tools[i].Name == "set_commit_message" {
			outputSpec = &provider.Requests[0].Tools[i]
			break
		}
	}
	if outputSpec == nil {
		t.Fatal("set_commit_message schema was not sent to provider")
	}
	properties, ok := outputSpec.Schema["properties"].(map[string]interface{})
	if !ok || properties["message"] == nil || properties["category"] == nil {
		t.Fatalf("output tool properties = %#v", outputSpec.Schema["properties"])
	}
}
