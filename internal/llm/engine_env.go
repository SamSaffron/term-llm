package llm

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/debug"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	rp "github.com/samsaffron/term-llm/internal/llm/providers/responsesproviders"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

func init() {
	engine.SetEnv(engine.Env{
		CompactionEnv: compactionEnv,
		ModelLimits: core.ModelLimits{
			InputLimit:        InputLimitForProviderModel,
			RuntimeInputLimit: providerRuntimeInputLimit,
			RefreshDynamic:    refreshDynamicModelLimitsForContext,
		},
		UnwrapProvider: func(p core.Provider) core.Provider {
			if retry, ok := p.(*RetryProvider); ok {
				return retry.inner
			}
			return p
		},
		IsNonRecoverableStreamError: func(err error) bool {
			var nonRecoverable *responses.NonRecoverableStreamError
			return errors.As(err, &nonRecoverable)
		},
		WithRunContext: responses.WithWebSocketContinuationLifetime,
		WithDebugDiagnosticSink: func(ctx context.Context, logger engine.DebugLogger) context.Context {
			if dl, ok := logger.(*debug.Logger); ok {
				return debug.WithDiagnosticSink(ctx, dl)
			}
			return ctx
		},
		DebugToolCall:      DebugToolCall,
		DebugToolResult:    DebugToolResult,
		DebugRawRequest:    DebugRawRequest,
		DebugRawEvent:      DebugRawEvent,
		DebugRawToolResult: DebugRawToolResult,
	})
}

// providerRuntimeInputLimit asks provider for its live effective input limit.
// Runtime-aware providers must win over canonical model-prefix tables.
func providerRuntimeInputLimit(ctx context.Context, provider core.Provider, providerName, modelName string) (int, bool, error) {
	if scoped, ok := provider.(interface {
		EffectiveInputLimitForProvider(context.Context, string, string) (int, error)
	}); ok {
		if _, hasLimiter := runtimeLimiter(provider); hasLimiter {
			limit, err := scoped.EffectiveInputLimitForProvider(ctx, providerName, modelName)
			return limit, true, err
		}
	}
	limiter, ok := runtimeLimiter(provider)
	if !ok {
		return 0, false, nil
	}
	limit, err := limiter(ctx, modelName)
	return limit, true, err
}

func runtimeLimiter(provider core.Provider) (func(context.Context, string) (int, error), bool) {
	if l, ok := provider.(interface {
		EffectiveInputLimit(context.Context, string) (int, error)
	}); ok {
		return l.EffectiveInputLimit, true
	}
	if l, ok := provider.(interface {
		effectiveInputLimit(context.Context, string) (int, error)
	}); ok {
		return l.effectiveInputLimit, true
	}
	return nil, false
}

func refreshDynamicModelLimitsForContext(provider core.Provider, providerName, modelName string) {
	providerType := resolveProviderType(providerName)
	if (providerType != "copilot" && providerType != "opencode-go") || strings.TrimSpace(modelName) == "" {
		return
	}
	timeout := 30 * time.Second
	if providerType == "opencode-go" {
		timeout = rp.OpenCodeGoRefreshTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if providerType == "opencode-go" {
		refresher, ok := provider.(interface {
			RefreshModelMetadata(context.Context) error
		})
		if !ok {
			return
		}
		if err := refresher.RefreshModelMetadata(ctx); err != nil {
			runtimeoutput.Debug("failed to refresh dynamic model metadata for context limits", "provider", providerName, "model", modelName, "error", err)
		}
		return
	}
	lister, ok := provider.(interface {
		ListModels(context.Context) ([]core.ModelInfo, error)
	})
	if !ok {
		return
	}
	models, err := lister.ListModels(ctx)
	if err != nil {
		runtimeoutput.Debug("failed to refresh dynamic model metadata for context limits", "provider", providerName, "model", modelName, "error", err)
		return
	}
	if providerType == "copilot" {
		rp.RefreshCopilotCacheSync(models)
	}
}
