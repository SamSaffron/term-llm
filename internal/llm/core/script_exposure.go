package core

// CodeModeExposure declares whether a registered tool may be composed in a script.
// It is independent of permission classification: allowing exposure never grants
// permission, registers a tool, or supplies a missing runtime binding. The zero
// value is direct-only. Reason is a stable exclusion identifier for denied tools.
type CodeModeExposure struct {
	Allowed bool
	Reason  string
}

// CodeModeExposureProvider is an optional declaration on the actual registered
// tool instance. Tools without a declaration remain directly callable, but are
// excluded from scripts. Engines must still enforce lifecycle, finishing,
// identity, binding, and live permission checks independently of this declaration.
type CodeModeExposureProvider interface {
	CodeModeExposure() CodeModeExposure
}

// CodeModeLifecycleTool marks script execution/observation controls. Like a
// FinishingTool, a lifecycle control cannot be admitted even if another adapter
// declares allowed exposure. This is a type contract, never a public-name test.
type CodeModeLifecycleTool interface {
	IsCodeModeLifecycleTool() bool
}
