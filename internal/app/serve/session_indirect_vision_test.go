package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
)

func indirectVisionTestConfig() *config.Config {
	return &config.Config{
		DefaultProvider: "text",
		Providers: map[string]config.ProviderConfig{
			"text": {
				Model: "tiny-text",
				ModelConfigs: []config.ProviderModelConfig{{
					ID:        "tiny-text",
					VisionVia: "debug:vision-model",
				}},
			},
		},
	}
}

func TestNewVisionProviderForTargetUsesProviderDefaultModel(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"debug": {Model: "default-vision-model"},
		},
	}
	provider, model, err := newVisionProviderForTarget(cfg, "debug")
	if err != nil {
		t.Fatalf("newVisionProviderForTarget: %v", err)
	}
	if provider == nil {
		t.Fatal("provider = nil")
	}
	if model != "default-vision-model" {
		t.Fatalf("model = %q, want default-vision-model", model)
	}
}

func TestSetupToolManagerAutoEnablesIndirectVisionViewImage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := indirectVisionTestConfig()
	engine := engine.NewEngine(mockprovider.NewMockProvider("primary"), nil)
	settings := SessionSettings{Provider: "text", Model: "tiny-text"}

	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager: %v", err)
	}
	if toolMgr == nil {
		t.Fatal("toolMgr = nil, want auto-enabled view_image manager")
	}
	if !engine.IndirectVision() {
		t.Fatal("engine indirect vision not enabled")
	}
	if _, ok := toolMgr.Registry.Get(tools.ViewImageToolName); !ok {
		t.Fatal("view_image not registered in local registry")
	}
	if _, ok := engine.Tools().Get(tools.ViewImageToolName); !ok {
		t.Fatal("view_image not registered with engine")
	}
}

func TestIndirectVisionUploadsPermissionScopedToViewImage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg := indirectVisionTestConfig()
	engine := engine.NewEngine(mockprovider.NewMockProvider("primary"), nil)
	settings := SessionSettings{Provider: "text", Model: "tiny-text", Tools: tools.ReadFileToolName}

	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager: %v", err)
	}
	uploadsDir := uploadsReadDir()
	if uploadsDir == "" {
		t.Fatal("uploadsReadDir returned empty")
	}
	uploadPath := filepath.Join(uploadsDir, "secret.png")
	if err := os.WriteFile(uploadPath, []byte("not really an image"), 0o600); err != nil {
		t.Fatalf("write upload fixture: %v", err)
	}

	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ViewImageToolName, uploadPath, uploadPath, false); err != nil || outcome == tools.Cancel {
		t.Fatalf("view_image upload approval = %v, %v; want allowed", outcome, err)
	}
	if outcome, err := toolMgr.ApprovalMgr.CheckPathApproval(tools.ReadFileToolName, uploadPath, uploadPath, false); err == nil || outcome != tools.Cancel {
		t.Fatalf("read_file upload approval = %v, %v; want denied", outcome, err)
	}

	readTool, ok := toolMgr.Registry.Get(tools.ReadFileToolName)
	if !ok {
		t.Fatal("read_file not registered")
	}
	args, _ := json.Marshal(map[string]string{"path": uploadPath})
	out, err := readTool.Execute(t.Context(), args)
	if err != nil {
		t.Fatalf("read_file Execute error: %v", err)
	}
	if !strings.Contains(out.Content, "PERMISSION_DENIED") {
		t.Fatalf("read_file output = %q, want permission error", out.Content)
	}
}

func TestAlignSettingsToActiveProviderUsesResolvedConfig(t *testing.T) {
	cfg := indirectVisionTestConfig()
	cfg.DefaultProvider = "text"
	cfg.Providers["text"] = config.ProviderConfig{Model: "resolved-model"}
	settings := SessionSettings{Provider: "stale", Model: "stale-model"}

	AlignSettingsToActiveProvider(&settings, cfg, mockprovider.NewMockProvider("primary"))

	if settings.Provider != "text" || settings.Model != "resolved-model" {
		t.Fatalf("settings provider/model = %q/%q, want text/resolved-model", settings.Provider, settings.Model)
	}
}
