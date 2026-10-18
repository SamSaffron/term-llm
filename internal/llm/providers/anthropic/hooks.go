package anthropic

import (
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

// CuratedModel is a curated fallback model row supplied by the parent llm
// package (its ProviderModels["anthropic"] table).
type CuratedModel struct {
	ID               string
	ReasoningEfforts []string
}

// Hooks lets the parent llm package supply behaviour that depends on the
// shared model registry and config-backed limit tables, which this package
// cannot import without a cycle. Nil fields fall back to neutral defaults
// (no limits, no curated rows, no effort-suffix parsing).
type Hooks struct {
	// BaseModelAndEffort is llm.BaseModelAndEffortForProvider("anthropic", model).
	BaseModelAndEffort func(model string) (base, effort string)
	// InputLimit and OutputLimit are llm.InputLimitForModel / OutputLimitForModel.
	InputLimit  func(model string) int
	OutputLimit func(model string) int
	// Curated returns the curated fallback rows for the anthropic provider.
	Curated func() []CuratedModel
}

// ParentHooks is installed by the parent llm package (typically in init).
var ParentHooks Hooks

func baseModelAndEffort(model string) (string, string) {
	if ParentHooks.BaseModelAndEffort != nil {
		return ParentHooks.BaseModelAndEffort(model)
	}
	return model, ""
}

func inputLimitForModel(model string) int {
	if ParentHooks.InputLimit != nil {
		return ParentHooks.InputLimit(model)
	}
	return 0
}

func outputLimitForModel(model string) int {
	if ParentHooks.OutputLimit != nil {
		return ParentHooks.OutputLimit(model)
	}
	return 0
}

// clampOutputTokens mirrors llm.ClampOutputTokens using the hooked limits.
func clampOutputTokens(requested int, model string) int {
	if requested <= 0 {
		return requested
	}
	if limit := outputLimitForModel(model); limit > 0 && requested > limit {
		return limit
	}
	return requested
}

// ClampOutputTokens caps requested output at the model's curated limit; the
// parent's Bedrock path uses it because Bedrock limits differ from the live
// direct-API catalog.
func ClampOutputTokens(requested int, model string) int {
	return clampOutputTokens(requested, model)
}

func newHTTPStatusError(provider string, resp *http.Response, body []byte) *providerhttp.StatusError {
	return providerhttp.NewStatusError(provider, resp, body)
}

var knownEffortSuffixes = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// trimKnownEffortSuffix removes a trailing reasoning-effort suffix from model.
func trimKnownEffortSuffix(model string) (string, bool) {
	for _, suffix := range knownEffortSuffixes {
		if strings.HasSuffix(model, "-"+suffix) {
			return strings.TrimSuffix(model, "-"+suffix), true
		}
	}
	return model, false
}

func cloneEfforts(efforts []string) []string {
	if len(efforts) == 0 {
		return nil
	}
	return append([]string(nil), efforts...)
}

func schemaRequired(schema map[string]interface{}) []string {
	if schema == nil {
		return nil
	}
	switch v := schema["required"].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func modelInfosFromCache(cached *cache.ModelCache) []core.ModelInfo {
	if cached == nil {
		return nil
	}
	if len(cached.ModelInfos) > 0 {
		models := make([]core.ModelInfo, 0, len(cached.ModelInfos))
		for _, m := range cached.ModelInfos {
			if m.ID == "" {
				continue
			}
			models = append(models, core.ModelInfo{
				ID:                m.ID,
				DisplayName:       m.DisplayName,
				Created:           m.Created,
				OwnedBy:           m.OwnedBy,
				InputLimit:        m.InputLimit,
				OutputLimit:       m.OutputLimit,
				ConfiguredContext: m.ConfiguredContext,
				InputPrice:        m.InputPrice,
				OutputPrice:       m.OutputPrice,
				ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
				ReasoningModes:    cloneEfforts(m.ReasoningModes),
			})
		}
		return models
	}
	models := make([]core.ModelInfo, 0, len(cached.Models))
	for _, id := range cached.Models {
		if id != "" {
			models = append(models, core.ModelInfo{ID: id})
		}
	}
	return models
}

func modelInfosToCache(models []core.ModelInfo) []cache.CachedModel {
	cached := make([]cache.CachedModel, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		cached = append(cached, cache.CachedModel{
			ID:                m.ID,
			DisplayName:       m.DisplayName,
			Created:           m.Created,
			OwnedBy:           m.OwnedBy,
			InputLimit:        m.InputLimit,
			OutputLimit:       m.OutputLimit,
			ConfiguredContext: m.ConfiguredContext,
			InputPrice:        m.InputPrice,
			OutputPrice:       m.OutputPrice,
			ReasoningEfforts:  cloneEfforts(m.ReasoningEfforts),
			ReasoningModes:    cloneEfforts(m.ReasoningModes),
		})
	}
	return cached
}
