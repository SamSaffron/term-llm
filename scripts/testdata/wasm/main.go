//go:build js && wasm

// Command wasm-smoke exercises the js/wasm platform seams without a browser.
// It boots the real engine with a scripted provider, executes a registered
// tool, verifies that unsupported platform facilities fail closed, and exits
// non-zero on any mismatch. scripts/wasm-smoke.sh builds and runs it under
// Node with a deliberately small JavaScript stack.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/samsaffron/term-llm/internal/filelock"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
)

type echoTool struct{}

func (echoTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "wasm_echo",
		Description: "Echo text back to the caller.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"text": map[string]interface{}{"type": "string"},
			},
			"required": []string{"text"},
		},
	}
}

func (echoTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return core.ToolOutput{}, err
	}
	return core.TextOutput("echo:" + in.Text), nil
}

func (echoTool) Preview(json.RawMessage) string { return "" }

type smoke struct {
	failures []string
}

func (s *smoke) check(ok bool, format string, args ...any) {
	if !ok {
		s.failures = append(s.failures, fmt.Sprintf(format, args...))
	}
}

func (s *smoke) runEngine() {
	provider := mockprovider.NewMockProvider("wasm-script")
	provider.AddToolCall("call_1", "wasm_echo", map[string]string{"text": "ping"})
	provider.AddTextResponse("wasm-ok")

	registry := engine.NewToolRegistry()
	registry.Register(echoTool{})
	eng := engine.NewEngine(provider, registry)

	stream, err := eng.Stream(context.Background(), core.Request{
		Model:    "mock",
		Messages: []core.Message{core.UserText("exercise the wasm platform")},
		Tools:    []core.ToolSpec{echoTool{}.Spec()},
	})
	if err != nil {
		s.check(false, "engine.Stream: %v", err)
		return
	}
	defer stream.Close()

	var text strings.Builder
	var toolOutput string
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.check(false, "stream.Recv: %v", err)
			return
		}
		switch event.Type {
		case core.EventTextDelta:
			text.WriteString(event.Text)
		case core.EventToolExecEnd:
			s.check(event.ToolSuccess, "tool %s failed", event.ToolName)
			toolOutput = event.ToolOutput
		}
	}

	s.check(strings.Contains(text.String(), "wasm-ok"), "final text %q does not contain wasm-ok", text.String())
	s.check(strings.Contains(toolOutput, "echo:ping"), "tool output %q does not contain echo:ping", toolOutput)
	s.check(len(provider.RecordedRequests()) >= 2, "provider saw %d requests, want 2", len(provider.RecordedRequests()))
}

func (s *smoke) runUnsupportedFacilities() {
	if db, err := sql.Open("sqlite", ":memory:"); err == nil {
		_ = db.Close()
		s.check(false, "sql.Open(sqlite) succeeded on js/wasm; expected unknown driver")
	} else {
		s.check(strings.Contains(err.Error(), "unknown driver"), "sql.Open error %q does not report unknown driver", err)
	}

	if release, err := filelock.Lock("/tmp/term-llm-wasm-smoke.lock"); err == nil {
		_ = release()
		s.check(false, "filelock.Lock succeeded on js/wasm; expected fail-closed error")
	} else {
		s.check(errors.Is(err, errors.ErrUnsupported), "filelock.Lock error %v is not ErrUnsupported", err)
	}

	if release, err := filelock.TryLock("/tmp/term-llm-wasm-smoke.lock"); err == nil {
		_ = release()
		s.check(false, "filelock.TryLock succeeded on js/wasm; expected fail-closed error")
	} else {
		s.check(errors.Is(err, errors.ErrUnsupported), "filelock.TryLock error %v is not ErrUnsupported", err)
	}

	if answers, err := tools.RunAskUser([]tools.AskUserQuestion{{Header: "Smoke"}}); err == nil || answers != nil {
		s.check(false, "RunAskUser = (%v, %v), want nil answers and error", answers, err)
	}

	if outcome, _ := tools.HuhApprovalPrompt(&tools.ApprovalRequest{Description: "smoke"}); outcome != tools.Cancel {
		s.check(false, "HuhApprovalPrompt outcome = %q, want cancel", outcome)
	}
}

func (s *smoke) runToolsMetadata() {
	spec, ok := tools.ToolSpec(tools.ReadFileToolName)
	s.check(ok, "tools.ToolSpec(%q) was not found", tools.ReadFileToolName)
	s.check(spec.Name == tools.ReadFileToolName, "tools.ToolSpec name = %q", spec.Name)
	s.check(len(tools.BuiltinToolDescriptors()) > 0, "builtin tool descriptors are empty")
}

func main() {
	s := &smoke{}
	s.runEngine()
	s.runToolsMetadata()
	s.runUnsupportedFacilities()
	if len(s.failures) > 0 {
		for _, failure := range s.failures {
			fmt.Fprintln(os.Stderr, "wasm smoke:", failure)
		}
		os.Exit(1)
	}
	fmt.Println("wasm smoke: ok")
}
