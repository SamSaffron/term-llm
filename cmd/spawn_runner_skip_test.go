package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestCommitChildDisablesConfiguredToolsWithoutInvalidSentinels(t *testing.T) {
	spawn := &SpawnAgentRunner{}
	host := &runpkg.HostOutputTool{Name: "set_commit_message", Param: "message", Description: "Return message"}
	req := spawn.buildChildExecutionRequest(context.Background(), runpkg.ChildRunRequest{
		Kind:         runpkg.ChildRunCommitDraft,
		AgentName:    "commit-message",
		DisableTools: true,
		OutputTool:   host,
	}, "child", true)
	if req.Tools != "" || req.MCP != "" {
		t.Fatalf("disabled child used sentinel config: tools=%q mcp=%q", req.Tools, req.MCP)
	}
	if req.IncludeConfiguredTools == nil || *req.IncludeConfiguredTools {
		t.Fatal("configured tools were not disabled")
	}
	if req.Skills != "none" || !req.NoSearch || req.Search == nil || *req.Search {
		t.Fatalf("disabled child retained capabilities: skills=%q no_search=%v search=%v", req.Skills, req.NoSearch, req.Search)
	}
	if specs := appendHostOutputToolSpec(nil, host); len(specs) != 1 || specs[0].Name != host.Name {
		t.Fatalf("host output tool specs = %#v", specs)
	}
}

func TestResolveSettingsSuppressesParentAndAgentCapabilitiesForCommitChild(t *testing.T) {
	include := false
	runner := &cmdRunner{defaults: cmdRunnerOptions{Tools: "none", MCP: "unknown-server", Search: true}}
	agent := &agents.Agent{
		Tools:  agents.ToolsConfig{Enabled: []string{"none"}},
		MCP:    []agents.MCPConfig{{Name: "unknown-server"}},
		Skills: "all",
		Search: true,
	}
	settings, err := runner.resolveSettings(&config.Config{}, agent, runpkg.Request{
		Cwd:                    t.TempDir(),
		NoSearch:               true,
		IncludeConfiguredTools: &include,
	}, "")
	if err != nil {
		t.Fatalf("resolveSettings returned invalid disabled config: %v", err)
	}
	if settings.Tools != "" || settings.MCP != "" || settings.Search {
		t.Fatalf("disabled settings retained capabilities: tools=%q mcp=%q search=%v", settings.Tools, settings.MCP, settings.Search)
	}
}

func TestCompleteChildAgentSkipOnComplete(t *testing.T) {
	baseDir := t.TempDir()
	agent := &agents.Agent{OnComplete: "cat > should-not-exist.txt"}
	output, err := completeChildAgent(agent, runpkg.Result{Response: "draft"}, "", baseDir, true)
	if err != nil || output != "draft" {
		t.Fatalf("completeChildAgent = %q, %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(baseDir, "should-not-exist.txt")); !os.IsNotExist(err) {
		t.Fatalf("on_complete ran despite SkipOnComplete: %v", err)
	}
}
