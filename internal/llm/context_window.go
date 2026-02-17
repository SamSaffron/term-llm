package llm

import (
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

// InputLimitForModel returns the effective input token limit for a known model
// using canonical (direct API) numbers. This is the maximum number of tokens
// that can be sent as input — not the total context window (input + output).
// Returns 0 for unknown models.
// For provider-specific limits, use InputLimitForProviderModel instead.
func InputLimitForModel(model string) int {
	return lookupPrefix(strings.ToLower(model), inputLimitTable)
}

// InputLimitForProviderModel returns the effective input token limit for a model
// accessed through a specific provider. Providers like GitHub Copilot impose
// their own limits that differ from the model's canonical input limit.
// The providerName can be either a provider type (e.g., "copilot") or a
// custom provider name — it is resolved to the underlying type automatically.
// Falls back to canonical numbers if no provider-specific data exists.
func InputLimitForProviderModel(providerName, model string) int {
	model = strings.ToLower(model)

	// Resolve provider name to type (e.g., "my-copilot" -> "copilot")
	providerType := string(config.InferProviderType(providerName, ""))

	// Check provider-specific overrides first
	if table, ok := providerInputOverrides[providerType]; ok {
		if result := lookupPrefix(model, table); result > 0 {
			return result
		}
	}

	// Fall back to canonical table
	return lookupPrefix(model, inputLimitTable)
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

type inputLimitEntry struct {
	prefix string
	tokens int
}

func lookupPrefix(model string, table []inputLimitEntry) int {
	best := 0
	bestLen := 0
	for _, e := range table {
		if strings.HasPrefix(model, e.prefix) && len(e.prefix) > bestLen {
			best = e.tokens
			bestLen = len(e.prefix)
		}
	}
	return best
}

// inputLimitTable contains effective input token limits from models.dev/api.json.
// These represent the maximum INPUT tokens (not total context = input + output).
// Compaction must trigger before hitting this limit. Values are computed as:
//   - limit.input if explicitly set (GPT-5 family)
//   - limit.context - limit.output if both are set
//   - limit.context as fallback (conservative upper bound)
//
// Entries are matched by longest prefix. Unknown models return 0 (compaction disabled).
var inputLimitTable = []inputLimitEntry{
	// Anthropic Claude (from models.dev anthropic section: context - output)
	// Claude 4.6: 200K ctx - 64K out = 136K input
	{"claude-sonnet-4-6", 136_000},
	// technically supports 128 out but in practice will not happen
	{"claude-opus-4-6", 136_000},
	// Claude 4.5: 200K ctx - 64K out = 136K input
	{"claude-sonnet-4-5", 136_000},
	{"claude-opus-4-5", 136_000},
	{"claude-haiku-4-5", 136_000},
	// Claude 4.0: 200K ctx - 64K out = 136K input
	{"claude-sonnet-4", 136_000},
	{"claude-opus-4", 168_000}, // 200K - 32K
	{"claude-haiku-4", 136_000},
	// Claude 3.x: 200K ctx - 8K out = 192K input
	{"claude-3.5-sonnet", 192_000},
	{"claude-3.5-haiku", 192_000},
	{"claude-3-opus", 196_000}, // 200K - 4K
	{"claude-3-sonnet", 196_000},
	{"claude-3-haiku", 196_000},

	// OpenAI GPT-5 family (from models.dev openai section: explicit input=272000)
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
var providerInputOverrides = map[string][]inputLimitEntry{
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
}
