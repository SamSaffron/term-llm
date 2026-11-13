package core

import (
	"context"
	"encoding/json"
)

// ClientToolRunner runs tools the caller declared but term-llm does not
// register: passthrough (client) tools such as a page's WebMCP tools.
//
// Most providers return such a call as the end of their turn, and the engine
// passes it through for the caller to answer in its next request. Providers
// with an inline tool loop (claude-bin and the other CLI bridges) cannot stop
// there: the CLI blocks on the MCP call until it gets a result. A runner lets
// the caller answer that call while the provider waits.
//
// An owned call runs through the same execution path as a registered tool —
// steering freeze, restart tracking, heartbeat, cancellation, truncation — but
// skips allow-lists and approval, as passthrough does: the client decides
// whether to run its own tools.
type ClientToolRunner interface {
	// OwnsClientTool reports whether the runner answers calls to name.
	OwnsClientTool(name string) bool
	// RunClientTool answers one call. The call ID is also on ctx.
	RunClientTool(ctx context.Context, call ToolCall) (ToolOutput, error)
}

type clientToolRunnerContextKey struct{}

// ContextWithClientToolRunner installs the runner that inline calls to
// unregistered tools are offered to. A nil runner clears an inherited one.
func ContextWithClientToolRunner(ctx context.Context, runner ClientToolRunner) context.Context {
	return context.WithValue(ctx, clientToolRunnerContextKey{}, clientToolRunnerBox{runner})
}

// clientToolRunnerBox lets a nil runner be stored, so it can clear one.
type clientToolRunnerBox struct{ runner ClientToolRunner }

func clientToolRunnerFromContext(ctx context.Context) ClientToolRunner {
	if ctx == nil {
		return nil
	}
	box, _ := ctx.Value(clientToolRunnerContextKey{}).(clientToolRunnerBox)
	return box.runner
}

// ScopeClientToolRunner limits the context's runner to the tools req offers.
// The runner rides on the context, so a nested run started from a tool (an
// in-process subagent, say) inherits it; such a run never offered the page's
// tools, so it must not answer calls to them on the parent's behalf.
func ScopeClientToolRunner(ctx context.Context, tools []ToolSpec) context.Context {
	// A child engine run must obtain its own cell/controller rather than using
	// the parent's capability via an inherited tool-execution context.
	ctx = context.WithValue(ctx, codeModeCellKey{}, struct{}{})
	ctx = context.WithValue(ctx, codeModeControllerKey{}, struct{}{})
	ctx = context.WithValue(ctx, codeModePauseKey{}, struct{}{})
	runner := clientToolRunnerFromContext(ctx)
	if runner == nil {
		return ctx
	}
	// A second engine run is a child/resume boundary, not a new grant from the
	// host. In particular, offering the same names cannot inherit a cell lease.
	if _, scoped := runner.(scopedClientToolRunner); scoped {
		return ContextWithClientToolRunner(ctx, nil)
	}
	offered := make(map[string]ToolSpec, len(tools))
	for _, spec := range tools {
		if runner.OwnsClientTool(spec.Name) {
			copy, err := cloneClientToolSpec(spec)
			if err == nil {
				offered[spec.Name] = copy
			}
		}
	}
	if len(offered) == 0 {
		return ContextWithClientToolRunner(ctx, nil)
	}
	return ContextWithClientToolRunner(ctx, scopedClientToolRunner{ClientToolRunner: runner, offered: offered})
}

type scopedClientToolRunner struct {
	ClientToolRunner
	offered map[string]ToolSpec
}

func (r scopedClientToolRunner) OwnsClientTool(name string) bool {
	_, ok := r.offered[name]
	return ok
}

// CaptureClientTools captures only a first-level request-scoped inline runner.
// Ambient runners and ordinary passthrough requests never grant script access.
// The caller must reject collisions with every server-origin registered tool.
func CaptureClientTools(ctx context.Context) (ClientToolRunner, *ClientToolLease, []ToolSpec) {
	r, ok := clientToolRunnerFromContext(ctx).(scopedClientToolRunner)
	if !ok {
		return nil, nil, nil
	}
	provider, ok := r.ClientToolRunner.(ClientToolLeaseProvider)
	if !ok {
		return nil, nil, nil
	}
	lease := provider.ClientToolLease()
	if lease == nil || lease.Check() != nil {
		return nil, nil, nil
	}
	specs := make([]ToolSpec, 0, len(r.offered))
	for name, offered := range r.offered {
		spec, found := lease.Spec(name)
		if !found {
			continue
		}
		// Both the originating host offer and this exact request must agree.
		a, errA := json.Marshal(offered)
		b, errB := json.Marshal(spec)
		if errA == nil && errB == nil && string(a) == string(b) {
			specs = append(specs, spec)
		}
	}
	return r, lease, specs
}

// ClientTool returns a Tool adapter for an unregistered call the context's
// runner owns, or nil.
func ClientTool(ctx context.Context, name string) Tool {
	runner := clientToolRunnerFromContext(ctx)
	if runner == nil || !runner.OwnsClientTool(name) {
		return nil
	}
	return clientToolAdapter{runner: runner, name: name}
}

// clientToolAdapter presents one client tool as a Tool, so the engine runs it
// exactly as it runs a registered one.
type clientToolAdapter struct {
	runner ClientToolRunner
	name   string
}

func (t clientToolAdapter) Spec() ToolSpec                 { return ToolSpec{Name: t.name} }
func (t clientToolAdapter) Preview(json.RawMessage) string { return "" }

func (t clientToolAdapter) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return t.runner.RunClientTool(ctx, ToolCall{ID: CallIDFromContext(ctx), Name: t.name, Arguments: args})
}
