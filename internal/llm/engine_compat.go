package llm

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// The turn loop, steering and continuation live in internal/llm/engine. The
// declarations below keep the original llm.Engine surface available.

type (
	// Deprecated: use engine.Engine in new packages.
	Engine = engine.Engine
	// Deprecated: use engine.TurnCompletedCallback in new packages.
	TurnCompletedCallback = engine.TurnCompletedCallback
	// Deprecated: use engine.ResponseCompletedCallback in new packages.
	ResponseCompletedCallback = engine.ResponseCompletedCallback
	// Deprecated: use engine.AssistantSnapshotCallback in new packages.
	AssistantSnapshotCallback = engine.AssistantSnapshotCallback
	// Deprecated: use engine.RuntimeSwitch in new packages.
	RuntimeSwitch = engine.RuntimeSwitch
	// Deprecated: use engine.RuntimeSwitchCallback in new packages.
	RuntimeSwitchCallback = engine.RuntimeSwitchCallback
	// Deprecated: use engine.CompactionCallback in new packages.
	CompactionCallback = engine.CompactionCallback
	// Deprecated: use engine.FileTrackingRunLifecycle in new packages.
	FileTrackingRunLifecycle = engine.FileTrackingRunLifecycle
	// Deprecated: use engine.ToolExecutorSetter in new packages.
	ToolExecutorSetter = engine.ToolExecutorSetter
	// Deprecated: use engine.DynamicToolPublisher in new packages.
	DynamicToolPublisher = engine.DynamicToolPublisher
	// Deprecated: use engine.InlineFlusher in new packages.
	InlineFlusher = engine.InlineFlusher
	// Deprecated: use engine.ImmediateInterrupter in new packages.
	ImmediateInterrupter = engine.ImmediateInterrupter
	// Deprecated: use engine.ProviderCleaner in new packages.
	ProviderCleaner = engine.ProviderCleaner
	// Deprecated: use engine.ProviderTurnCleaner in new packages.
	ProviderTurnCleaner = engine.ProviderTurnCleaner
	// Deprecated: use engine.SteeringQueueStatus in new packages.
	SteeringQueueStatus = engine.SteeringQueueStatus
	// Deprecated: use engine.SteeringClaimStatus in new packages.
	SteeringClaimStatus = engine.SteeringClaimStatus
	// Deprecated: use engine.SteeringOrigin in new packages.
	SteeringOrigin = engine.SteeringOrigin
	// Deprecated: use engine.QueuedSteering in new packages.
	QueuedSteering = engine.QueuedSteering
	// Deprecated: use engine.SteeringAvailability in new packages.
	SteeringAvailability = engine.SteeringAvailability
	// Deprecated: use engine.SteeringTransition in new packages.
	SteeringTransition = engine.SteeringTransition
	// Deprecated: use engine.SuspendedError in new packages.
	SuspendedError = engine.SuspendedError
	// Deprecated: use engine.RequestContextTool in new packages.
	RequestContextTool = engine.RequestContextTool
	// Deprecated: use engine.SessionStateResetter in new packages.
	SessionStateResetter = engine.SessionStateResetter

	// Deprecated: use core.ToolSurfacePlanner in new packages.
	ToolSurfacePlanner = core.ToolSurfacePlanner
	// Deprecated: use core.NativeToolDiscoveryPlanner in new packages.
	NativeToolDiscoveryPlanner = core.NativeToolDiscoveryPlanner
	// Deprecated: use core.ToolDiscoveryDiagnostics in new packages.
	ToolDiscoveryDiagnostics = core.ToolDiscoveryDiagnostics
	// Deprecated: use core.ToolDiscoveryServerDiagnostic in new packages.
	ToolDiscoveryServerDiagnostic = core.ToolDiscoveryServerDiagnostic
	// Deprecated: use core.ToolActivationDiagnostic in new packages.
	ToolActivationDiagnostic = core.ToolActivationDiagnostic
	// Deprecated: use core.ToolEvictionDiagnostic in new packages.
	ToolEvictionDiagnostic = core.ToolEvictionDiagnostic
	// Deprecated: use core.ToolDiscoveryDiagnoser in new packages.
	ToolDiscoveryDiagnoser = core.ToolDiscoveryDiagnoser
	// Deprecated: use core.ToolDiscoverySurfaceInspector in new packages.
	ToolDiscoverySurfaceInspector = core.ToolDiscoverySurfaceInspector
	// Deprecated: use core.ClientToolRunner in new packages.
	ClientToolRunner = core.ClientToolRunner
)

const (
	PhaseCompacting                 = engine.PhaseCompacting
	PhaseCompactingWriteBrief       = engine.PhaseCompactingWriteBrief
	PhaseCompactingSummarizeHistory = engine.PhaseCompactingSummarizeHistory
	PhaseCompactingResumeTask       = engine.PhaseCompactingResumeTask

	SteeringQueueTransitioning = engine.SteeringQueueTransitioning
	SteeringQueueRushOwned     = engine.SteeringQueueRushOwned
	SteeringQueueQueued        = engine.SteeringQueueQueued
	SteeringQueueAlreadyQueued = engine.SteeringQueueAlreadyQueued
	SteeringQueueFollowUpOwned = engine.SteeringQueueFollowUpOwned
	SteeringQueueCommitted     = engine.SteeringQueueCommitted
	SteeringQueueRunFinished   = engine.SteeringQueueRunFinished

	SteeringClaimRushOwned     = engine.SteeringClaimRushOwned
	SteeringClaimNotFound      = engine.SteeringClaimNotFound
	SteeringClaimed            = engine.SteeringClaimed
	SteeringClaimFollowUpOwned = engine.SteeringClaimFollowUpOwned
	SteeringClaimCommitted     = engine.SteeringClaimCommitted

	SteeringOriginUser            = engine.SteeringOriginUser
	SteeringOriginReview          = engine.SteeringOriginReview
	SteeringOriginJobNotification = engine.SteeringOriginJobNotification
	SteeringOriginLegacy          = engine.SteeringOriginLegacy

	SteeringInterruptionNotice = engine.SteeringInterruptionNotice

	ToolDiscoverySearchTurnReserve = core.ToolDiscoverySearchTurnReserve
)

var ErrSteeringTransition = engine.ErrSteeringTransition

// NewEngine creates an Engine for provider and tools.
// Deprecated: use engine.NewEngine in new packages.
func NewEngine(provider Provider, tools *ToolRegistry) *Engine {
	return engine.NewEngine(provider, tools)
}

// ExtractToolInfo summarises a tool call for display.
// Deprecated: use engine.ExtractToolInfo in new packages.
func ExtractToolInfo(call ToolCall) string { return engine.ExtractToolInfo(call) }

// SteeringOriginForMessage reports the steering origin recorded on msg.
// Deprecated: use engine.SteeringOriginForMessage in new packages.
func SteeringOriginForMessage(msg Message) SteeringOrigin {
	return engine.SteeringOriginForMessage(msg)
}

// ToolDiscoverySearchCutoff returns the first attempt on which discovery is closed.
// Deprecated: use core.ToolDiscoverySearchCutoff in new packages.
func ToolDiscoverySearchCutoff(maxTurns int) int { return core.ToolDiscoverySearchCutoff(maxTurns) }

// ContextWithClientToolRunner installs the runner that inline calls to
// unregistered tools are offered to.
// Deprecated: use core.ContextWithClientToolRunner in new packages.
func ContextWithClientToolRunner(ctx context.Context, runner ClientToolRunner) context.Context {
	return core.ContextWithClientToolRunner(ctx, runner)
}

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
			var nonRecoverable *NonRecoverableStreamError
			return errors.As(err, &nonRecoverable)
		},
		WithRunContext: withResponsesWebSocketContinuationLifetime,
		WithDebugDiagnosticSink: func(ctx context.Context, logger engine.DebugLogger) context.Context {
			if dl, ok := logger.(*DebugLogger); ok {
				return withDebugDiagnosticSink(ctx, dl)
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
		timeout = opencodeGoRefreshTimeout
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
		RefreshCopilotCacheSync(models)
	}
}

// contextContinuationPrompt is the control prompt the engine appends after
// compaction; root tests and compaction filters refer to it by this name.
const contextContinuationPrompt = engine.ContextContinuationPrompt
