package chat

import (
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/runboundary"
)

// captureProviderContext exports the live provider's opaque transport state
// together with the identity and working directory it belongs to.
//
// Provider key, model and working directory travel with the state because
// eligibility to branch from it depends on all of them: Claude Code keys its
// sessions by project directory, and a model or provider swap makes an older
// session the wrong thing to resume.
func (m *Model) captureProviderContext() runboundary.ProviderContext {
	if m == nil {
		return runboundary.ProviderContext{}
	}
	return m.providerContextCapture()()
}

// beginMainStreamEpoch records that the UI has committed to a stream that will
// use the live provider.
//
// MainRunManager.Start happens later, inside the command the update loop
// returns, so a side question's fork gate cannot rely on HasActive alone: there
// is a window where the UI has committed but the manager has not been told. The
// epoch closes it, and it is an atomic because the side question re-checks the
// gate from its own goroutine.
func (m *Model) beginMainStreamEpoch() {
	if m != nil {
		m.mainStreamEpoch.Add(1)
	}
}

// providerContextCapture binds the provider identity once, on the caller's
// goroutine, and returns a function that exports state from it later. Callbacks
// that fire during a run use this so a live model-switch cannot race the export.
func (m *Model) providerContextCapture() func() runboundary.ProviderContext {
	if m == nil {
		return func() runboundary.ProviderContext { return runboundary.ProviderContext{} }
	}
	provider, providerKey, model := m.provider, m.providerKey, m.modelName
	workingDir := m.effectiveWorkingDir()
	return func() runboundary.ProviderContext {
		captured := runboundary.ProviderContext{WorkingDir: workingDir, ProviderKey: providerKey, Model: model}
		if exporter, ok := provider.(llm.ProviderStateExporter); ok {
			if state, exported := exporter.ExportProviderState(); exported {
				captured.State = state
			}
		}
		return captured
	}
}
