package serve

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
)

// Typed WebMCP calls belong to the response that offered them, not to a live
// voice session. The response stream carries the request while an inline CLI
// provider waits; the page posts its answer back to this response only.
const typedClientToolTimeout = 120 * time.Second

type typedClientToolRunner struct {
	run     *responseRun
	runtime *serveRuntime
	names   map[string]bool
	specs   []core.ToolSpec
	lease   *core.ClientToolLease // nil until a reliable host lifecycle is attached
	mu      sync.Mutex
	mailbox *core.ClientToolMailbox
}

func (r *typedClientToolRunner) OwnsClientTool(name string) bool { return r.names[name] }

func (r *typedClientToolRunner) ClientToolLease() *core.ClientToolLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease
}

func (r *typedClientToolRunner) clientMailbox() *core.ClientToolMailbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mailbox == nil {
		r.mailbox = core.NewClientToolMailbox(r.names, liveClientToolInlineCallLimit, typedClientToolTimeout, r.publishClientTool)
	}
	return r.mailbox
}

func (r *typedClientToolRunner) publishClientTool(ctx context.Context, call core.ToolCall) error {
	payload := map[string]any{"call_id": call.ID, "name": call.Name, "arguments": string(call.Arguments)}
	if core.ClientToolCallLease(ctx) != nil {
		payload["foreground_only"] = true
	}
	r.run.mu.Lock()
	defer r.run.mu.Unlock()
	if r.run.status != "in_progress" || r.run.cancelRequested {
		return errors.New("originating response ended before client publication")
	}
	if err := core.BeginClientToolDispatch(ctx); err != nil {
		return err
	}
	return r.run.appendEventLocked("response.client_tool.requested", payload, false)
}

func (r *typedClientToolRunner) RunClientTool(ctx context.Context, call core.ToolCall) (core.ToolOutput, error) {
	if r.runtime != nil {
		defer r.runtime.pauseForInteractiveWait()()
	}
	return r.clientMailbox().RunClientTool(ctx, call)
}

func (r *typedClientToolRunner) pendingSnapshot() []map[string]string {
	return r.clientMailbox().RecoveryPending()
}

func (s *serveServer) typedClientToolRunner(runtime *serveRuntime, tools []core.ToolSpec) func(string) core.ClientToolRunner {
	names, specs := core.ClientToolSpecsWithPrefix(tools, "webmcp__")
	if len(names) == 0 {
		return nil
	}
	return func(responseID string) core.ClientToolRunner {
		run, ok := s.ensureResponseRuns().get(responseID)
		if !ok {
			return nil
		}
		runner := &typedClientToolRunner{run: run, runtime: runtime, names: names, specs: specs}
		run.mu.Lock()
		run.typedClientTools = runner
		run.mu.Unlock()
		return runner
	}
}

// typedForegroundClientToolRunner pins the ORIGINAL streaming POST, not any
// later replay subscriber. Returning/disconnecting that stream irreversibly
// revokes its leases even when the host response and a script survive it.
func (s *serveServer) typedForegroundClientToolRunner(ctx context.Context, runtime *serveRuntime, tools []core.ToolSpec, enabled bool) (context.Context, func(string) core.ClientToolRunner, func()) {
	if !enabled {
		return ctx, nil, func() {}
	}
	factory := s.typedClientToolRunner(runtime, tools)
	if factory == nil {
		return ctx, nil, func() {}
	}
	foreground := core.NewClientToolForeground(ctx, "typed_"+randomSuffix())
	return foreground.Context(ctx), func(responseID string) core.ClientToolRunner {
		runner, ok := factory(responseID).(*typedClientToolRunner)
		if !ok {
			return nil
		}
		runner.mu.Lock()
		runner.lease = foreground.Bind(responseID, runner.run.sessionID, runner.specs)
		runner.mu.Unlock()
		return runner
	}, foreground.Close
}

func (r *responseRun) revokeClientToolLeaseLocked(reason string) {
	if r.clientToolLease != nil {
		r.clientToolLease.Revoke(reason)
	}
}

func (s *serveServer) handleTypedClientToolResult(w http.ResponseWriter, r *http.Request, run *responseRun, callID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	var body struct {
		Output string `json:"output"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(body.Output) > 512<<10 {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "client tool output too large")
		return
	}
	run.mu.Lock()
	runner := run.typedClientTools
	run.mu.Unlock()
	if runner == nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "client tool call not found")
		return
	}
	switch runner.clientMailbox().Answer(callID, body.Output) {
	case core.ClientToolAnswerAccepted:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case core.ClientToolAnswerDuplicate:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true})
	case core.ClientToolAnswerConflict:
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "client tool already answered differently")
	default:
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "client tool call not found")
	}
}

func (r *responseRun) writeForegroundSafeEvent(ctx context.Context, w io.Writer, event responseRunEvent) error {
	if event.Event == "response.client_tool.requested" {
		r.mu.Lock()
		lease := r.clientToolLease
		r.mu.Unlock()
		if !core.ClientToolEventDeliverable(ctx, event.Data, lease) {
			return nil
		}
	}
	return writeStoredResponseEvent(w, event)
}
