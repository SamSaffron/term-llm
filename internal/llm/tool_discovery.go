package llm

import "context"

// ToolSurfacePlanner owns provider visibility for dynamically discoverable tools.
// Implementations must not change a surface during an in-flight provider stream.
type ToolSurfacePlanner interface {
	BeginRun(ctx context.Context, provider Provider, req *Request, runID string) (resetReason string, err error)
	PrepareTurn(ctx context.Context, provider Provider, req *Request, runID string, attempt, maxTurns int) (resetReason string, err error)
	EndRun(runID string)
	ResetSession(sessionID string)
	ToolExecuted(sessionID, name string)
	CanActivateDeferredTool(name string) bool
	AllowsPlannerTool(runID, name string) bool
	ResolveProviderToolCall(runID string, call ToolCall) (ToolCall, error)
}

// NativeToolDiscoveryPlanner is the provider-neutral engine contract for native
// client-executed discovery. The engine owns call orchestration; planners own
// search, policy, trusted schema selection, and one-shot fallback decisions.
type NativeToolDiscoveryPlanner interface {
	ResolveNativeToolDiscovery(ctx context.Context, runID string, call ToolDiscoveryCall) (ToolDiscoveryOutput, error)
	FallbackNativeToolDiscovery(runID string, cause error, committed bool) (fallback bool, reason string)
}

// ToolDiscoverySearchTurnReserve is the minimum number of provider turns needed
// after discovery: one to call an activated tool and one answer-only final turn.
const ToolDiscoverySearchTurnReserve = 2

// ToolDiscoverySearchCutoff returns the first attempt on which discovery is closed.
func ToolDiscoverySearchCutoff(maxTurns int) int {
	cutoff := maxTurns - ToolDiscoverySearchTurnReserve
	if cutoff < 0 {
		return 0
	}
	return cutoff
}

// ToolDiscoveryDiagnostics is a provider-neutral inspect projection.
type ToolDiscoveryDiagnostics struct {
	ConfiguredMode     string
	ResolvedMode       string
	ConfiguredStrategy string
	Strategy           string
	Reason             string
	StrategyReason     string
	FallbackCount      int
	FallbackReason     string
	CatalogueHash      string
	CatalogueGen       uint64
	PinnedCount        int
	ActiveMCPCount     int
	DeferredCount      int
	PinnedTokens       int
	ActiveMCPTokens    int
	DeferredTokens     int
	DynamicActive      int
	DynamicLimit       int
	Recent             []ToolActivationDiagnostic
	Servers            []ToolDiscoveryServerDiagnostic
	ResetReason        string
}

// ToolDiscoveryServerDiagnostic reports the effective surface for one MCP server.
type ToolDiscoveryServerDiagnostic struct {
	Name         string
	ResolvedMode string
	Total        int
	Pinned       int
	Active       int
	Deferred     int
}

// ToolActivationDiagnostic records a recent model-driven activation.
type ToolActivationDiagnostic struct {
	Name      string
	Namespace string
	ChildName string
	Reason    string
}

// ToolDiscoveryDiagnoser is implemented by planners that expose inspect data.
type ToolDiscoveryDiagnoser interface {
	Diagnostics(sessionID string) ToolDiscoveryDiagnostics
}
