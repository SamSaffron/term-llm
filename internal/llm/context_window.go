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

func configInputLimitForProvider(provider, model string) int {
	configMu.RLock()
	defer configMu.RUnlock()
	if v, ok := configProviderInputLimits[configKey(strings.ToLower(provider), model)]; ok {
		return v
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
// accessed through a specific provider. Providers like GitHub Copilot impose
// their own limits that differ from the model's canonical input limit.
// The providerName can be either a provider type (e.g., "copilot") or a
// custom provider name — it is resolved to the underlying type automatically.
// Falls back to canonical numbers if no provider-specific data exists.
func InputLimitForProviderModel(providerName, model string) int {
	model = strings.ToLower(model)

	// Resolve provider name to type (e.g., "my-copilot" -> "copilot",
	// or "acme" -> "venice" via registered aliases).
	providerType := resolveProviderType(providerName)

	// 1. Explicit provider-scoped limits from ProviderModels
	if v, ok := explicitProviderInput[configKey(providerType, model)]; ok {
		return v
	}
	if providerType != providerName {
		if v, ok := explicitProviderInput[configKey(providerName, model)]; ok {
			return v
		}
	}

	// 2. Provider-specific overrides (e.g., Copilot)
	if table, ok := providerInputOverrides[providerType]; ok {
		if result := lookupPrefix(model, table); result > 0 {
			return result
		}
	}

	// 3. Prefix-matched canonical tables
	if result := lookupPrefix(model, inputLimitTable); result > 0 {
		return result
	}

	// 4. Config fallback (provider-scoped, then model-only)
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
	// Enabled via -1m suffix (sends context-1m-2025-08-07 beta header).
	// Requires Anthropic usage tier 4 or custom rate limits.
	{"claude-sonnet-4-6-1m", 980_000},
	{"claude-sonnet-4-5-1m", 980_000},
	{"claude-sonnet-4-1m", 980_000},
	{"claude-opus-4-7-1m", 980_000},
	{"claude-opus-4-6-1m", 980_000},

	// Anthropic Claude 4.x: 200K ctx - 20K practical output reserve = 180K
	{"claude-opus-4-7", 180_000},
	{"claude-sonnet-4-6", 180_000},
	{"claude-opus-4-6", 180_000},
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
	{"us.anthropic.claude-opus-4-7", 180_000},
	{"us.anthropic.claude-sonnet-4-6", 180_000},
	{"us.anthropic.claude-opus-4-6", 180_000},
	{"us.anthropic.claude-haiku-4-5", 180_000},
	{"us.anthropic.claude-sonnet-4-5", 180_000},
	{"us.anthropic.claude-opus-4-5", 180_000},
	{"us.anthropic.claude-sonnet-4", 180_000},
	{"eu.anthropic.claude-opus-4-7", 180_000},
	{"eu.anthropic.claude-sonnet-4-6", 180_000},
	{"eu.anthropic.claude-opus-4-6", 180_000},
	{"eu.anthropic.claude-haiku-4-5", 180_000},
	{"eu.anthropic.claude-sonnet-4-5", 180_000},
	{"eu.anthropic.claude-opus-4-5", 180_000},
	{"eu.anthropic.claude-sonnet-4", 180_000},
	{"ap.anthropic.claude-opus-4-7", 180_000},
	{"ap.anthropic.claude-sonnet-4-6", 180_000},
	{"ap.anthropic.claude-opus-4-6", 180_000},
	{"ap.anthropic.claude-haiku-4-5", 180_000},
	{"ap.anthropic.claude-sonnet-4-5", 180_000},
	{"ap.anthropic.claude-opus-4-5", 180_000},
	{"ap.anthropic.claude-sonnet-4", 180_000},
	{"anthropic.claude-opus-4-7", 180_000},
	{"anthropic.claude-sonnet-4-6", 180_000},
	{"anthropic.claude-opus-4-6", 180_000},
	{"anthropic.claude-haiku-4-5", 180_000},
	{"anthropic.claude-sonnet-4-5", 180_000},
	{"anthropic.claude-opus-4-5", 180_000},
	{"anthropic.claude-sonnet-4", 180_000},

	// OpenAI GPT-5 family
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
// Values are context - output (effective input), from models.dev/api.json.
var providerInputOverrides = map[string][]limitEntry{
	// GitHub Copilot imposes its own limits (from models.dev github-copilot section)
	"copilot": {
		{"claude-haiku-4.5", 96_000},  // 128K - 32K
		{"claude-opus-4.6", 64_000},   // 128K - 64K
		{"claude-opus-4.5", 96_000},   // 128K - 32K
		{"claude-sonnet-4.5", 96_000}, // 128K - 32K
		{"claude-sonnet-4", 112_000},  // 128K - 16K
		{"gemini-3-pro", 64_000},      // 128K - 64K
		{"gemini-3-flash", 64_000},    // 128K - 64K
		{"gemini-2.5-pro", 64_000},    // 128K - 64K
		{"gpt-5.4", 922_000},          // 1,050,000 ctx - 128,000 out (same as canonical)
		{"gpt-5.3-codex", 272_000},    // 400K ctx, input=272K
		{"gpt-5.2-codex", 144_000},    // 272K - 128K
		{"gpt-5.2", 64_000},           // 128K - 64K
		{"gpt-5.1-codex", 64_000},     // 128K ctx but out=128K; conservative
		{"gpt-5.1", 64_000},           // 128K - 64K
		{"gpt-5-mini", 64_000},        // 128K - 64K
		{"gpt-5", 64_000},             // 128K - 128K output is suspicious; use 64K
		{"gpt-4.1", 48_000},           // 64K - 16K
		{"gpt-4o", 48_000},            // 64K - 16K
		{"grok-code", 64_000},         // 128K - 64K
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
	// Anthropic Claude 4.x: theoretical max is 64K-128K but practical is 16K-20K.
	// Use the actual API max to avoid rejections.
	{"claude-sonnet-4-6-1m", 64_000},
	{"claude-sonnet-4-5-1m", 64_000},
	{"claude-sonnet-4-1m", 64_000},
	{"claude-opus-4-7-1m", 64_000},
	{"claude-opus-4-6-1m", 64_000},
	{"claude-opus-4-7", 64_000},
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
	{"us.anthropic.claude-opus-4-7", 64_000},
	{"us.anthropic.claude-sonnet-4-6", 64_000},
	{"us.anthropic.claude-opus-4-6", 64_000},
	{"us.anthropic.claude-haiku-4-5", 64_000},
	{"us.anthropic.claude-sonnet-4-5", 64_000},
	{"us.anthropic.claude-opus-4-5", 64_000},
	{"us.anthropic.claude-sonnet-4", 64_000},
	{"eu.anthropic.claude-opus-4-7", 64_000},
	{"eu.anthropic.claude-sonnet-4-6", 64_000},
	{"eu.anthropic.claude-opus-4-6", 64_000},
	{"eu.anthropic.claude-haiku-4-5", 64_000},
	{"eu.anthropic.claude-sonnet-4-5", 64_000},
	{"eu.anthropic.claude-opus-4-5", 64_000},
	{"eu.anthropic.claude-sonnet-4", 64_000},
	{"ap.anthropic.claude-opus-4-7", 64_000},
	{"ap.anthropic.claude-sonnet-4-6", 64_000},
	{"ap.anthropic.claude-opus-4-6", 64_000},
	{"ap.anthropic.claude-haiku-4-5", 64_000},
	{"ap.anthropic.claude-sonnet-4-5", 64_000},
	{"ap.anthropic.claude-opus-4-5", 64_000},
	{"ap.anthropic.claude-sonnet-4", 64_000},
	{"anthropic.claude-opus-4-7", 64_000},
	{"anthropic.claude-sonnet-4-6", 64_000},
	{"anthropic.claude-opus-4-6", 64_000},
	{"anthropic.claude-haiku-4-5", 64_000},
	{"anthropic.claude-sonnet-4-5", 64_000},
	{"anthropic.claude-opus-4-5", 64_000},
	{"anthropic.claude-sonnet-4", 64_000},

	// OpenAI GPT-5 family
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
