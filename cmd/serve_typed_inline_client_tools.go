package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// Typed WebMCP calls belong to the response that offered them, not to a live
// voice session. The response stream carries the request while an inline CLI
// provider waits; the page posts its answer back to this response only.
const typedClientToolTimeout = 60 * time.Second

type typedClientToolAnswer struct {
	output   string
	done     chan struct{}
	answered bool
}

type typedClientToolRunner struct {
	run      *responseRun
	runtime  *serveRuntime
	names    map[string]bool
	mu       sync.Mutex
	pending  map[string]*typedClientToolAnswer
	answered map[string][sha256.Size]byte
	calls    int
}

func (r *typedClientToolRunner) OwnsClientTool(name string) bool { return r.names[name] }

func (r *typedClientToolRunner) RunClientTool(ctx context.Context, call llm.ToolCall) (llm.ToolOutput, error) {
	if !r.names[call.Name] {
		return llm.ToolOutput{}, fmt.Errorf("client tool %q was not offered", call.Name)
	}
	if r.runtime != nil {
		defer r.runtime.pauseForInteractiveWait()()
	}
	answer := &typedClientToolAnswer{done: make(chan struct{})}
	r.mu.Lock()
	if r.pending == nil {
		r.pending = make(map[string]*typedClientToolAnswer)
	}
	if r.calls >= liveClientToolInlineCallLimit {
		r.mu.Unlock()
		return llm.ToolOutput{}, fmt.Errorf("stopped after %d device tool calls", liveClientToolInlineCallLimit)
	}
	if _, exists := r.answered[call.ID]; exists {
		r.mu.Unlock()
		return llm.ToolOutput{}, fmt.Errorf("duplicate client tool call %q", call.ID)
	}
	if _, exists := r.pending[call.ID]; exists {
		r.mu.Unlock()
		return llm.ToolOutput{}, fmt.Errorf("duplicate client tool call %q", call.ID)
	}
	r.pending[call.ID] = answer
	r.calls++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, call.ID); r.mu.Unlock() }()
	args := string(call.Arguments)
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	if err := r.run.appendEvent("response.client_tool.requested", map[string]any{"call_id": call.ID, "name": call.Name, "arguments": args}); err != nil {
		return llm.ToolOutput{}, fmt.Errorf("request client tool: %w", err)
	}
	timer := time.NewTimer(typedClientToolTimeout)
	defer timer.Stop()
	select {
	case <-answer.done:
		return llm.TextOutput(answer.output), nil
	case <-timer.C:
		return llm.ToolOutput{}, errors.New("the device did not run its tool in time")
	case <-ctx.Done():
		return llm.ToolOutput{}, ctx.Err()
	}
}

func (s *serveServer) typedClientToolRunner(runtime *serveRuntime, tools []llm.ToolSpec) func(string) llm.ClientToolRunner {
	names := make(map[string]bool)
	for _, tool := range tools {
		if strings.HasPrefix(tool.Name, "webmcp__") {
			names[tool.Name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return func(responseID string) llm.ClientToolRunner {
		run, ok := s.ensureResponseRuns().get(responseID)
		if !ok {
			return nil
		}
		runner := &typedClientToolRunner{run: run, runtime: runtime, names: names}
		run.mu.Lock()
		run.typedClientTools = runner
		run.mu.Unlock()
		return runner
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
	runner.mu.Lock()
	answer := runner.pending[callID]
	if answer == nil {
		output, answered := runner.answered[callID]
		runner.mu.Unlock()
		if answered && output == sha256.Sum256([]byte(body.Output)) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true})
			return
		}
		if answered {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "client tool already answered differently")
			return
		}
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "client tool call not found")
		return
	}
	if answer.answered {
		same := answer.output == body.Output
		runner.mu.Unlock()
		if !same {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "client tool already answered differently")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true})
		return
	}
	answer.output = body.Output
	answer.answered = true
	if runner.answered == nil {
		runner.answered = make(map[string][sha256.Size]byte)
	}
	runner.answered[callID] = sha256.Sum256([]byte(body.Output))
	close(answer.done)
	runner.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
