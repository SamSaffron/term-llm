package contain_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/contain"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestRenderedAgentBootstrapEnablesScriptBundleAndDirectTools(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	image, err := contain.SyncImage("agent", false)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for source, target := range map[string]string{"bootstrap.yaml": "agent.yaml", "system.md": "system.md", "soul.md": "soul.md", "memory/palace.md": "memory/palace.md", "memory/recent.md": "memory/recent.md"} {
		data, err := os.ReadFile(filepath.Join(image.Dir, "bootstrap", source))
		if err != nil {
			t.Fatal(err)
		}
		rendered, err := contain.RenderTemplateContent(data, contain.PlaceholderValues{"AGENT_NAME": "scaffold"})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, target)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, rendered, 0600); err != nil {
			t.Fatal(err)
		}
	}
	agent, err := agents.LoadFromDir(dir, agents.SourceUser)
	if err != nil {
		t.Fatal(err)
	}
	cfg := tools.DefaultToolConfig()
	cfg.Enabled = agent.Tools.Enabled
	cfg.Workspace = "none"
	manager, err := tools.NewToolManager(&cfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ApprovalMgr.Close()
	advertised := map[string]bool{}
	for _, spec := range manager.Registry.GetSpecs() {
		advertised[spec.Name] = true
	}
	for _, name := range []string{"spawn_script", "wait_script", "cancel_script", "read_file", "write_file", "edit_file", "glob", "grep", "shell", "ask_user", "spawn_agent", "view_image", "show_media", "image_generate"} {
		if !advertised[name] || !manager.Registry.IsEnabled(name) {
			t.Errorf("rendered scaffold did not advertise and enable %s through manager", name)
		}
	}
	if advertised["code_mode"] {
		t.Fatal("legacy compatibility tool unexpectedly enabled")
	}
}
