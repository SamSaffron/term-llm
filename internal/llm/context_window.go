package llm

import (
	"fmt"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/config"
)

// ConfigModelLimit holds per-model token limits from user config.
type ConfigModelLimit struct {
	Provider    string // provider config key (e.g., "cdck", "discourse")
	Model       string
	InputLimit  int
	OutputLimit int
}

// ConfigModelReasoningEfforts holds per-provider/model reasoning-effort
// capabilities from user config.
type ConfigModelReasoningEfforts struct {
	Provider string // provider config key (e.g., "cdck_qwen")
	Model    string
	Efforts  []string
}

var (
	configMu sync.RWMutex
	// Provider-scoped: "provider\x00model" -> limit (always authoritative)
	configProviderInputLimits  map[string]int
	configProviderOutputLimits map[string]int
	// Model-only fallback: populated only when all providers agree on limits
	configInputLimits  map[string]int
	configOutputLimits map[string]int

	// Explicit limits from ProviderModels (populated by init).
	// Provider-scoped: "provider\x00model" -> limit
	explicitProviderInput map[string]int
	// Model-only: populated only when all providers agree on limits
	explicitModelInput  map[string]int
	explicitModelOutput map[string]int

	// Provider-scoped reasoning-effort capabilities registered from user config.
	// Key is "provider\x00model" with model lower-cased.
	configProviderReasoningEfforts map[string][]string

	// providerAliases maps custom provider names to built-in type names
	// so that provider-scoped limits resolve correctly for aliases like
	// "acme" -> "venice". Populated by RegisterProviderAliases.
	providerAliases map[string]string
)

func init() {
	explicitProviderInput = make(map[string]int)

	inputByModel := make(map[string]int)
	outputByModel := make(map[string]int)
	inputConflict := make(map[string]bool)
	outputConflict := make(map[string]bool)

	for provider, entries := range ProviderModels {
		for _, e := range entries {
			model := strings.ToLower(e.ID)
			if e.InputLimit > 0 {
				explicitProviderInput[configKey(provider, model)] = e.InputLimit
				if prev, ok := inputByModel[model]; ok && prev != e.InputLimit {
					inputConflict[model] = true
				}
				inputByModel[model] = e.InputLimit
			}
			if e.OutputLimit > 0 {
				if prev, ok := outputByModel[model]; ok && prev != e.OutputLimit {
					outputConflict[model] = true
				}
				outputByModel[model] = e.OutputLimit
			}
		}
	}

	explicitModelInput = make(map[string]int, len(inputByModel))
	for model, limit := range inputByModel {
		if !inputConflict[model] {
			explicitModelInput[model] = limit
		}
	}
	explicitModelOutput = make(map[string]int, len(outputByModel))
	for model, limit := range outputByModel {
		if !outputConflict[model] {
			explicitModelOutput[model] = limit
		}
	}
}

// RegisterProviderAliases registers custom provider name → built-in type mappings
// from user config. This allows provider-scoped limits to resolve correctly for
// aliases like "acme" → "venice".
func RegisterProviderAliases(aliases map[string]string) {
	configMu.Lock()
	defer configMu.Unlock()
	providerAliases = aliases
}

func resolveProviderType(providerName string) string {
	providerType := string(config.InferProviderType(providerName, ""))
	if providerType == providerName || providerType == string(config.ProviderTypeOpenAICompat) {
		// InferProviderType didn't recognize the name; check aliases
		configMu.RLock()
		if alias, ok := providerAliases[providerName]; ok {
			configMu.RUnlock()
			return alias
		}
		configMu.RUnlock()
	}
	return providerType
}

func configKey(provider, model string) string {
	return provider + "\x00" + model
}

// RegisterConfigLimits registers model token limits from user configuration.
// These are used as a fallback when hardcoded tables return 0.
// Limits are stored provider-scoped; model-only fallback is populated only
// when all providers defining a model agree on the same limits.
func RegisterConfigLimits(limits []ConfigModelLimit) {
	configMu.Lock()
	defer configMu.Unlock()

	configProviderInputLimits = make(map[string]int, len(limits))
	configProviderOutputLimits = make(map[string]int, len(limits))

	// First pass: populate provider-scoped maps and detect model-only collisions
	inputByModel := make(map[string]int)
	outputByModel := make(map[string]int)
	inputConflict := make(map[string]bool)
	outputConflict := make(map[string]bool)

	for _, l := range limits {
		model := strings.ToLower(l.Model)
		provider := strings.ToLower(l.Provider)

		if l.InputLimit > 0 {
			configProviderInputLimits[configKey(provider, model)] = l.InputLimit
			if prev, exists := inputByModel[model]; exists && prev != l.InputLimit {
				inputConflict[model] = true
			}
			inputByModel[model] = l.InputLimit
		}
		if l.OutputLimit > 0 {
			configProviderOutputLimits[configKey(provider, model)] = l.OutputLimit
			if prev, exists := outputByModel[model]; exists && prev != l.OutputLimit {
				outputConflict[model] = true
			}
			outputByModel[model] = l.OutputLimit
		}
	}

	// Second pass: model-only maps exclude conflicting entries
	configInputLimits = make(map[string]int, len(inputByModel))
	for model, limit := range inputByModel {
		if !inputConflict[model] {
			configInputLimits[model] = limit
		}
	}
	configOutputLimits = make(map[string]int, len(outputByModel))
	for model, limit := range outputByModel {
		if !outputConflict[model] {
			configOutputLimits[model] = limit
		}
	}
}

// RegisterConfigReasoningEfforts registers per-provider/model reasoning-effort
// capabilities from user configuration. Always call it on config load (even
// with nil) so reloads clear stale capabilities.
func RegisterConfigReasoningEfforts(entries []ConfigModelReasoningEfforts) {
	configMu.Lock()
	defer configMu.Unlock()

	configProviderReasoningEfforts = make(map[string][]string, len(entries))
	for _, entry := range entries {
		provider := strings.ToLower(strings.TrimSpace(entry.Provider))
		model := strings.ToLower(strings.TrimSpace(entry.Model))
		if provider == "" || model == "" || len(entry.Efforts) == 0 {
			continue
		}
		configProviderReasoningEfforts[configKey(provider, model)] = normalizeReasoningEfforts(entry.Efforts)
	}
}

func normalizeReasoningEfforts(efforts []string) []string {
	seen := make(map[string]bool, len(efforts))
	out := make([]string, 0, len(efforts))
	for _, effort := range efforts {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" || seen[effort] {
			continue
		}
		seen[effort] = true
		out = append(out, effort)
	}
	return out
}

func configReasoningEffortsForProviderModel(provider, model string) []string {
	configMu.RLock()
	defer configMu.RUnlock()
	if len(configProviderReasoningEfforts) == 0 {
		return nil
	}
	if efforts := configProviderReasoningEfforts[configKey(strings.ToLower(strings.TrimSpace(provider)), strings.ToLower(strings.TrimSpace(model)))]; len(efforts) > 0 {
		return cloneEfforts(efforts)
	}
	return nil
}

func configBaseModelAndEffortForProvider(provider, model string) (string, string, []string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	trimmedModel := strings.TrimSpace(model)
	modelLower := strings.ToLower(trimmedModel)

	configMu.RLock()
	defer configMu.RUnlock()
	if len(configProviderReasoningEfforts) == 0 || provider == "" || modelLower == "" {
		return "", "", nil
	}
	if efforts := configProviderReasoningEfforts[configKey(provider, modelLower)]; len(efforts) > 0 {
		return trimmedModel, "", cloneEfforts(efforts)
	}
	prefix := provider + "\x00"
	for key, efforts := range configProviderReasoningEfforts {
		if !strings.HasPrefix(key, prefix) || len(efforts) == 0 {
			continue
		}
		baseLower := strings.TrimPrefix(key, prefix)
		for _, effort := range efforts {
			suffix := "-" + effort
			if strings.HasSuffix(modelLower, suffix) && strings.TrimSuffix(modelLower, suffix) == baseLower {
				return strings.TrimSuffix(trimmedModel, suffix), effort, cloneEfforts(efforts)
			}
		}
	}
	return "", "", nil
}

func configInputLimitForProvider(provider, model string) int {
	configMu.RLock()
	defer configMu.RUnlock()
	provider = strings.ToLower(provider)
	if v, ok := configProviderInputLimits[configKey(provider, model)]; ok {
		return v
	}
	if base, ok := trimKnownEffortSuffix(model); ok {
		if v, ok := configProviderInputLimits[configKey(provider, base)]; ok {
			return v
		}
	}
	return configInputLimits[model]
}

func configInputLimit(model string) int {
	configMu.RLock()
	defer configMu.RUnlock()
	return configInputLimits[model]
}

func configOutputLimit(model string) int {
	configMu.RLock()
	defer configMu.RUnlock()
	return configOutputLimits[model]
}

// InputLimitForModel returns the effective input token limit for a known model
// using canonical (direct API) numbers. This is the maximum number of tokens
// that can be sent as input — not the total context window (input + output).
// Returns 0 for unknown models.
// For provider-specific limits, use InputLimitForProviderModel instead.
func InputLimitForModel(model string) int {
	model = strings.ToLower(model)
	// 1. Explicit limits from ProviderModels (non-conflicting model-only)
	if v, ok := explicitModelInput[model]; ok {
		return v
	}
	// 2. Prefix-matched tables
	if result := lookupPrefix(model, inputLimitTable); result > 0 {
		return result
	}
	// 3. Config fallback
	return configInputLimit(model)
}

// InputLimitForProviderModel returns the effective input token limit for a model
// accessed through a specific provider. Some providers expose/reroute models
// with limits that differ from the model's canonical direct-API limit.
// The providerName can be either a provider type (e.g., "copilot") or a
// custom provider name — it is resolved to the underlying type automatically.
// Most providers fall back to canonical numbers when provider-specific data is
// unavailable; dynamic providers such as Copilot and Venice avoid that fallback
// because canonical model-prefix tables can be wrong for their routed models.
func InputLimitForProviderModel(providerName, model string) int {
	model = strings.ToLower(model)

	// Resolve provider name to type (e.g., "my-copilot" -> "copilot",
	// or "acme" -> "venice" via registered aliases).
	providerType := resolveProviderType(providerName)

	// Copilot model metadata is account/provider-specific and fetched from the
	// live /models endpoint. Do not fall back to canonical OpenAI/Gemini/Claude
	// prefix tables here: those numbers can be wrong for Copilot's routed models.
	if providerType == string(config.ProviderTypeCopilot) {
		if result := copilotCachedInputLimit(model); result > 0 {
			return result
		}
		return configInputLimitForProvider(providerName, model)
	}

	// 1. Explicit provider-scoped limits from ProviderModels
	if v, ok := explicitProviderInput[configKey(providerType, model)]; ok {
		return v
	}
	if providerType != providerName {
		if v, ok := explicitProviderInput[configKey(providerName, model)]; ok {
			return v
		}
	}
	if base, ok := trimKnownEffortSuffix(model); ok {
		if v, ok := explicitProviderInput[configKey(providerType, base)]; ok {
			return v
		}
		if providerType != providerName {
			if v, ok := explicitProviderInput[configKey(providerName, base)]; ok {
				return v
			}
		}
	}

	// 2. Provider-specific overrides (e.g., Zen)
	if table, ok := providerInputOverrides[providerType]; ok {
		if result := lookupPrefix(model, table); result > 0 {
			return result
		}
	}

	// 3. Provider model APIs can return dynamic context windows for models that
	// move faster than the curated static table. Consult those provider caches
	// before generic canonical prefix tables so provider-reported metadata wins.
	if providerType == string(config.ProviderTypeOpenRouter) {
		if result := openRouterCachedInputLimit(model); result > 0 {
			return result
		}
	}

	// Venice's model IDs and context windows are provider-specific and come from
	// Venice /models. Do not fall back to canonical prefix tables here: they can
	// be wrong for routed/renamed models and create a second source of truth.
	if providerType == string(config.ProviderTypeVenice) {
		if result := veniceCachedInputLimit(model); result > 0 {
			return result
		}
		return configInputLimitForProvider(providerName, model)
	}

	// 4. Prefix-matched canonical tables
	if result := lookupPrefix(model, inputLimitTable); result > 0 {
		return result
	}

	// 5. Config fallback (provider-scoped, then model-only)
	return configInputLimitForProvider(providerName, model)
}

// FormatTokenCount returns a human-readable string for a token count
// (e.g., "128K", "1M", "200K"). Returns "" for zero or negative values.
func FormatTokenCount(tokens int) string {
	if tokens <= 0 {
		return ""
	}
	if tokens >= 1_000_000 {
		// Round to nearest 100K for cleaner display
		rounded := (tokens + 50_000) / 100_000 // e.g., 1_048_576 → 10, 2_097_152 → 21
		if rounded%10 == 0 {
			return fmt.Sprintf("%dM", rounded/10) // e.g., 10 → "1M"
		}
		return fmt.Sprintf("%.1fM", float64(rounded)/10) // e.g., 21 → "2.1M"
	}
	k := (tokens + 500) / 1_000 // Round to nearest K
	return fmt.Sprintf("%dK", k)
}

type limitEntry struct {
	prefix string
	tokens int
}

func lookupPrefix(model string, table []limitEntry) int {
	best := 0
	bestLen := 0
	for _, e := range table {
		if !strings.HasPrefix(model, e.prefix) {
			continue
		}
		// Require a token boundary after the prefix: exact match or the next char
		// must be non-alphanumeric (e.g. '-', '.'). Without this check,
		// "gpt-5.4-minimal" would wrongly match the longer prefix "gpt-5.4-mini".
		if len(model) != len(e.prefix) {
			c := model[len(e.prefix)]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				continue
			}
		}
		if len(e.prefix) > bestLen {
			best = e.tokens
			bestLen = len(e.prefix)
		}
	}
	return best
}

// inputLimitTable contains effective input token limits.
// These represent the practical INPUT token budget for compaction purposes.
//
// For Anthropic Claude 4.x: we follow Claude CLI's approach — cap the output
// reservation at 20K rather than the full theoretical max (64K–128K). In practice,
// compaction never fires while the model is also generating 64K of output, so
// reserving that much is needlessly conservative. 200K - 20K = 180K working window.
// Claude 3.x models have small max outputs (4K–8K) so the full deduction applies.
//
// For other providers: context - max_output (or explicit input limit if known).
//
// Entries are matched by longest prefix. Unknown models return 0 (compaction disabled).
var inputLimitTable = []limitEntry{
	// Anthropic Claude 1M context: 1M ctx - 20K practical output reserve = 980K
	// Fable 5, Opus 4.8, Opus 4.7, Opus 4.6, and Sonnet 4.6 have 1M context.
	// The -1m aliases remain for backwards compatibility with existing configs.
	// Older 1M beta-only entries are enabled via -1m suffix (sends context-1m-2025-08-07 beta header).
	// Requires Anthropic usage tier 4 or custom rate limits.
	{"claude-fable-5", 980_000},
	{"claude-opus-4-8", 980_000},
	{"claude-opus-4-7", 980_000},
	{"claude-opus-4-6", 980_000},
	{"claude-sonnet-4-6", 980_000},
	{"claude-sonnet-4-6-1m", 980_000},
	{"claude-sonnet-4-5-1m", 980_000},
	{"claude-sonnet-4-1m", 980_000},
	{"claude-opus-4-7-1m", 980_000},
	{"claude-opus-4-6-1m", 980_000},

	// Anthropic Claude 4.x: 200K ctx - 20K practical output reserve = 180K
	{"claude-sonnet-4-5", 180_000},
	{"claude-opus-4-5", 180_000},
	{"claude-haiku-4-5", 180_000},
	{"claude-sonnet-4", 180_000},
	{"claude-opus-4", 180_000},
	{"claude-haiku-4", 180_000},
	// Claude 3.x: max output is 4K–8K, well under 20K — full deduction
	{"claude-3.5-sonnet", 192_000}, // 200K - 8K
	{"claude-3.5-haiku", 192_000},  // 200K - 8K
	{"claude-3-opus", 196_000},     // 200K - 4K
	{"claude-3-sonnet", 196_000},   // 200K - 4K
	{"claude-3-haiku", 196_000},    // 200K - 4K

	// AWS Bedrock: same limits as direct Anthropic. Bedrock model IDs use
	// geographic prefixes (us./eu./ap.) followed by anthropic.<model>.
	// Prefix matching covers all regions and ARN passthrough users can
	// set context_window in config.
	{"us.anthropic.claude-fable-5", 980_000},
	{"us.anthropic.claude-opus-4-8", 980_000},
	{"us.anthropic.claude-opus-4-7", 980_000},
	{"us.anthropic.claude-sonnet-4-6", 980_000},
	{"us.anthropic.claude-opus-4-6", 980_000},
	{"us.anthropic.claude-haiku-4-5", 180_000},
	{"us.anthropic.claude-sonnet-4-5", 180_000},
	{"us.anthropic.claude-opus-4-5", 180_000},
	{"us.anthropic.claude-sonnet-4", 180_000},
	{"eu.anthropic.claude-fable-5", 980_000},
	{"eu.anthropic.claude-opus-4-8", 980_000},
	{"eu.anthropic.claude-opus-4-7", 980_000},
	{"eu.anthropic.claude-sonnet-4-6", 980_000},
	{"eu.anthropic.claude-opus-4-6", 980_000},
	{"eu.anthropic.claude-haiku-4-5", 180_000},
	{"eu.anthropic.claude-sonnet-4-5", 180_000},
	{"eu.anthropic.claude-opus-4-5", 180_000},
	{"eu.anthropic.claude-sonnet-4", 180_000},
	{"ap.anthropic.claude-fable-5", 980_000},
	{"ap.anthropic.claude-opus-4-8", 980_000},
	{"ap.anthropic.claude-opus-4-7", 980_000},
	{"ap.anthropic.claude-sonnet-4-6", 980_000},
	{"ap.anthropic.claude-opus-4-6", 980_000},
	{"ap.anthropic.claude-haiku-4-5", 180_000},
	{"ap.anthropic.claude-sonnet-4-5", 180_000},
	{"ap.anthropic.claude-opus-4-5", 180_000},
	{"ap.anthropic.claude-sonnet-4", 180_000},
	{"anthropic.claude-fable-5", 980_000},
	{"anthropic.claude-opus-4-8", 980_000},
	{"anthropic.claude-opus-4-7", 980_000},
	{"anthropic.claude-sonnet-4-6", 980_000},
	{"anthropic.claude-opus-4-6", 980_000},
	{"anthropic.claude-haiku-4-5", 180_000},
	{"anthropic.claude-sonnet-4-5", 180_000},
	{"anthropic.claude-opus-4-5", 180_000},
	{"anthropic.claude-sonnet-4", 180_000},

	// OpenAI GPT-5 family
	{"gpt-5.6-sol", 922_000},         // 1,050,000 ctx - 128,000 out
	{"gpt-5.6-terra", 922_000},       // 1,050,000 ctx - 128,000 out
	{"gpt-5.6-luna", 922_000},        // 1,050,000 ctx - 128,000 out
	{"gpt-5.5", 922_000},             // 1,050,000 ctx - 128,000 out
	{"gpt-5.4-mini", 272_000},        // 400K ctx - 128K out
	{"gpt-5.4-nano", 272_000},        // 400K ctx - 128K out
	{"gpt-5.4", 922_000},             // 1,050,000 ctx - 128,000 out
	{"gpt-5.3-codex-spark", 100_000}, // input=100000
	{"gpt-5.1-chat", 112_000},        // 128K ctx - 16K out (no explicit input)
	{"gpt-5.2-chat", 112_000},        // 128K ctx - 16K out
	{"gpt-5", 272_000},               // all other gpt-5.*: input=272000

	// OpenAI GPT-4.1 family (from models.dev: 1047576 ctx - 32768 out)
	{"gpt-4.1", 1_014_808},

	// OpenAI GPT-4o family (128K ctx - 16K out)
	{"gpt-4o", 112_000},

	// OpenAI GPT-4 Turbo (128K ctx - 4K out)
	{"gpt-4-turbo", 124_000},

	// OpenAI GPT-4 (original)
	{"gpt-4-32k", 32_768},
	{"gpt-4", 8_192},

	// OpenAI GPT-3.5 (16385 ctx - 4096 out)
	{"gpt-3.5-turbo", 12_000},

	// OpenAI o-series reasoning models (context - output)
	{"o1-pro", 100_000},  // 200K - 100K
	{"o1-mini", 62_000},  // 128K - 65K
	{"o1", 100_000},      // 200K - 100K
	{"o3-mini", 100_000}, // 200K - 100K
	{"o3", 100_000},      // 200K - 100K
	{"o4-mini", 100_000}, // 200K - 100K

	// Google Gemini (from models.dev google section: context - output)
	{"gemini-3-pro", 936_000},   // 1M - 64K
	{"gemini-3-flash", 983_000}, // 1M - 65K
	{"gemini-2.5-pro", 983_000},
	{"gemini-2.5-flash", 983_000},
	{"gemini-2.0-flash", 1_040_000}, // 1M - 8K
	{"gemini-1.5-pro", 992_000},     // 1M - 8K
	{"gemini-1.5-flash", 992_000},

	// DeepSeek (context only, no output breakdown available)
	{"deepseek-chat", 128_000},
	{"deepseek-reasoner", 128_000},

	// xAI Grok (from models.dev xai section: context - output)
	{"grok-4-1", 1_970_000}, // 2M - 30K
	{"grok-4", 192_000},     // 256K - 64K
	{"grok-3", 123_000},     // 131K - 8K
	{"grok-code", 246_000},  // 256K - 10K
	{"grok-2", 123_000},     // 131K - 8K
}

// providerInputOverrides contains provider-specific effective input limits
// that differ from the model's canonical limits.
// Values are effective input budgets for compaction/model-list display.
var providerInputOverrides = map[string][]limitEntry{
	// The ChatGPT Codex backend reports an account/rollout-specific 372K input
	// budget for GPT-5.6, distinct from the public API's 922K effective input.
	"chatgpt": {
		{"gpt-5.6-sol", 372_000},
		{"gpt-5.6-terra", 372_000},
		{"gpt-5.6-luna", 372_000},
	},
	// Zen free tier imposes lower limits than canonical models
	"zen": {
		{"gpt-5-nano", 96_000}, // 128K context on Zen (not 400K like direct OpenAI)
	},
}

// OutputLimitForModel returns the maximum output tokens for a known model.
// Returns 0 for unknown models.
func OutputLimitForModel(model string) int {
	model = strings.ToLower(model)
	// 1. Explicit limits from ProviderModels (non-conflicting model-only)
	if v, ok := explicitModelOutput[model]; ok {
		return v
	}
	// 2. Prefix-matched tables
	if result := lookupPrefix(model, outputLimitTable); result > 0 {
		return result
	}
	// 3. Config fallback
	return configOutputLimit(model)
}

// ClampOutputTokens returns the requested output token count clamped to the
// model's maximum output limit. If the model is unknown (limit=0) or the
// requested value is within bounds, it is returned unchanged. This allows
// callers like Compact() to always set a budget without worrying about
// per-model limits — providers call this to silently cap the value.
func ClampOutputTokens(requested int, model string) int {
	if requested <= 0 {
		return requested
	}
	limit := OutputLimitForModel(model)
	if limit > 0 && requested > limit {
		return limit
	}
	return requested
}

// outputLimitTable contains maximum output token limits per model.
// Derived from context_window - input_limit (see inputLimitTable comments).
// Entries are matched by longest prefix. Unknown models return 0 (no clamping).
var outputLimitTable = []limitEntry{
	// Anthropic Claude 4.x/5: theoretical max is 64K-128K but practical is 16K-20K.
	// Use the actual API max to avoid rejections.
	{"claude-fable-5", 128_000},
	{"claude-opus-4-8", 128_000},
	{"claude-sonnet-4-6-1m", 64_000},
	{"claude-sonnet-4-5-1m", 64_000},
	{"claude-sonnet-4-1m", 64_000},
	{"claude-opus-4-7", 64_000},
	{"claude-opus-4-7-1m", 64_000},
	{"claude-opus-4-6-1m", 64_000},
	{"claude-sonnet-4-6", 64_000},
	{"claude-opus-4-6", 64_000},
	{"claude-sonnet-4-5", 64_000},
	{"claude-opus-4-5", 64_000},
	{"claude-haiku-4-5", 64_000},
	{"claude-sonnet-4", 64_000},
	{"claude-opus-4", 64_000},
	{"claude-haiku-4", 64_000},
	// Claude 3.x: smaller max outputs
	{"claude-3.5-sonnet", 8_192},
	{"claude-3.5-haiku", 8_192},
	{"claude-3-opus", 4_096},
	{"claude-3-sonnet", 4_096},
	{"claude-3-haiku", 4_096},

	// AWS Bedrock (same as direct Anthropic, all geo prefixes)
	{"us.anthropic.claude-fable-5", 128_000},
	{"us.anthropic.claude-opus-4-8", 128_000},
	{"us.anthropic.claude-opus-4-7", 64_000},
	{"us.anthropic.claude-sonnet-4-6", 64_000},
	{"us.anthropic.claude-opus-4-6", 64_000},
	{"us.anthropic.claude-haiku-4-5", 64_000},
	{"us.anthropic.claude-sonnet-4-5", 64_000},
	{"us.anthropic.claude-opus-4-5", 64_000},
	{"us.anthropic.claude-sonnet-4", 64_000},
	{"eu.anthropic.claude-fable-5", 128_000},
	{"eu.anthropic.claude-opus-4-8", 128_000},
	{"eu.anthropic.claude-opus-4-7", 64_000},
	{"eu.anthropic.claude-sonnet-4-6", 64_000},
	{"eu.anthropic.claude-opus-4-6", 64_000},
	{"eu.anthropic.claude-haiku-4-5", 64_000},
	{"eu.anthropic.claude-sonnet-4-5", 64_000},
	{"eu.anthropic.claude-opus-4-5", 64_000},
	{"eu.anthropic.claude-sonnet-4", 64_000},
	{"ap.anthropic.claude-fable-5", 128_000},
	{"ap.anthropic.claude-opus-4-8", 128_000},
	{"ap.anthropic.claude-opus-4-7", 64_000},
	{"ap.anthropic.claude-sonnet-4-6", 64_000},
	{"ap.anthropic.claude-opus-4-6", 64_000},
	{"ap.anthropic.claude-haiku-4-5", 64_000},
	{"ap.anthropic.claude-sonnet-4-5", 64_000},
	{"ap.anthropic.claude-opus-4-5", 64_000},
	{"ap.anthropic.claude-sonnet-4", 64_000},
	{"anthropic.claude-fable-5", 128_000},
	{"anthropic.claude-opus-4-8", 128_000},
	{"anthropic.claude-opus-4-7", 64_000},
	{"anthropic.claude-sonnet-4-6", 64_000},
	{"anthropic.claude-opus-4-6", 64_000},
	{"anthropic.claude-haiku-4-5", 64_000},
	{"anthropic.claude-sonnet-4-5", 64_000},
	{"anthropic.claude-opus-4-5", 64_000},
	{"anthropic.claude-sonnet-4", 64_000},

	// OpenAI GPT-5 family
	{"gpt-5.6-sol", 128_000},
	{"gpt-5.6-terra", 128_000},
	{"gpt-5.6-luna", 128_000},
	{"gpt-5.5", 128_000},
	{"gpt-5.4-mini", 128_000},
	{"gpt-5.4-nano", 128_000},
	{"gpt-5.4", 128_000},
	{"gpt-5.3-codex-spark", 16_000},
	{"gpt-5.1-chat", 16_000},
	{"gpt-5.2-chat", 16_000},
	{"gpt-5", 128_000}, // gpt-5.x default

	// OpenAI GPT-4.1 (32K out)
	{"gpt-4.1", 32_768},

	// OpenAI GPT-4o (16K out)
	{"gpt-4o", 16_384},

	// OpenAI GPT-4 Turbo (4K out)
	{"gpt-4-turbo", 4_096},

	// OpenAI GPT-4 (original)
	{"gpt-4-32k", 32_768},
	{"gpt-4", 8_192},

	// OpenAI GPT-3.5 (4K out)
	{"gpt-3.5-turbo", 4_096},

	// OpenAI o-series
	{"o1-pro", 100_000},
	{"o1-mini", 65_536},
	{"o1", 100_000},
	{"o3-mini", 100_000},
	{"o3", 100_000},
	{"o4-mini", 100_000},

	// Google Gemini
	{"gemini-3-pro", 65_536},
	{"gemini-3-flash", 65_536},
	{"gemini-2.5-pro", 65_536},
	{"gemini-2.5-flash", 65_536},
	{"gemini-2.0-flash", 8_192},
	{"gemini-1.5-pro", 8_192},
	{"gemini-1.5-flash", 8_192},

	// xAI Grok
	{"grok-4-1", 32_000},
	{"grok-4", 64_000},
	{"grok-3", 8_192},
	{"grok-code", 16_384},
	{"grok-2", 8_192},
}
