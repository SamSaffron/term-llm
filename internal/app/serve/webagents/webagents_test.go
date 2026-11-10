package webagents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/tools"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	common := config.DefaultWebAgentPickerCommon()
	return &config.Config{
		Agents: config.AgentsConfig{UseBuiltin: true},
		Web: config.WebConfig{
			ProjectDefaultAgent: config.DefaultWebProjectDefaultAgent,
			AgentPicker:         config.WebAgentPickerConfig{Common: &common},
		},
	}
}

func writeTestAgent(t *testing.T, cfg *config.Config, name, body string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "agents", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: "+name+"\ndescription: test\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.SearchPaths = append(cfg.Agents.SearchPaths, filepath.Dir(dir))
}

func TestResolveRequiresAvailableProjectDefault(t *testing.T) {
	cfg := testConfig(t)
	cfg.Web.ProjectDefaultAgent = "missing-agent"
	_, err := Resolve(cfg, "", []string{"developer", "sysadmin"}, true, true, "", false, "")
	if err == nil || !strings.Contains(err.Error(), `web.project_default_agent "missing-agent"`) {
		t.Fatalf("error = %v, want unavailable project default", err)
	}

	cfg.Web.ProjectDefaultAgent = ""
	if _, err := Resolve(cfg, "", []string{"developer"}, true, true, "", false, ""); err == nil {
		t.Fatal("an empty project default must fail while projects are enabled")
	}
	if _, err := Resolve(cfg, "", []string{"developer"}, false, true, "", false, ""); err != nil {
		t.Fatalf("projects disabled should allow an empty default: %v", err)
	}
	if _, err := Resolve(cfg, "", nil, true, false, "", false, ""); err != nil {
		t.Fatalf("non-web serve must not validate web defaults: %v", err)
	}
}

func TestResolvePreservesExplicitEmptyCommonThroughGlobals(t *testing.T) {
	cfg := testConfig(t)
	empty := []string{}
	cfg.Web.AgentPicker.Common = &empty
	settings, err := Resolve(cfg, "", []string{"developer", "reviewer"}, false, true, "", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if settings.PickerCommon == nil || len(settings.PickerCommon) != 0 {
		t.Fatalf("explicit empty common = %#v, want a non-nil empty list", settings.PickerCommon)
	}
	snippet := AppendGlobals("", settings.ProjectDefaultAgent, nil, settings.ProjectDefaultAgent, settings.PickerCommon, nil)
	if !strings.Contains(snippet, `TERM_LLM_AGENT_PICKER_COMMON=[]`) {
		t.Fatalf("globals = %q, want an explicit empty list, not null", snippet)
	}
}

func TestResolveHonorsPinnedAgent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Web.ProjectDefaultAgent = "missing-agent"
	settings, err := Resolve(cfg, "pinned", nil, true, true, "", false, "")
	if err != nil {
		t.Fatalf("pinned serve must not validate a picker default: %v", err)
	}
	if settings.ProjectDefaultAgent != "" || settings.ApprovalActions != nil {
		t.Fatalf("pinned settings = %#v", settings)
	}
}

func TestDraftApprovalActionsCoverChatToolfulAndCustom(t *testing.T) {
	cfg := testConfig(t)
	writeTestAgent(t, cfg, "custom-gated", "tools:\n  custom:\n    - name: do_thing\n      description: \"Do a thing\"\n      script: do.sh\n")
	actions := DraftApprovalActions(cfg, "", false, "")
	if actions == nil {
		t.Fatal("expected a capability map")
	}
	if governed, ok := actions[""]; !ok || governed {
		t.Fatalf("no-project Chat capability = %v (known %v), want known no approval actions", governed, ok)
	}
	if governed, ok := actions["developer"]; !ok || !governed {
		t.Fatalf("developer capability = %v (known %v), want known governed", governed, ok)
	}
	if governed, ok := actions["web-researcher"]; !ok || !governed {
		t.Fatalf("read-only agent capability = %v (known %v), want known governed", governed, ok)
	}
	if governed, ok := actions["custom-gated"]; !ok || !governed {
		t.Fatalf("custom-tool agent capability = %v (known %v), want known governed", governed, ok)
	}
	if governed, ok := actions["artist"]; !ok || !governed {
		t.Fatalf("image agent capability = %v (known %v), want known governed", governed, ok)
	}
}

func TestDraftApprovalActionsCountConfiguredToolsForChat(t *testing.T) {
	cfg := testConfig(t)
	cfg.Tools.Enabled = []string{tools.AskUserToolName, tools.ActivateSkillToolName}
	if governed := DraftApprovalActions(cfg, "", false, "")[""]; governed {
		t.Fatal("approval-free chat tools must not be governed")
	}
	cfg.Tools.Enabled = []string{tools.ShellToolName}
	if governed := DraftApprovalActions(cfg, "", false, "")[""]; !governed {
		t.Fatal("a configured shell tool makes Chat approval-governed")
	}
}

func TestDraftApprovalActionsLeaveUndiscoveredMCPUnknown(t *testing.T) {
	cfg := testConfig(t)
	writeTestAgent(t, cfg, "mcp-agent", "mcp:\n  - name: github\n")
	mcpDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.MkdirAll(mcpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mcpDir, "mcp.json"), []byte(`{"servers":{"github":{"command":"true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	actions := DraftApprovalActions(cfg, "", false, "")
	if governed, ok := actions["mcp-agent"]; ok {
		t.Fatalf("undiscovered MCP capability = %v, want unknown without launching a server", governed)
	}
	if governed, ok := actions[""]; !ok || governed {
		t.Fatalf("Chat must not inherit an agent's MCP selection: %v (known %v)", governed, ok)
	}
}
