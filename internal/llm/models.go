package llm

import (
	"sort"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// ModelEntry describes a model available through a specific provider.
// InputLimit and OutputLimit are the effective token budgets for compaction
// and output clamping. A value of 0 means "unknown — fall back to prefix
// tables in context_window.go".
type ModelEntry struct {
	ID               string
	InputLimit       int      // effective input budget (context - output reserve)
	OutputLimit      int      // max output tokens
	ReasoningEfforts []string // supported suffix-based reasoning-effort aliases (e.g. low, medium, high)
}

// ProviderModels contains curated fallback model entries for providers without
// dynamic discovery or for offline/default UIs. Dynamic providers such as Copilot
// may intentionally omit entries and populate model IDs from their live cache.
// When adding a curated model, include InputLimit/OutputLimit if known.
var ProviderModels = map[string][]ModelEntry{
	"anthropic": {
		// Claude 4.x/5. Reasoning-effort metadata is provider-level below:
		// Fable and Opus support low/medium/high/xhigh/max; Sonnet supports low/medium/high.
		// 1M-capable current models use 980K effective input (1M minus reserve).
		{ID: "claude-fable-5", InputLimit: 980_000, OutputLimit: 128_000},
		{ID: "claude-opus-4-8", InputLimit: 980_000, OutputLimit: 128_000},
		{ID: "claude-opus-4-7", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-5-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4", InputLimit: 180_000, OutputLimit: 64_000},
	},
	"openai": {
		{ID: "gpt-5.5", InputLimit: 922_000, OutputLimit: 128_000},
		{ID: "gpt-5.4", InputLimit: 922_000, OutputLimit: 128_000},
		{ID: "gpt-5.4-mini", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.4-nano", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.3-codex", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.2-codex", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.2", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.1", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5-mini", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5-nano", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "o3-mini", InputLimit: 100_000, OutputLimit: 100_000},
	},
	"chatgpt": {
		// Uses ChatGPT backend API with native OAuth
		{ID: "gpt-5.5", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.4", InputLimit: 922_000, OutputLimit: 128_000},
		{ID: "gpt-5.4-mini", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.3-codex", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.3-codex-spark", InputLimit: 100_000, OutputLimit: 16_000},
		{ID: "gpt-5.2-codex", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.2", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.1-codex-max", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.1-codex", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.1-codex-mini", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.1", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5-codex", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5-codex-mini", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5", InputLimit: 272_000, OutputLimit: 128_000},
	},
	"openrouter": {
		{ID: "x-ai/grok-code-fast-1"},
	},
	"gemini": {
		{ID: "gemini-3-pro-preview", InputLimit: 936_000, OutputLimit: 65_536},
		{ID: "gemini-3-pro-preview-thinking", InputLimit: 936_000, OutputLimit: 65_536},
		{ID: "gemini-3-flash-preview", InputLimit: 983_000, OutputLimit: 65_536},
		{ID: "gemini-3-flash-preview-thinking", InputLimit: 983_000, OutputLimit: 65_536},
		{ID: "gemini-2.5-flash", InputLimit: 983_000, OutputLimit: 65_536},
		{ID: "gemini-2.5-flash-lite", InputLimit: 983_000, OutputLimit: 65_536},
	},
	"gemini-cli": {
		{ID: "gemini-3-pro-preview", InputLimit: 936_000, OutputLimit: 65_536},
		{ID: "gemini-3-pro-preview-thinking", InputLimit: 936_000, OutputLimit: 65_536},
		{ID: "gemini-3-flash-preview", InputLimit: 983_000, OutputLimit: 65_536},
		{ID: "gemini-3-flash-preview-thinking", InputLimit: 983_000, OutputLimit: 65_536},
		{ID: "gemini-2.5-flash", InputLimit: 983_000, OutputLimit: 65_536},
		{ID: "gemini-2.5-flash-lite", InputLimit: 983_000, OutputLimit: 65_536},
	},
	"zen": {
		{ID: "minimax-m2.5-free", InputLimit: 168_000, OutputLimit: 32_000},
		{ID: "big-pickle", InputLimit: 168_000, OutputLimit: 32_000},
		{ID: "gpt-5-nano", InputLimit: 96_000, OutputLimit: 32_000},
		{ID: "nemotron-3-super-free", InputLimit: 96_000, OutputLimit: 32_000},
		{ID: "trinity-large-preview-free", InputLimit: 96_000, OutputLimit: 32_000},
		{ID: "qwen3.6-plus-free", InputLimit: 900_000, OutputLimit: 100_000},
	},
	"claude-bin": {
		// Aliases resolved internally by claude-bin provider
		{ID: "opus", ReasoningEfforts: claudeBinOpusEffortVariants},
		{ID: "opus-low"},
		{ID: "opus-medium"},
		{ID: "opus-high"},
		{ID: "opus-xhigh"},
		{ID: "opus-max"},
		{ID: "sonnet", ReasoningEfforts: claudeBinSonnetEffortVariants},
		{ID: "sonnet-low"},
		{ID: "sonnet-medium"},
		{ID: "sonnet-high"},
		{ID: "fable", ReasoningEfforts: claudeBinFableEffortVariants},
		{ID: "fable-low"},
		{ID: "fable-medium"},
		{ID: "fable-high"},
		{ID: "fable-xhigh"},
		{ID: "fable-max"},
		{ID: "haiku"},
	},
	"grok-bin": {
		{ID: "grok-4.5", ReasoningEfforts: grokBinEffortVariants},
		{ID: "grok-4.5-low"},
		{ID: "grok-4.5-medium"},
		{ID: "grok-4.5-high"},
		{ID: "grok-4.5-xhigh"},
		{ID: "grok-composer-2.5-fast"},
	},
	"xai": {
		// Grok 4.1 (latest, 2M context)
		{ID: "grok-4-1-fast", InputLimit: 1_970_000, OutputLimit: 32_000},
		{ID: "grok-4-1-fast-reasoning", InputLimit: 1_970_000, OutputLimit: 32_000},
		{ID: "grok-4-1-fast-non-reasoning", InputLimit: 1_970_000, OutputLimit: 32_000},
		// Grok 4 (256K context)
		{ID: "grok-4", InputLimit: 192_000, OutputLimit: 64_000},
		{ID: "grok-4-fast-reasoning", InputLimit: 192_000, OutputLimit: 64_000},
		{ID: "grok-4-fast-non-reasoning", InputLimit: 192_000, OutputLimit: 64_000},
		// Grok 3 (131K context)
		{ID: "grok-3", InputLimit: 123_000, OutputLimit: 8_192},
		{ID: "grok-3-fast", InputLimit: 123_000, OutputLimit: 8_192},
		{ID: "grok-3-mini", InputLimit: 123_000, OutputLimit: 8_192},
		{ID: "grok-3-mini-fast", InputLimit: 123_000, OutputLimit: 8_192},
		// Specialized
		{ID: "grok-code-fast-1", InputLimit: 246_000, OutputLimit: 16_384},
		// Grok 2
		{ID: "grok-2", InputLimit: 123_000, OutputLimit: 8_192},
	},
	"ollama": {
		// Qwen3 coding/agent (think suffix enables extended reasoning via -think flag)
		{ID: "qwen2.5-coder:7b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen2.5-coder:14b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen2.5-coder:32b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen3:8b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen3:8b-think", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen3:14b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen3:14b-think", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen3:32b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "qwen3:32b-think", InputLimit: 30_000, OutputLimit: 8_192},
		// Llama
		{ID: "llama3.3:70b", InputLimit: 120_000, OutputLimit: 8_192},
		{ID: "llama3.2:3b", InputLimit: 120_000, OutputLimit: 8_192},
		{ID: "llama3.2:1b", InputLimit: 120_000, OutputLimit: 8_192},
		// DeepSeek-R1 (think is always on)
		{ID: "deepseek-r1:7b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "deepseek-r1:14b", InputLimit: 30_000, OutputLimit: 8_192},
		{ID: "deepseek-r1:32b", InputLimit: 30_000, OutputLimit: 8_192},
	},
	"bedrock": {
		// AWS Bedrock: same friendly names as anthropic (translated to Bedrock IDs internally)
		// and the same reasoning-effort defaults as anthropic/claude-bin.
		{ID: "claude-opus-4-8", InputLimit: 980_000, OutputLimit: 128_000},
		{ID: "claude-opus-4-7", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-5-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4", InputLimit: 180_000, OutputLimit: 64_000},
	},
	"venice": {},
	"nearai": {
		// TEE-hosted text models synced from NEAR AI Cloud /model/list on 2026-05-21.
		// Run `term-llm models --provider nearai` for the full live catalog.
		{ID: "zai-org/GLM-5.1-FP8", InputLimit: 202_752},
		{ID: "Qwen/Qwen3.6-35B-A3B-FP8", InputLimit: 262_144},
		{ID: "Qwen/Qwen3-VL-30B-A3B-Instruct", InputLimit: 256_000},
		{ID: "Qwen/Qwen3-30B-A3B-Instruct-2507", InputLimit: 262_144},
		{ID: "openai/gpt-oss-120b", InputLimit: 131_000},
		{ID: "google/gemma-4-31B-it", InputLimit: 262_144},
	},
	"sambanova": {
		// SambaCloud-hosted models on RDU. Limits/prices synced from:
		// https://docs.sambanova.ai/docs/en/models/sambacloud-models
		// https://cloud.sambanova.ai/plans/pricing
		{ID: "gpt-oss-120b", InputLimit: 131_072, OutputLimit: 8_192},
		{ID: "MiniMax-M2.7", InputLimit: 192_000, OutputLimit: 8_192},
		{ID: "DeepSeek-V3.1", InputLimit: 128_000, OutputLimit: 8_192},
		{ID: "Meta-Llama-3.3-70B-Instruct", InputLimit: 128_000, OutputLimit: 8_192},
		{ID: "DeepSeek-V3.2", InputLimit: 32_000, OutputLimit: 8_192},
		{ID: "gemma-3-12b-it", InputLimit: 128_000, OutputLimit: 8_192},
		{ID: "Llama-4-Maverick-17B-128E-Instruct", InputLimit: 128_000, OutputLimit: 8_192},
	},
}

// providerModelPricing contains curated provider-specific prices in USD per
// million input/output tokens for providers whose /models APIs do not include
// pricing metadata.
type modelPricing struct {
	InputPrice  float64
	OutputPrice float64
}

var providerModelPricing = map[string]map[string]modelPricing{
	"nearai": {
		// Synced from https://cloud-api.near.ai/v1/model/list on 2026-05-21.
		"zai-org/GLM-5.1-FP8":              {InputPrice: 0.85, OutputPrice: 3.30},
		"Qwen/Qwen3.6-35B-A3B-FP8":         {InputPrice: 0.17, OutputPrice: 1.10},
		"Qwen/Qwen3-VL-30B-A3B-Instruct":   {InputPrice: 0.15, OutputPrice: 0.55},
		"Qwen/Qwen3-30B-A3B-Instruct-2507": {InputPrice: 0.15, OutputPrice: 0.55},
		"openai/gpt-oss-120b":              {InputPrice: 0.15, OutputPrice: 0.55},
		"google/gemma-4-31B-it":            {InputPrice: 0.13, OutputPrice: 0.40},
	},
	"sambanova": {
		// Synced from https://cloud.sambanova.ai/plans/pricing on 2026-05-21.
		"DeepSeek-R1-Distill-Llama-70B":      {InputPrice: 0.70, OutputPrice: 1.40},
		"DeepSeek-V3.1-cb":                   {InputPrice: 0.15, OutputPrice: 0.75},
		"DeepSeek-V3.1":                      {InputPrice: 3.00, OutputPrice: 4.50},
		"DeepSeek-V3.2":                      {InputPrice: 3.00, OutputPrice: 4.50},
		"gemma-3-12b-it":                     {InputPrice: 0.35, OutputPrice: 0.59},
		"gpt-oss-120b":                       {InputPrice: 0.22, OutputPrice: 0.59},
		"Llama-4-Maverick-17B-128E-Instruct": {InputPrice: 0.63, OutputPrice: 1.80},
		"Meta-Llama-3.3-70B-Instruct":        {InputPrice: 0.60, OutputPrice: 1.20},
		"MiniMax-M2.7":                       {InputPrice: 0.60, OutputPrice: 2.40},
	},
}

// PricingForProviderModel returns known provider-specific pricing in USD per
// million input/output tokens.
func PricingForProviderModel(provider, model string) (inputPrice, outputPrice float64, ok bool) {
	provider = resolveProviderType(provider)
	if byModel := providerModelPricing[provider]; byModel != nil {
		if pricing, found := byModel[model]; found {
			return pricing.InputPrice, pricing.OutputPrice, true
		}
	}
	return 0, 0, false
}

// ProviderModelIDs returns model IDs for a built-in provider.
// Copilot is populated from the latest live model-list cache instead of a
// hardcoded list. For callers that might receive a custom alias name, use
// ResolveProviderModelIDs.
func ProviderModelIDs(provider string) []string {
	if resolveProviderType(provider) == "copilot" {
		return GetCachedCopilotModels()
	}
	entries := ProviderModels[provider]
	if entries == nil {
		return nil
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// ResolveProviderModelIDs returns curated model IDs for a provider, resolving
// custom aliases (e.g., "acme" → "venice") via registered provider aliases
// and built-in type inference.
func ResolveProviderModelIDs(name string) []string {
	if ids := ProviderModelIDs(name); len(ids) > 0 {
		return ids
	}
	// Resolve via alias or type inference
	resolved := resolveProviderType(name)
	if resolved != name {
		return ProviderModelIDs(resolved)
	}
	return nil
}

// ProviderFastModels contains the default lightweight model for each provider type.
// These are used for short control-plane tasks (interrupt classification, summarization).
var ProviderFastModels = config.DefaultProviderFastModels()

var ImageProviderModels = map[string][]string{
	"debug":      {"random"},
	"gemini":     {"gemini-2.5-flash-image", "gemini-3-pro-image-preview", "gemini-3.1-flash-image-preview"},
	"openai":     {"gpt-image-2", "gpt-image-1.5", "gpt-image-1-mini"},
	"chatgpt":    {"gpt-5.4-mini", "gpt-5.4"},
	"xai":        {"grok-2-image", "grok-2-image-1212"},
	"venice":     {"nano-banana-pro", "nano-banana-2", "flux-2-pro", "flux-2-max", "gpt-image-1-5", "imagineart-1.5-pro", "recraft-v4", "recraft-v4-pro", "seedream-v4", "seedream-v5-lite", "qwen-image", "qwen-image-2", "qwen-image-2-pro", "grok-imagine-image", "grok-imagine-image-pro", "hunyuan-image-v3", "venice-sd35", "hidream", "chroma", "z-image-turbo", "wan-2-7-text-to-image", "wan-2-7-pro-text-to-image", "lustify-sdxl", "lustify-v7", "lustify-v8", "wai-Illustrious", "bria-bg-remover", "qwen-edit", "nano-banana-pro-edit", "nano-banana-2-edit", "flux-2-max-edit", "gpt-image-1-5-edit", "seedream-v4-edit", "seedream-v5-lite-edit", "qwen-image-2-edit", "qwen-image-2-pro-edit", "grok-imagine-edit", "firered-image-edit"},
	"flux":       {"flux-2-pro", "flux-kontext-pro", "flux-2-max"},
	"openrouter": {"google/gemini-2.5-flash-image", "google/gemini-3-pro-image-preview", "openai/gpt-5-image", "openai/gpt-5-image-mini", "bytedance-seed/seedream-4.5", "black-forest-labs/flux.2-pro"},
}

// defaultEffortVariants are the standard effort levels for GPT-5-family
// suffix aliases. This is deliberately not the union of all known suffixes:
// GPT-5 models do not support "max".
var defaultEffortVariants = []string{"minimal", "low", "medium", "high", "xhigh"}

var claudeBinOpusEffortVariants = []string{"low", "medium", "high", "xhigh", "max"}
var claudeBinSonnetEffortVariants = []string{"low", "medium", "high"}
var grokBinEffortVariants = []string{"low", "medium", "high", "xhigh"}

// Fable 5 supports the same effort levels as Opus.
var claudeBinFableEffortVariants = claudeBinOpusEffortVariants

func DefaultReasoningEffortsForProviderType(providerType string) []string {
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case "vllm":
		return []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	default:
		return nil
	}
}

// EffortVariantsFor returns the legacy provider-agnostic effort suffixes for a
// model, or nil if none. Prefer ReasoningEffortsForProviderModel when provider
// context is available, because effort support is model- and provider-specific.
//
// This compatibility helper intentionally preserves the historical GPT-5
// heuristic used by older tests and callers that have only a bare model name.
func EffortVariantsFor(model string) []string {
	name := model
	if i := strings.LastIndex(model, "/"); i >= 0 {
		name = model[i+1:]
	}
	if strings.HasPrefix(name, "gpt-5") {
		return cloneEfforts(defaultEffortVariants)
	}
	return nil
}

func resolveProviderModelEntries(provider string) []ModelEntry {
	if entries := ProviderModels[provider]; len(entries) > 0 {
		return entries
	}
	resolved := resolveProviderType(provider)
	if resolved != provider {
		return ProviderModels[resolved]
	}
	return nil
}

func reasoningEffortsForProviderBaseModel(provider, baseModel string) []string {
	for _, entry := range resolveProviderModelEntries(provider) {
		if entry.ID != baseModel {
			continue
		}
		if len(entry.ReasoningEfforts) > 0 {
			return cloneEfforts(entry.ReasoningEfforts)
		}
		return defaultReasoningEffortsForProviderModel(provider, baseModel)
	}
	return defaultReasoningEffortsForProviderModel(provider, baseModel)
}

func defaultReasoningEffortsForProviderModel(provider, model string) []string {
	providerType := resolveProviderType(strings.ToLower(strings.TrimSpace(provider)))
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	name := model
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	nameLower := strings.ToLower(name)

	switch providerType {
	case "openai", "chatgpt", "copilot":
		if strings.HasPrefix(nameLower, "gpt-5") && !strings.HasSuffix(nameLower, "-codex-max") {
			return cloneEfforts(defaultEffortVariants)
		}
	case "anthropic", "bedrock":
		if isClaudeOpusModelName(nameLower) || isClaudeFableModelName(nameLower) {
			return cloneEfforts(claudeBinOpusEffortVariants)
		}
		if isClaudeSonnetModelName(nameLower) {
			return cloneEfforts(claudeBinSonnetEffortVariants)
		}
	case "grok-bin":
		if base, hasSuffix := trimKnownEffortSuffix(nameLower); strings.HasPrefix(base, "grok-4") && !hasSuffix {
			return cloneEfforts(grokBinEffortVariants)
		}
	case "claude-bin":
		if nameLower == "opus" || nameLower == "fable" {
			return cloneEfforts(claudeBinOpusEffortVariants)
		}
		if nameLower == "sonnet" {
			return cloneEfforts(claudeBinSonnetEffortVariants)
		}
	}
	return nil
}

func isClaudeOpusModelName(name string) bool {
	return strings.HasPrefix(name, "claude-opus-4")
}

func isClaudeSonnetModelName(name string) bool {
	return strings.HasPrefix(name, "claude-sonnet-4")
}

func isClaudeFableModelName(name string) bool {
	return strings.HasPrefix(name, "claude-fable-5")
}

func cloneEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	return append([]string(nil), efforts...)
}

// BaseModelAndEffortForProvider returns the switchable base model and current
// reasoning effort for provider/model when the suffix is explicitly supported
// by that provider's model metadata. Unknown suffix-like endings are preserved
// as part of the base model name (for example, gpt-5.1-codex-max is not parsed
// as effort=max for GPT-5 providers because GPT-5 does not support max).
func BaseModelAndEffortForProvider(provider, model string) (base string, effort string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", ""
	}

	if cfgBase, cfgEffort, _ := configBaseModelAndEffortForProvider(provider, model); cfgBase != "" {
		return cfgBase, cfgEffort
	}

	entries := resolveProviderModelEntries(provider)
	for _, entry := range entries {
		if entry.ID == model && len(reasoningEffortsForEntry(provider, entry)) > 0 {
			return model, ""
		}
	}
	for _, entry := range entries {
		efforts := reasoningEffortsForEntry(provider, entry)
		if len(efforts) == 0 {
			continue
		}
		for _, effort := range efforts {
			if model == entry.ID+"-"+effort {
				return entry.ID, effort
			}
		}
	}
	if base, effort, ok := defaultBaseModelAndEffortForProvider(provider, model); ok {
		return base, effort
	}
	return model, ""
}

func reasoningEffortsForEntry(provider string, entry ModelEntry) []string {
	if len(entry.ReasoningEfforts) > 0 {
		return cloneEfforts(entry.ReasoningEfforts)
	}
	return defaultReasoningEffortsForProviderModel(provider, entry.ID)
}

func defaultBaseModelAndEffortForProvider(provider, model string) (base string, effort string, ok bool) {
	for _, suffix := range knownEffortSuffixes {
		if !strings.HasSuffix(model, "-"+suffix) {
			continue
		}
		candidate := strings.TrimSuffix(model, "-"+suffix)
		for _, allowed := range defaultReasoningEffortsForProviderModel(provider, candidate) {
			if suffix == allowed {
				return candidate, suffix, true
			}
		}
	}
	if len(defaultReasoningEffortsForProviderModel(provider, model)) > 0 {
		return model, "", true
	}
	return "", "", false
}

// ReasoningEffortsForProviderModel returns the valid suffix-based reasoning
// efforts for the provider/model pair. If model is already suffixed with a
// supported effort, the efforts for its base model are returned.
func ReasoningEffortsForProviderModel(provider, model string) []string {
	if _, _, efforts := configBaseModelAndEffortForProvider(provider, model); len(efforts) > 0 {
		return efforts
	}
	base, _ := BaseModelAndEffortForProvider(provider, model)
	if base == "" {
		return nil
	}
	if efforts := configReasoningEffortsForProviderModel(provider, base); len(efforts) > 0 {
		return efforts
	}
	return reasoningEffortsForProviderBaseModel(provider, base)
}

// ExpandWithEffortVariantsForProvider expands a model list by appending valid
// effort variants after each switchable base model for the given provider.
// Existing effort-suffixed entries are kept but not expanded again. Output is
// de-duplicated while preserving first-seen order.
func ExpandWithEffortVariantsForProvider(provider string, models []string) []string {
	var expanded []string
	seen := make(map[string]bool, len(models))
	appendModel := func(model string) {
		if model == "" || seen[model] {
			return
		}
		seen[model] = true
		expanded = append(expanded, model)
	}

	for _, m := range models {
		appendModel(m)
		base, effort := BaseModelAndEffortForProvider(provider, m)
		if effort != "" || base != m {
			continue
		}
		for _, v := range ReasoningEffortsForProviderModel(provider, base) {
			appendModel(base + "-" + v)
		}
	}
	return expanded
}

// ExpandWithEffortVariants expands a model list by appending legacy
// provider-agnostic GPT-5 effort variants. Prefer
// ExpandWithEffortVariantsForProvider when provider context is available.
func ExpandWithEffortVariants(models []string) []string {
	var expanded []string
	for _, m := range models {
		expanded = append(expanded, m)
		if hasEffortSuffix(m) {
			continue
		}
		if variants := EffortVariantsFor(m); len(variants) > 0 {
			for _, v := range variants {
				expanded = append(expanded, m+"-"+v)
			}
		}
	}
	return expanded
}

// hasEffortSuffix reports whether model already ends with a known effort suffix.
func hasEffortSuffix(model string) bool {
	_, ok := trimKnownEffortSuffix(model)
	return ok
}

// trimKnownEffortSuffix removes a trailing reasoning-effort suffix from model.
func trimKnownEffortSuffix(model string) (string, bool) {
	for _, suffix := range knownEffortSuffixes {
		if strings.HasSuffix(model, "-"+suffix) {
			return strings.TrimSuffix(model, "-"+suffix), true
		}
	}
	return model, false
}

// knownEffortSuffixes is the union of reasoning-effort suffixes recognized
// across providers. It is a parser/legacy-dedup helper, not a capability list
// for every model. Provider-aware callers should use
// BaseModelAndEffortForProvider / ReasoningEffortsForProviderModel instead.
var knownEffortSuffixes = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// DedupeEffortVariants removes effort-suffixed aliases (e.g. "opus-high",
// "gpt-5.4-medium") when the base model is also present in the list.
// Used by UIs that expose reasoning effort through a dedicated selector,
// where "<base>-<effort>" entries just duplicate what the selector covers.
// Order is preserved; entries without a matching base are kept as-is.
func DedupeEffortVariants(ids []string) []string {
	have := make(map[string]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		drop := false
		for _, suffix := range knownEffortSuffixes {
			if strings.HasSuffix(id, "-"+suffix) && have[strings.TrimSuffix(id, "-"+suffix)] {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, id)
		}
	}
	return out
}

// DedupeEffortVariantsForProvider removes effort-suffixed aliases only when
// the suffix is an explicitly supported reasoning effort for the provider/model
// and the corresponding base model is present. Natural model names such as
// gpt-5.1-codex-max are preserved when "max" is not a supported effort for
// that base model.
func DedupeEffortVariantsForProvider(provider string, ids []string) []string {
	have := make(map[string]bool, len(ids))
	for _, id := range ids {
		have[id] = true
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		base, effort := BaseModelAndEffortForProvider(provider, id)
		if effort != "" && have[base] {
			continue
		}
		out = append(out, id)
	}
	return out
}

// SortModelIDsByPopularity orders ids so the picker shows the models a user
// is most likely to pick first, then everything else alpha-sorted for easy
// scanning. The ranking signal comes from ResolveProviderModelIDs: curated
// entries for static providers and cached live entries for dynamic providers
// such as Copilot. defaultModel is pinned to the very top — always included
// even if absent from ids, so the user's configured model stays reachable when
// an upstream provider drops it from /v1/models.
// IDs not in the curated list fall through to alpha-sort.
//
// Centralizing this here means the web picker, TUI picker, and CLI completion
// all agree on what "popular first" means.
func SortModelIDsByPopularity(provider, defaultModel string, ids []string) []string {
	curated := ResolveProviderModelIDs(provider)
	rank := make(map[string]int, len(curated))
	for i, id := range curated {
		rank[id] = i
	}

	seen := make(map[string]bool, len(ids)+1)
	out := make([]string, 0, len(ids)+1)

	pin := strings.TrimSpace(defaultModel)
	if pin != "" {
		out = append(out, pin)
		seen[pin] = true
	}

	type ranked struct {
		id   string
		rank int
	}
	var curatedHits []ranked
	var others []string
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if r, ok := rank[id]; ok {
			curatedHits = append(curatedHits, ranked{id, r})
		} else {
			others = append(others, id)
		}
	}
	sort.Slice(curatedHits, func(i, j int) bool {
		return curatedHits[i].rank < curatedHits[j].rank
	})
	sort.Strings(others)

	for _, c := range curatedHits {
		out = append(out, c.id)
	}
	out = append(out, others...)
	return out
}

func resolvedProviderAPIKey(cfg *config.Config, provider string) string {
	if cfg == nil {
		return ""
	}
	_ = cfg.ResolveProviderCredentials(provider)
	if providerCfg, ok := cfg.Providers[provider]; ok {
		return providerCfg.ResolvedAPIKey
	}
	return ""
}

// GetBuiltInProviderNames returns the built-in provider type names
func GetBuiltInProviderNames() []string {
	return []string{"anthropic", "bedrock", "openai", "chatgpt", "copilot", "openrouter", "gemini", "gemini-cli", "zen", "claude-bin", "grok-bin", "vllm", "xai", "venice", "nearai", "sambanova", "ollama"}
}

// GetProviderNames returns valid provider names from config plus built-in types.
// If cfg is nil, returns only built-in provider names.
func GetProviderNames(cfg *config.Config) []string {
	names := make(map[string]bool)

	// Add built-in provider names
	for _, name := range GetBuiltInProviderNames() {
		names[name] = true
	}

	// Add configured provider names
	if cfg != nil {
		for name := range cfg.Providers {
			names[name] = true
		}
	}

	// Convert to sorted slice
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// GetImageProviderNames returns valid provider names for image generation
func GetImageProviderNames() []string {
	return []string{"debug", "gemini", "openai", "chatgpt", "xai", "venice", "flux", "openrouter"}
}

// GetProviderCompletions returns completions for the --provider flag
// It handles both provider-only and provider:model completion scenarios.
// For LLM providers, pass a config to include custom provider names.
func GetProviderCompletions(toComplete string, isImage bool, cfg *config.Config) []string {
	var providerNames []string
	var getModelIDs func(string) []string

	if isImage {
		providerNames = GetImageProviderNames()
		getModelIDs = func(p string) []string { return ImageProviderModels[p] }
	} else {
		providerNames = GetProviderNames(cfg)
		getModelIDs = ProviderModelIDs
	}

	// Check if user has typed a colon (wants model completion)
	if strings.Contains(toComplete, ":") {
		parts := strings.SplitN(toComplete, ":", 2)
		provider := parts[0]
		modelPrefix := parts[1]

		// Get models for completion
		var models []string

		// Check if config has a models list for this provider
		var configModels []string
		var configModel string
		if cfg != nil {
			if providerCfg, ok := cfg.Providers[provider]; ok {
				configModels = providerCfg.Models
				configModel = providerCfg.Model
			}
		}

		if len(configModels) > 0 {
			// Use config-defined models list, plus configured model (deduped)
			seen := make(map[string]bool)
			if configModel != "" {
				models = append(models, configModel)
				seen[configModel] = true
			}
			for _, m := range configModels {
				if !seen[m] {
					models = append(models, m)
					seen[m] = true
				}
			}
		} else {
			// Resolve provider type, including custom aliases (e.g., "acme" → "venice")
			providerType := resolveProviderType(provider)

			// For LLM (non-image) openrouter, fetch models from API cache
			if !isImage && (providerType == "openrouter" || provider == "openrouter") {
				apiKey := resolvedProviderAPIKey(cfg, provider)
				if cachedModels := GetCachedOpenRouterModels(apiKey); len(cachedModels) > 0 {
					models = cachedModels
				} else {
					models = getModelIDs("openrouter")
				}
			} else if !isImage && providerType == "venice" {
				apiKey := resolvedProviderAPIKey(cfg, provider)
				models = GetCachedVeniceModels(apiKey)
				if len(models) == 0 && configModel != "" {
					models = []string{configModel}
				}
			} else {
				models = getModelIDs(providerType)
				if len(models) == 0 {
					models = getModelIDs(provider)
				}
				if len(models) == 0 && configModel != "" {
					models = []string{configModel}
				} else if len(models) == 0 {
					return nil
				}
			}
		}

		// Expand provider-specific effort variants for tab-completion.
		if !isImage {
			models = ExpandWithEffortVariantsForProvider(provider, models)
		}

		// Filter by prefix and return as provider:model
		var completions []string
		for _, model := range models {
			if strings.HasPrefix(model, modelPrefix) {
				completions = append(completions, provider+":"+model)
			}
		}
		return completions
	}

	// No colon - offer provider names (filtered by what user typed)
	var completions []string
	for _, name := range providerNames {
		if strings.HasPrefix(name, toComplete) {
			completions = append(completions, name)
		}
	}
	return completions
}
