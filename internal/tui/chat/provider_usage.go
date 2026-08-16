package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
)

var fetchProviderUsage = llm.FetchProviderUsageWithAPIKey

type providerUsageDoneMsg struct {
	provider string
	report   *llm.ProviderUsage
	err      error
}

func (m *Model) cmdProviderUsage(args []string) (tea.Model, tea.Cmd) {
	if len(args) != 0 {
		return m.showFooterError("Usage: /usage")
	}
	provider, _ := m.currentProviderAndModel()
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return m.showFooterError("Unable to determine the current provider.")
	}

	apiKey, err := m.resolvedProviderUsageAPIKey(provider)
	if err != nil {
		return m.showFooterError(fmt.Sprintf("Unable to resolve %s credentials: %v", provider, err))
	}
	m.setTextareaValue("")
	parentCtx := m.rootContext()
	return m.showFooterMutedWithCmd("Fetching "+provider+" usage…", func() tea.Msg {
		ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
		defer cancel()
		report, err := fetchProviderUsage(ctx, provider, apiKey)
		return providerUsageDoneMsg{provider: provider, report: report, err: err}
	})
}

func (m *Model) resolvedProviderUsageAPIKey(provider string) (string, error) {
	if provider != "opencode-go" || m.config == nil {
		return "", nil
	}
	providerCfg, err := m.config.GetResolvedProviderConfig(provider)
	if err != nil || providerCfg == nil {
		return "", err
	}
	if err := providerCfg.ResolveForInference(); err != nil {
		return "", err
	}
	return strings.TrimSpace(providerCfg.ResolvedAPIKey), nil
}

func (m *Model) handleProviderUsageDone(msg providerUsageDoneMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		return m.showFooterError(fmt.Sprintf("Unable to fetch %s usage: %v", msg.provider, msg.err))
	}
	width := 0
	if m.dialog != nil {
		width = m.dialog.contentWidth() - 4
	}
	content := llm.FormatProviderUsageWithOptions(msg.report, time.Now(), llm.ProviderUsageFormatOptions{Width: width})
	if strings.TrimSpace(content) == "" {
		return m.showFooterError("Provider returned an empty usage report.")
	}
	m.clearFooterMessage()
	m.dialog.ShowContent("Provider Usage", content)
	m.scrollToBottom = true
	return m, nil
}
