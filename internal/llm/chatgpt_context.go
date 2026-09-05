package llm

import (
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

var chatGPTContextConfig struct {
	sync.RWMutex
	providers map[string]config.ProviderConfig
}

// RegisterChatGPTContextConfig replaces context policy on every config load.
// Keep provider keys separate so aliases do not leak settings into one another.
func RegisterChatGPTContextConfig(providers map[string]config.ProviderConfig) {
	snapshot := make(map[string]config.ProviderConfig)
	for name, pc := range providers {
		if config.InferProviderType(name, pc.Type) == config.ProviderTypeChatGPT {
			pc.ModelConfigs = append([]config.ProviderModelConfig(nil), pc.ModelConfigs...)
			snapshot[name] = pc
		}
	}
	chatGPTContextConfig.Lock()
	chatGPTContextConfig.providers = snapshot
	chatGPTContextConfig.Unlock()
}

// resolveChatGPTContext applies policy to backend facts, without persisting the
// selected budget. Shipped budgets already include headroom: do not subtract
// the model's theoretical output maximum from them a second time.
func resolveChatGPTContext(provider string, info ModelInfo) ModelInfo {
	chatGPTContextConfig.RLock()
	pc := chatGPTContextConfig.providers[provider]
	chatGPTContextConfig.RUnlock()
	id := strings.ToLower(strings.TrimSpace(info.ID))
	// Exact IDs win over suffix stripping (notably gpt-5.1-codex-max).
	base := id
	if _, ok := staticChatGPTModelInfo(id); !ok {
		base, _ = trimKnownEffortSuffix(id)
	}
	context, reserve := pc.ContextWindow, pc.MaxOutputTokens
	for _, mc := range pc.ModelConfigs {
		if strings.EqualFold(mc.ID, id) || strings.EqualFold(mc.ID, base) || (mc.Alias != "" && (strings.EqualFold(mc.Alias, id) || strings.EqualFold(mc.Alias, base))) {
			base = strings.ToLower(mc.ID)
			if mc.ContextWindow > 0 {
				context = mc.ContextWindow
			}
			if mc.MaxOutputTokens > 0 {
				reserve = mc.MaxOutputTokens
			}
			break
		}
	}
	fallback, known := staticChatGPTModelInfo(base)
	maximum := info.MaxContext
	if maximum <= 0 {
		// Bundled maxima from Codex models.json. For other models, retain the
		// established budget but do not allow unverified upward overrides.
		switch base {
		case "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
			maximum = 872_000
		case "gpt-5.4":
			maximum = 1_000_000
		default:
			maximum = firstNonZero(info.BackendContext, info.InputLimit, fallback.InputLimit)
		}
	}
	target := info.InputLimit
	if known {
		target = fallback.InputLimit
	}
	if target <= 0 {
		target = info.BackendContext
	}
	info.RecommendedContext = target
	if maximum > 0 && info.RecommendedContext > maximum {
		info.RecommendedContext = maximum
	}
	if context > 0 && maximum > 0 {
		target = context
	}
	if maximum > 0 && target > maximum {
		target = maximum
	}
	info.MaxContext = maximum
	info.ConfiguredContext = target
	if context > 0 && reserve > 0 {
		target -= reserve
		// Never turn off compaction due to an excessive output reservation.
		if target < 1 {
			target = 1
		}
	}
	info.InputLimit = target
	return info
}

func resolveChatGPTModels(provider string, models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, len(models))
	for i, model := range models {
		out[i] = resolveChatGPTContext(provider, model)
	}
	return out
}

// chatGPTContextModelID resolves configured aliases before looking up account facts.
func chatGPTContextModelID(provider, model string) string {
	chatGPTContextConfig.RLock()
	pc := chatGPTContextConfig.providers[provider]
	chatGPTContextConfig.RUnlock()
	id := strings.ToLower(strings.TrimSpace(model))
	for _, mc := range pc.ModelConfigs {
		if strings.EqualFold(mc.ID, id) || (mc.Alias != "" && strings.EqualFold(mc.Alias, id)) {
			return mc.ID
		}
	}
	base, _ := trimKnownEffortSuffix(id)
	for _, mc := range pc.ModelConfigs {
		if strings.EqualFold(mc.ID, base) || (mc.Alias != "" && strings.EqualFold(mc.Alias, base)) {
			return mc.ID
		}
	}
	return id
}
