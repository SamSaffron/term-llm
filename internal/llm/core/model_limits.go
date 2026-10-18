package core

import "context"

// ModelLimits carries model-limit lookups that the engine needs but that are
// implemented by packages which depend on core (model tables, provider
// families). It lets the engine consult them without importing those
// packages. Every field is optional; a nil field reports "unknown".
type ModelLimits struct {
	// InputLimit resolves the input token limit for a provider/model pair
	// from static and cached metadata (0 = unknown).
	InputLimit func(providerName, modelName string) int
	// RuntimeInputLimit asks a provider for its live effective input limit.
	// ok is false when the provider cannot report one, in which case the
	// static InputLimit applies. A non-nil err means the provider could
	// report a limit but inspection failed.
	RuntimeInputLimit func(ctx context.Context, provider Provider, providerName, modelName string) (limit int, ok bool, err error)
	// RefreshDynamic refreshes dynamically discovered model metadata
	// (for example a provider's model list) so InputLimit may resolve it.
	RefreshDynamic func(provider Provider, providerName, modelName string)
}

// ResolveInputLimit returns the input limit for a provider/model pair, or 0.
func (m ModelLimits) ResolveInputLimit(providerName, modelName string) int {
	if m.InputLimit == nil {
		return 0
	}
	return m.InputLimit(providerName, modelName)
}

// ResolveRuntimeInputLimit queries the provider's live limit when supported.
func (m ModelLimits) ResolveRuntimeInputLimit(ctx context.Context, provider Provider, providerName, modelName string) (int, bool, error) {
	if m.RuntimeInputLimit == nil {
		return 0, false, nil
	}
	return m.RuntimeInputLimit(ctx, provider, providerName, modelName)
}

// RefreshDynamicLimits refreshes dynamic model metadata when supported.
func (m ModelLimits) RefreshDynamicLimits(provider Provider, providerName, modelName string) {
	if m.RefreshDynamic != nil {
		m.RefreshDynamic(provider, providerName, modelName)
	}
}
