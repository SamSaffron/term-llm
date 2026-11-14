package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/term-llm/term-llm/internal/codemode"
	"github.com/term-llm/term-llm/internal/llm/core"
)

// codeModeClientTools is separate from the engine registry: a cell pins one
// request's exact inline runner, lease, provenance and full offered schemas.
// Never recover a client tool from the live context as a dispatch fallback.
type codeModeClientTools struct {
	runner core.ClientToolRunner
	lease  *core.ClientToolLease
	tools  map[string]codeModeClientTool
}

// captureCodeModeClients must run at admission with the entire execution
// snapshot (including denied/direct-only/deferred server tools). Cross-origin
// collisions are errors, not registration order or allow-list decisions.
func captureCodeModeClients(ctx context.Context, registered map[string]core.Tool) (*codeModeClientTools, error) {
	runner, lease, specs := core.CaptureClientTools(ctx)
	if lease == nil || len(specs) == 0 {
		return nil, nil
	}
	clients := &codeModeClientTools{runner: runner, lease: lease, tools: make(map[string]codeModeClientTool, len(specs))}
	for _, spec := range specs {
		if _, collision := registered[spec.Name]; collision || clientControlName(spec.Name) {
			return nil, &codemode.Error{Code: "client_tool_collision", Outcome: "not_dispatched", Message: "client tool conflicts with server origin: " + spec.Name}
		}
		raw, err := json.Marshal(struct {
			Binding core.ClientToolBinding
			Spec    core.ToolSpec
		}{lease.Binding(), spec})
		if err != nil {
			return nil, fmt.Errorf("snapshot client identity: %w", err)
		}
		clients.tools[spec.Name] = codeModeClientTool{owner: clients, spec: spec, hash: fmt.Sprintf("%x", sha256.Sum256(raw))}
	}
	if err := lease.Check(); err != nil {
		return nil, clientCodeModeError(err)
	}
	return clients, nil
}

func clientControlName(name string) bool {
	switch name {
	case codemode.LegacyToolName, codemode.ExecToolName, codemode.WaitToolName, codemode.CancelToolName:
		return true
	}
	return false
}

// identities is merged into the cell ceiling once, before any code executes.
func (c *codeModeClientTools) identities() map[string]nestedIdentity {
	identities := make(map[string]nestedIdentity)
	if c != nil {
		for name, tool := range c.tools {
			identities[name] = nestedIdentity{spec: tool.Spec(), hash: tool.hash}
		}
	}
	return identities
}

// identity handles only this cell's captured client names. The handled result
// must be respected even on expiry; falling back would change tool origin.
func (c *codeModeClientTools) identity(name string, registered map[string]core.Tool) (id nestedIdentity, handled bool, err error) {
	if c == nil {
		return id, false, nil
	}
	tool, ok := c.tools[name]
	if !ok {
		return id, false, nil
	}
	if _, collision := registered[name]; collision {
		return id, true, &codemode.Error{Code: "client_tool_collision", Outcome: "not_dispatched", Message: "client tool now conflicts with server origin: " + name}
	}
	if err = c.lease.Check(); err != nil {
		return id, true, clientCodeModeError(err)
	}
	return nestedIdentity{spec: tool.Spec(), hash: tool.hash}, true, nil
}

// tool returns only a frozen client override, never a generic nested fallback.
func (c *codeModeClientTools) tool(name string) (core.Tool, bool) {
	if c == nil {
		return nil, false
	}
	t, ok := c.tools[name]
	return t, ok
}

type codeModeClientTool struct {
	owner *codeModeClientTools
	spec  core.ToolSpec
	hash  string
}

func (t codeModeClientTool) Spec() core.ToolSpec {
	// The lease owns the immutable full schema. Spec callers receive a copy.
	spec, _ := t.owner.lease.Spec(t.spec.Name)
	return spec
}
func (t codeModeClientTool) Preview(json.RawMessage) string { return "on the user's device" }
func (t codeModeClientTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Allowed: true}
}
func (t codeModeClientTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	// Client delivery/cancellation must not need the shared execution permits
	// held by the device waiter. Reacquire only to complete normal bookkeeping.
	resume, err := pauseCodeModeClientTool(ctx)
	if err != nil {
		return core.ToolOutput{}, &core.ClientToolError{Code: "cancelled", Outcome: "not_dispatched", Err: err}
	}
	output, err := t.owner.lease.Dispatch(ctx, t.owner.runner, core.ToolCall{ID: core.CallIDFromContext(ctx), Name: t.spec.Name, Arguments: args})
	if resumeErr := resume(); err == nil && resumeErr != nil {
		return core.ToolOutput{}, &core.ClientToolError{Code: "cancelled", Outcome: "outcome_unknown", Err: resumeErr}
	}
	return output, err
}

func pauseCodeModeClientTool(ctx context.Context) (func() error, error) {
	// Legacy synchronous cells also park their process permits while waiting
	// for the device; their parent tool context does not install the async hook.
	if lease, _ := ctx.Value(nestedContextKey{}).(*nestedLease); lease != nil {
		return lease.pause()
	}
	return core.PauseCodeModeTool(ctx)
}

// isCodeModeClientTool is the narrow server-allowlist exemption for the exact
// cell-owned override. Do not exempt arbitrary ClientToolRunner adapters.
func isCodeModeClientTool(tool core.Tool) bool {
	_, ok := tool.(codeModeClientTool)
	return ok
}

// clientCodeModeError must be used when Call projects a dispatch failure; a
// pre-dispatch lease rejection must not be relabeled outcome_unknown.
func clientCodeModeError(err error) error {
	var clientErr *core.ClientToolError
	if errors.As(err, &clientErr) {
		return &codemode.Error{Code: clientErr.Code, Outcome: clientErr.Outcome, Message: err.Error()}
	}
	return err
}
