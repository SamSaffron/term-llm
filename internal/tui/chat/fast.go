package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

type serviceTierOverride int

const (
	serviceTierInherit serviceTierOverride = iota
	serviceTierClear
	serviceTierFast
)

func (m *Model) providerType() config.ProviderType {
	if m == nil {
		return ""
	}
	providerKey := strings.TrimSpace(m.providerKey)
	if m.config != nil {
		if pc, ok := m.config.Providers[providerKey]; ok {
			return config.InferProviderType(providerKey, pc.Type)
		}
	}
	return config.InferProviderType(providerKey, "")
}

func (m *Model) isChatGPTProvider() bool {
	return m != nil && m.providerType() == config.ProviderTypeChatGPT
}

func (m *Model) supportsServiceTierToggle() bool {
	if m == nil {
		return false
	}
	switch m.providerType() {
	case config.ProviderTypeChatGPT, config.ProviderTypeOpenAI:
		return true
	default:
		return false
	}
}

func (m *Model) canUseFastMetadata() bool {
	return m != nil && m.isChatGPTProvider()
}

func (m *Model) currentModelSupportsFast() bool {
	if m == nil || !m.canUseFastMetadata() || !m.fastMetadataLoaded {
		return false
	}
	return chatModelSupportsFast(m.modelMetadata, m.modelName)
}

func chatModelSupportsFast(models []llm.ModelInfo, modelName string) bool {
	modelName, _ = llm.ParseModelEffort(strings.TrimSpace(modelName))
	for _, model := range models {
		id, _ := llm.ParseModelEffort(strings.TrimSpace(model.ID))
		if id == modelName && llm.ModelSupportsFast(model) {
			return true
		}
	}
	return false
}

func (m *Model) configuredFastDefault() bool {
	if m == nil || m.config == nil {
		return false
	}
	pc, ok := m.config.Providers[strings.TrimSpace(m.providerKey)]
	if !ok {
		return false
	}
	return llm.NormalizeServiceTier(pc.ServiceTier) == llm.ServiceTierFast
}

func (m *Model) refreshEffectiveFastMode() {
	if m == nil {
		return
	}
	switch m.fastOverride {
	case serviceTierFast:
		m.fastMode = true
	case serviceTierClear:
		m.fastMode = false
	default:
		m.fastMode = m.fastProviderDefault
	}
}

func (m *Model) currentServiceTier() (string, bool) {
	if m == nil {
		return "", false
	}
	switch m.fastOverride {
	case serviceTierFast:
		return llm.ServiceTierFast, true
	case serviceTierClear:
		return "", true
	default:
		// Omitted means "use the provider default". If there is no configured
		// service_tier, the provider sends no service_tier field.
		return "", false
	}
}

func (m *Model) loadChatGPTModelsCmd() tea.Cmd {
	if m == nil || !m.canUseFastMetadata() || m.provider == nil {
		return nil
	}
	m.fastMetadataLoading = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if provider, ok := m.provider.(interface {
			ListModelsWithFreshness(context.Context) ([]llm.ModelInfo, bool, error)
		}); ok {
			models, fresh, err := provider.ListModelsWithFreshness(ctx)
			return chatGPTModelsLoadedMsg{models: models, fresh: fresh, err: err}
		}
		provider, ok := m.provider.(interface {
			ListModels(context.Context) ([]llm.ModelInfo, error)
		})
		if !ok {
			return chatGPTModelsLoadedMsg{err: fmt.Errorf("provider does not expose model metadata")}
		}
		models, err := provider.ListModels(ctx)
		return chatGPTModelsLoadedMsg{models: models, fresh: true, err: err}
	}
}

func (m *Model) applyChatGPTModelsLoaded(msg chatGPTModelsLoadedMsg) (tea.Model, tea.Cmd) {
	m.fastMetadataLoading = false
	if msg.err != nil {
		if m.pendingFastToggle {
			m.pendingFastToggle = false
			return m.showFooterError(fmt.Sprintf("Could not load model metadata: %v", msg.err))
		}
		return m, nil
	}
	m.modelMetadata = msg.models
	m.fastMetadataLoaded = true
	m.fastMetadataStale = !msg.fresh
	m.refreshEffectiveFastMode()

	if m.pendingFastToggle {
		m.pendingFastToggle = false
		return m.cmdFast()
	}
	return m, nil
}
