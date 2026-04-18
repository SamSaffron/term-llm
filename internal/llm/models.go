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
	ID          string
	InputLimit  int // effective input budget (context - output reserve)
	OutputLimit int // max output tokens
}

// ProviderModels contains the curated list of common models per LLM provider type.
// This is the single source of truth for model names AND their token limits.
// When adding a model, always include InputLimit/OutputLimit if known.
var ProviderModels = map[string][]ModelEntry{
	"anthropic": {
		// Claude 4.7 Opus (-thinking uses adaptive thinking)
		{ID: "claude-opus-4-7", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-thinking", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-1m-thinking", InputLimit: 980_000, OutputLimit: 64_000},
		// Claude 4.6 (-thinking uses adaptive thinking for 4.6 models)
		{ID: "claude-sonnet-4-6", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-thinking", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-1m-thinking", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-thinking", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-1m-thinking", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4-5-thinking", InputLimit: 180_000, OutputLimit: 64_000},
	},
	"openai": {
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
	"copilot": {
		// Uses GitHub Copilot API with device code OAuth
		// Run 'term-llm models --provider copilot' for live list
		// Copilot imposes its own context limits (from models.dev github-copilot section)
		// gpt-4.1 is free (no premium requests)
		{ID: "gpt-4.1", InputLimit: 48_000, OutputLimit: 32_768},
		// OpenAI Codex models
		{ID: "gpt-5.3-codex", InputLimit: 272_000, OutputLimit: 128_000},
		{ID: "gpt-5.2-codex", InputLimit: 144_000, OutputLimit: 128_000},
		{ID: "gpt-5.1-codex", InputLimit: 64_000, OutputLimit: 128_000},
		{ID: "gpt-5.1-codex-max", InputLimit: 64_000, OutputLimit: 128_000},
		{ID: "gpt-5.1-codex-mini", InputLimit: 64_000, OutputLimit: 128_000},
		// OpenAI standard
		{ID: "gpt-5.4", InputLimit: 922_000, OutputLimit: 128_000},
		{ID: "gpt-5.2", InputLimit: 64_000, OutputLimit: 128_000},
		{ID: "gpt-5.1", InputLimit: 64_000, OutputLimit: 128_000},
		{ID: "gpt-5-mini", InputLimit: 64_000, OutputLimit: 128_000},
		// Anthropic Claude (Copilot uses dot naming: claude-opus-4.6)
		{ID: "claude-opus-4.6-thinking", InputLimit: 64_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4.6-thinking", InputLimit: 112_000, OutputLimit: 64_000},
		{ID: "claude-opus-4.6", InputLimit: 64_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4.6", InputLimit: 112_000, OutputLimit: 64_000},
		{ID: "claude-opus-4.5", InputLimit: 96_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4.5", InputLimit: 96_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4", InputLimit: 112_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4.5", InputLimit: 96_000, OutputLimit: 64_000},
		// Google Gemini
		{ID: "gemini-3-pro", InputLimit: 64_000, OutputLimit: 65_536},
		{ID: "gemini-3-flash", InputLimit: 64_000, OutputLimit: 65_536},
		// Other
		{ID: "grok-code-fast-1", InputLimit: 64_000, OutputLimit: 16_384},
		{ID: "raptor-mini"},
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
		{ID: "opus"},
		{ID: "opus-low"},
		{ID: "opus-medium"},
		{ID: "opus-high"},
		{ID: "opus-xhigh"},
		{ID: "opus-max"},
		{ID: "sonnet"},
		{ID: "sonnet-low"},
		{ID: "sonnet-medium"},
		{ID: "sonnet-high"},
		{ID: "haiku"},
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
	"bedrock": {
		// AWS Bedrock: same friendly names as anthropic (translated to Bedrock IDs internally)
		{ID: "claude-opus-4-7", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-thinking", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-7-1m-thinking", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-thinking", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-6-1m-thinking", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-thinking", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-1m", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-opus-4-6-1m-thinking", InputLimit: 980_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4-5", InputLimit: 180_000, OutputLimit: 64_000},
		{ID: "claude-haiku-4-5-thinking", InputLimit: 180_000, OutputLimit: 64_000},
	},
	"venice": {
		// Limits synced from Venice /models API on 2026-04-18.
		{ID: "venice-uncensored", InputLimit: 32_000, OutputLimit: 8_192},
		{ID: "venice-uncensored-1-2", InputLimit: 128_000, OutputLimit: 8_192},
		{ID: "venice-uncensored-role-play", InputLimit: 128_000, OutputLimit: 4_096},
		{ID: "olafangensan-glm-4.7-flash-heretic", InputLimit: 200_000, OutputLimit: 24_000},
		{ID: "zai-org-glm-4.7-flash", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "zai-org-glm-5", InputLimit: 198_000, OutputLimit: 32_000},
		{ID: "zai-org-glm-5-1", InputLimit: 200_000, OutputLimit: 24_000},
		{ID: "z-ai-glm-5-turbo", InputLimit: 200_000, OutputLimit: 32_768},
		{ID: "z-ai-glm-5v-turbo", InputLimit: 200_000, OutputLimit: 32_768},
		{ID: "zai-org-glm-4.6", InputLimit: 198_000, OutputLimit: 16_384},
		{ID: "zai-org-glm-4.7", InputLimit: 198_000, OutputLimit: 16_384},
		{ID: "mistral-small-3-2-24b-instruct", InputLimit: 256_000, OutputLimit: 16_384},
		{ID: "mistral-small-2603", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "qwen3-235b-a22b-thinking-2507", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "qwen3-235b-a22b-instruct-2507", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "qwen3-next-80b", InputLimit: 256_000, OutputLimit: 16_384},
		{ID: "qwen3-coder-480b-a35b-instruct", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "qwen3-coder-480b-a35b-instruct-turbo", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "qwen-3-6-plus", InputLimit: 1_000_000, OutputLimit: 65_536},
		{ID: "qwen3-5-9b", InputLimit: 256_000, OutputLimit: 32_768},
		{ID: "qwen3-5-35b-a3b", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "qwen3-5-397b-a17b", InputLimit: 128_000, OutputLimit: 32_768},
		{ID: "hermes-3-llama-3.1-405b", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "google-gemma-3-27b-it", InputLimit: 198_000, OutputLimit: 16_384},
		{ID: "google-gemma-4-26b-a4b-it", InputLimit: 256_000, OutputLimit: 8_192},
		{ID: "google-gemma-4-31b-it", InputLimit: 256_000, OutputLimit: 8_192},
		{ID: "gemma-4-uncensored", InputLimit: 256_000, OutputLimit: 8_192},
		{ID: "grok-41-fast", InputLimit: 1_000_000, OutputLimit: 30_000},
		{ID: "grok-4-20", InputLimit: 2_000_000, OutputLimit: 128_000},
		{ID: "grok-4-20-multi-agent", InputLimit: 2_000_000, OutputLimit: 128_000},
		{ID: "gemini-3-1-pro-preview", InputLimit: 1_000_000, OutputLimit: 32_768},
		{ID: "gemini-3-flash-preview", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "claude-opus-4-7", InputLimit: 1_000_000, OutputLimit: 128_000},
		{ID: "claude-opus-4-6", InputLimit: 1_000_000, OutputLimit: 128_000},
		{ID: "claude-opus-4-6-fast", InputLimit: 1_000_000, OutputLimit: 128_000},
		{ID: "claude-opus-4-5", InputLimit: 198_000, OutputLimit: 32_768},
		{ID: "claude-sonnet-4-6", InputLimit: 1_000_000, OutputLimit: 64_000},
		{ID: "claude-sonnet-4-5", InputLimit: 198_000, OutputLimit: 64_000},
		{ID: "openai-gpt-oss-120b", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "openai-gpt-52", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "openai-gpt-52-codex", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "openai-gpt-53-codex", InputLimit: 400_000, OutputLimit: 128_000},
		{ID: "openai-gpt-54", InputLimit: 1_000_000, OutputLimit: 131_072},
		{ID: "openai-gpt-54-mini", InputLimit: 400_000, OutputLimit: 128_000},
		{ID: "openai-gpt-54-pro", InputLimit: 1_000_000, OutputLimit: 128_000},
		{ID: "openai-gpt-4o-2024-11-20", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "openai-gpt-4o-mini-2024-07-18", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "kimi-k2-thinking", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "arcee-trinity-large-thinking", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "kimi-k2-5", InputLimit: 256_000, OutputLimit: 65_536},
		{ID: "deepseek-v3.2", InputLimit: 160_000, OutputLimit: 32_768},
		{ID: "aion-labs-aion-2-0", InputLimit: 128_000, OutputLimit: 32_768},
		{ID: "llama-3.2-3b", InputLimit: 128_000, OutputLimit: 4_096},
		{ID: "llama-3.3-70b", InputLimit: 128_000, OutputLimit: 4_096},
		{ID: "minimax-m25", InputLimit: 198_000, OutputLimit: 32_768},
		{ID: "minimax-m27", InputLimit: 198_000, OutputLimit: 32_768},
		{ID: "mercury-2", InputLimit: 128_000, OutputLimit: 50_000},
		{ID: "qwen3-vl-235b-a22b", InputLimit: 256_000, OutputLimit: 16_384},
		{ID: "nvidia-nemotron-3-nano-30b-a3b", InputLimit: 128_000, OutputLimit: 16_384},
		{ID: "nvidia-nemotron-cascade-2-30b-a3b", InputLimit: 256_000, OutputLimit: 32_768},
	},
}

// ProviderModelIDs returns just the model ID strings for a provider.
// This only checks the built-in ProviderModels map by exact key.
// For callers that might receive a custom alias name, use ResolveProviderModelIDs.
func ProviderModelIDs(provider string) []string {
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
var ProviderFastModels = map[string]string{
	"anthropic":  "claude-haiku-4-5",
	"openai":     "gpt-5.4-nano",
	"chatgpt":    "gpt-5.4-mini",
	"copilot":    "gpt-4.1",
	"gemini":     "gemini-2.5-flash-lite",
	"gemini-cli": "gemini-2.5-flash-lite",
	"xai":        "grok-3-mini-fast",
	"zen":        "minimax-m2.5-free",
	"bedrock":    "claude-haiku-4-5",
	"venice":     "llama-3.2-3b",
	"openrouter": "anthropic/claude-haiku-4-5",
	"claude-bin": "haiku",
}

var ImageProviderModels = map[string][]string{
	"debug":      {"random"},
	"gemini":     {"gemini-2.5-flash-image", "gemini-3-pro-image-preview", "gemini-3.1-flash-image-preview"},
	"openai":     {"gpt-image-1.5", "gpt-image-1-mini"},
	"xai":        {"grok-2-image", "grok-2-image-1212"},
	"venice":     {"nano-banana-pro", "nano-banana-2", "flux-2-pro", "flux-2-max", "gpt-image-1-5", "imagineart-1.5-pro", "recraft-v4", "recraft-v4-pro", "seedream-v4", "seedream-v5-lite", "qwen-image", "qwen-image-2", "qwen-image-2-pro", "grok-imagine-image", "grok-imagine-image-pro", "hunyuan-image-v3", "venice-sd35", "hidream", "chroma", "z-image-turbo", "wan-2-7-text-to-image", "wan-2-7-pro-text-to-image", "lustify-sdxl", "lustify-v7", "lustify-v8", "wai-Illustrious", "bria-bg-remover", "qwen-edit", "nano-banana-pro-edit", "nano-banana-2-edit", "flux-2-max-edit", "gpt-image-1-5-edit", "seedream-v4-edit", "seedream-v5-lite-edit", "qwen-image-2-edit", "qwen-image-2-pro-edit", "grok-imagine-edit", "firered-image-edit"},
	"flux":       {"flux-2-pro", "flux-kontext-pro", "flux-2-max"},
	"openrouter": {"google/gemini-2.5-flash-image", "google/gemini-3-pro-image-preview", "openai/gpt-5-image", "openai/gpt-5-image-mini", "bytedance-seed/seedream-4.5", "black-forest-labs/flux.2-pro"},
}

// defaultEffortVariants are the standard effort levels for reasoning-capable models.
var defaultEffortVariants = []string{"minimal", "low", "medium", "high", "xhigh"}

// EffortVariantsFor returns the effort suffixes for a model, or nil if none.
// All GPT-5 family models are reasoning-capable and support effort levels.
// Handles both direct model names ("gpt-5.4") and vendor-prefixed names ("openai/gpt-5.4").
func EffortVariantsFor(model string) []string {
	name := model
	if i := strings.LastIndex(model, "/"); i >= 0 {
		name = model[i+1:]
	}
	if strings.HasPrefix(name, "gpt-5") {
		return defaultEffortVariants
	}
	return nil
}

// ExpandWithEffortVariants expands a model list by appending effort variants
// after each base model. Used for tab-completion where all variants are needed.
// Models that already end with a known effort suffix are not expanded again.
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
	for _, suffix := range defaultEffortVariants {
		if strings.HasSuffix(model, "-"+suffix) {
			return true
		}
	}
	return false
}

// knownEffortSuffixes is the union of reasoning-effort suffixes recognised
// across providers (gpt-5, claude-bin opus/sonnet). Used to detect
// "<base>-<effort>" aliases that duplicate a base model when effort is
// picked separately.
var knownEffortSuffixes = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

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

// GetBuiltInProviderNames returns the built-in provider type names
func GetBuiltInProviderNames() []string {
	return []string{"anthropic", "bedrock", "openai", "chatgpt", "copilot", "openrouter", "gemini", "gemini-cli", "zen", "claude-bin", "xai", "venice"}
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
	return []string{"debug", "gemini", "openai", "xai", "venice", "flux", "openrouter"}
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
				var apiKey string
				if cfg != nil {
					if providerCfg, ok := cfg.Providers[provider]; ok {
						apiKey = providerCfg.ResolvedAPIKey
					}
				}
				if cachedModels := GetCachedOpenRouterModels(apiKey); len(cachedModels) > 0 {
					models = cachedModels
				} else {
					models = getModelIDs("openrouter")
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

		// Expand effort variants for tab-completion
		if !isImage {
			models = ExpandWithEffortVariants(models)
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
