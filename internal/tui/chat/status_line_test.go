package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestRenderStatusLineShowsConfiguredContextWindow(t *testing.T) {
	llm.RegisterConfigLimits([]llm.ConfigModelLimit{{Provider: "mock", Model: "mock-model", InputLimit: 1048576}})
	defer llm.RegisterConfigLimits(nil)

	m := newTestChatModel(false)
	m.width = 120
	m.engine.ConfigureContextManagement(m.provider, m.providerKey, m.modelName, false)

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "~0/1M") {
		t.Fatalf("status line %q does not show context window", line)
	}
}

func TestRenderStatusLineShowsExactlyOneApprovalMode(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 120
	approvalMgr := tools.NewApprovalManager(tools.NewToolPermissions())
	m.SetApprovalManager(approvalMgr)

	approvalMgr.SetApprovalMode(tools.ModeAuto)
	m.yolo = true // stale local bool must not make status show both modes
	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "auto") {
		t.Fatalf("status line %q does not show auto mode", line)
	}
	if strings.Contains(line, "yolo") {
		t.Fatalf("status line %q shows yolo while effective mode is auto", line)
	}

	approvalMgr.SetApprovalMode(tools.ModeYolo)
	line = ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "yolo") {
		t.Fatalf("status line %q does not show yolo mode", line)
	}
	if strings.Contains(line, "auto") {
		t.Fatalf("status line %q shows auto while effective mode is yolo", line)
	}
}

func TestRenderStatusLinePrefersConfiguredModelAlias(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 120
	m.providerKey = "cdck_deepseek"
	m.providerName = "Cdck_deepseek"
	m.modelName = "Qwen/Qwen3.5-122B-A10B"
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{
		"cdck_deepseek": {
			Model: "Qwen/Qwen3.5-122B-A10B",
			ModelConfigs: []config.ProviderModelConfig{{
				ID:    "Qwen/Qwen3.5-122B-A10B",
				Alias: "deepseek-v4-flash",
			}},
		},
	}}

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "deepseek-v4-flash") {
		t.Fatalf("status line %q does not show configured alias", line)
	}
	if strings.Contains(line, "Qwen/Qwen3.5-122B-A10B") {
		t.Fatalf("status line %q shows upstream model id despite configured alias", line)
	}
}

func TestRenderStatusLinePrefersConfiguredModelAliasForAppliedStreamingEffort(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 120
	m.providerKey = "custom"
	m.providerName = "custom"
	m.modelName = "upstream/model-medium"
	m.streaming = true
	m.pendingStreamModelSwitch = &pendingStreamModelSwitch{provider: "custom", model: "upstream/model-high", applied: true}
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{
		"custom": {
			ModelConfigs: []config.ProviderModelConfig{{
				ID:               "upstream/model",
				Alias:            "friendly",
				ReasoningEfforts: []string{"medium", "high"},
			}},
		},
	}}

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "friendly-high") {
		t.Fatalf("status line %q does not show alias for applied effort model", line)
	}
	if strings.Contains(line, "upstream/model-high") || strings.Contains(line, "friendly-medium") {
		t.Fatalf("status line %q shows stale or upstream model", line)
	}
}

func TestRenderStatusLineShowsBoundWorktree(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 120
	m.sess = &session.Session{WorktreeDir: "/tmp/term-llm-worktrees/feature-branch"}

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "wt:feature-branch") {
		t.Fatalf("status line %q does not show bound worktree", line)
	}
}
