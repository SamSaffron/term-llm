package chat

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestCmdProviderUsageFetchesCurrentProviderAndOpensDialog(t *testing.T) {
	originalFetch := fetchProviderUsage
	defer func() { fetchProviderUsage = originalFetch }()
	fetchProviderUsage = func(_ context.Context, provider, apiKey string) (*llm.ProviderUsage, error) {
		if provider != "chatgpt" {
			t.Fatalf("provider = %q, want chatgpt", provider)
		}
		if apiKey != "" {
			t.Fatalf("apiKey = %q, want empty for chatgpt", apiKey)
		}
		return &llm.ProviderUsage{
			Provider: "chatgpt",
			Plan:     "pro",
			Limits: []llm.ProviderUsageLimit{{
				ID:   "codex",
				Name: "Codex",
				PrimaryWindow: &llm.ProviderUsageWindow{
					UsedPercent:     31,
					DurationMinutes: 300,
				},
			}},
		}, nil
	}

	m := newCmdTestModel(&mockStore{})
	m.providerKey = "chatgpt"
	m.setTextareaValue("/usage")
	updated, cmd := m.ExecuteCommand("/usage")
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("/usage returned no command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("/usage command = %T, want non-empty tea.BatchMsg", cmd())
	}
	msg := batch[0]()
	updated, _ = m.Update(msg)
	m = updated.(*Model)
	if !m.dialog.IsOpen() || m.dialog.Type() != DialogContent {
		t.Fatalf("usage dialog not open: open=%v type=%v", m.dialog.IsOpen(), m.dialog.Type())
	}
	content := strings.Join(m.dialog.contentLines, "\n")
	for _, want := range []string{"ChatGPT Codex", "Pro plan", "5 hour window\n  ███████░░░░░░░░░░░░░░░  31% used"} {
		if !strings.Contains(content, want) {
			t.Fatalf("dialog missing %q:\n%s", want, content)
		}
	}
}

func TestCmdProviderUsageOpenCodeGoUsesConfiguredKey(t *testing.T) {
	originalFetch := fetchProviderUsage
	defer func() { fetchProviderUsage = originalFetch }()
	fetchProviderUsage = func(_ context.Context, provider, apiKey string) (*llm.ProviderUsage, error) {
		if provider != "opencode-go" || apiKey != "configured-key" {
			t.Fatalf("fetch args = provider %q, apiKey %q", provider, apiKey)
		}
		return &llm.ProviderUsage{
			Provider: "opencode-go",
			Limits: []llm.ProviderUsageLimit{{
				Name:          "Weekly usage",
				PrimaryWindow: &llm.ProviderUsageWindow{Label: "1 week window", UsedPercent: 20},
			}},
		}, nil
	}

	m := newCmdTestModel(&mockStore{})
	m.providerKey = "opencode-go"
	m.config = &config.Config{Providers: map[string]config.ProviderConfig{
		"opencode-go": {Type: config.ProviderTypeOpenCodeGo, ResolvedAPIKey: "configured-key"},
	}}
	updated, cmd := m.ExecuteCommand("/usage")
	m = updated.(*Model)
	batch := cmd().(tea.BatchMsg)
	updated, _ = m.Update(batch[0]())
	m = updated.(*Model)
	content := strings.Join(m.dialog.contentLines, "\n")
	for _, want := range []string{"OpenCode Go", "Weekly usage", "20% used"} {
		if !strings.Contains(content, want) {
			t.Fatalf("dialog missing %q:\n%s", want, content)
		}
	}
}

func TestCmdProviderUsageReportsUnsupportedCurrentProvider(t *testing.T) {
	originalFetch := fetchProviderUsage
	defer func() { fetchProviderUsage = originalFetch }()
	fetchProviderUsage = func(ctx context.Context, provider, _ string) (*llm.ProviderUsage, error) {
		return llm.FetchProviderUsage(ctx, provider)
	}

	m := newCmdTestModel(&mockStore{})
	m.providerKey = "openai"
	updated, cmd := m.ExecuteCommand("/usage")
	m = updated.(*Model)
	batch := cmd().(tea.BatchMsg)
	updated, _ = m.Update(batch[0]())
	m = updated.(*Model)
	if !strings.Contains(m.footerMessage, "not supported") {
		t.Fatalf("footer = %q", m.footerMessage)
	}
}
