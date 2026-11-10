package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type controlFake struct {
	code       string
	cellID     string
	yield      time.Duration
	output     int
	waitCalled bool
	cancelCall string
	execResult core.CodeModeCallResult
}

func (f *controlFake) ExecCodeMode(_ context.Context, code string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	f.code, f.yield, f.output = code, yield, maxOutputTokens
	return f.execResult, nil
}
func (f *controlFake) WaitCodeMode(_ context.Context, cellID string, yield time.Duration, maxOutputTokens int) (core.CodeModeCallResult, error) {
	f.waitCalled, f.cellID, f.yield, f.output = true, cellID, yield, maxOutputTokens
	return f.execResult, nil
}
func (f *controlFake) CancelCodeMode(_ context.Context, cellID string) (core.CodeModeCallResult, error) {
	f.cancelCall = cellID
	return f.execResult, nil
}

func controlContext(controller core.CodeModeController) context.Context {
	ctx := core.ContextWithCallID(context.Background(), "call-outer")
	return core.ContextWithCodeModeController(ctx, controller)
}

func decodeEnvelope(t *testing.T, output core.ToolOutput) map[string]json.RawMessage {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output.Content), &envelope); err != nil {
		t.Fatalf("envelope is not JSON: %v: %q", err, output.Content)
	}
	return envelope
}

func TestControlToolSchemasUseExactNames(t *testing.T) {
	if got := ControlToolNames(); strings.Join(got, ",") != "spawn_script,wait_script,cancel_script" {
		t.Fatalf("control names %v", got)
	}
	if Name != "code_mode" || New().Spec().Name != "code_mode" {
		t.Fatal("legacy code_mode changed")
	}
	for _, spec := range []core.ToolSpec{NewExec().Spec(), NewWait().Spec(), NewCancel().Spec()} {
		if spec.Name != string(ExecName) && spec.Name != string(WaitName) && spec.Name != string(CancelName) {
			t.Fatalf("unexpected name %q", spec.Name)
		}
		props, ok := spec.Schema["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s schema", spec.Name)
		}
		if spec.Name == ExecName || spec.Name == WaitName {
			if _, ok := props["yield_time_ms"]; !ok {
				t.Fatalf("%s missing yield_time_ms", spec.Name)
			}
			if _, ok := props["max_output_tokens"]; !ok {
				t.Fatalf("%s missing max_output_tokens", spec.Name)
			}
		}
		if _, ok := props["yield_ms"]; ok {
			t.Fatalf("%s advertised private alias", spec.Name)
		}
	}
}

func TestExecRendersOrderedPartsAndMetadataEnvelope(t *testing.T) {
	fake := &controlFake{execResult: core.CodeModeCallResult{
		CellID: "cm_1", Status: "running", Yield: 1 * time.Millisecond, Elapsed: 5 * time.Millisecond,
		MaxOutputTokens: 7, StateCommitted: false, HasMoreOutput: true, OutputLimited: true,
		Parts: []core.ToolContentPart{
			{Type: core.ToolContentPartText, Text: "hello"},
			{Type: core.ToolContentPartImageData, ImageData: &core.ToolImageData{MediaType: "image/png", Base64: "abc"}},
		},
	}}
	output, err := NewExec().Execute(controlContext(fake), json.RawMessage(`{"code":"text(1)","yield_time_ms":1,"max_output_tokens":7}`))
	if err != nil {
		t.Fatal(err)
	}
	if fake.code != "text(1)" || fake.yield != time.Millisecond || fake.output != 7 {
		t.Fatalf("controller args: %+v", fake)
	}
	if len(output.ContentParts) != 3 {
		t.Fatalf("ordered parts: %+v", output.ContentParts)
	}
	if output.ContentParts[0].Text != "hello" {
		t.Fatalf("first part: %+v", output.ContentParts[0])
	}
	if output.ContentParts[1].Type != core.ToolContentPartImageData {
		t.Fatalf("image part: %+v", output.ContentParts[1])
	}
	if !strings.HasPrefix(output.ContentParts[2].Text, "\n") {
		t.Fatalf("trailing envelope: %q", output.ContentParts[2].Text)
	}
	if strings.Contains(output.Content, "abc") {
		t.Fatal("media bytes leaked into envelope")
	}
	if output.PreserveJSON != true || output.IsError {
		t.Fatalf("output flags: %+v", output)
	}
	envelope := decodeEnvelope(t, output)
	for _, key := range []string{"cell_id", "status", "state_committed", "max_output_tokens", "yield_time_ms", "tool_call_id"} {
		if _, ok := envelope[key]; !ok {
			t.Fatalf("envelope missing %s: %s", key, output.Content)
		}
	}
	if string(envelope["cell_id"]) != `"cm_1"` || string(envelope["status"]) != `"running"` {
		t.Fatalf("envelope identity: %s", output.Content)
	}
	if string(envelope["tool_call_id"]) != `"call-outer"` {
		t.Fatalf("tool_call_id: %s", output.Content)
	}
	if _, ok := envelope["state"]; ok {
		t.Fatalf("state values leaked: %s", output.Content)
	}
}

func TestWaitAndCancelRenderAndAcceptPrivateYieldAlias(t *testing.T) {
	fake := &controlFake{execResult: core.CodeModeCallResult{CellID: "cm_2", Status: "completed", StateCommitted: true, StateBytes: 12, Yield: 250 * time.Millisecond}}
	output, err := NewWait().Execute(controlContext(fake), json.RawMessage(`{"cell_id":"cm_2","yield_ms":250,"max_output_tokens":9}`))
	if err != nil {
		t.Fatal(err)
	}
	if !fake.waitCalled || fake.cellID != "cm_2" || fake.yield != 250*time.Millisecond || fake.output != 9 {
		t.Fatalf("wait controller args: %+v", fake)
	}
	envelope := decodeEnvelope(t, output)
	if string(envelope["state_committed"]) != "true" {
		t.Fatalf("state_committed: %s", output.Content)
	}

	cancelFake := &controlFake{execResult: core.CodeModeCallResult{CellID: "cm_2", Status: "cancelled", IsError: true}}
	cancelOutput, err := NewCancel().Execute(controlContext(cancelFake), json.RawMessage(`{"cell_id":"cm_2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cancelFake.cancelCall != "cm_2" || cancelOutput.IsError {
		t.Fatalf("cancel result: %+v", cancelOutput)
	}
}

func TestControlYieldAndOutputValidation(t *testing.T) {
	for _, tc := range []struct {
		args string
		ok   bool
	}{
		{`{"code":"","yield_time_ms":1,"max_output_tokens":1}`, true},
		{`{"code":"","yield_time_ms":60000,"max_output_tokens":100000}`, true},
		{`{"code":"","yield_time_ms":60001}`, false},
		{`{"code":"","max_output_tokens":100001}`, false},
	} {
		fake := &controlFake{execResult: core.CodeModeCallResult{Status: "yielded"}}
		_, err := NewExec().Execute(controlContext(fake), json.RawMessage(tc.args))
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err=%v", tc.args, err)
		}
	}
	if _, err := NewExec().Execute(context.Background(), json.RawMessage(`{"code":""}`)); err == nil {
		t.Fatal("tool ran without engine controller")
	}
}

func TestScriptStatusDoesNotExposeIntermediateArguments(t *testing.T) {
	const secret = "INTERMEDIATE_ARGUMENT_MUST_STAY_PRIVATE"
	call := core.CodeModeCallOutcome{ID: "nested", Name: "sink", Status: "failed", Outcome: "outcome_unknown",
		Arguments:       json.RawMessage(`{"content":"` + secret + `"}`),
		GuardianReviews: []core.GuardianReview{{Command: secret, Message: secret}},
	}
	fake := &controlFake{execResult: core.CodeModeCallResult{CellID: "cell", Status: "cancelled", Calls: []core.CodeModeCallOutcome{call}, UnsettledCalls: []core.CodeModeCallOutcome{call}}}
	output, err := NewExec().Execute(controlContext(fake), json.RawMessage(`{"code":""}`))
	if err != nil {
		t.Fatal(err)
	}
	visible := output.Content
	for _, part := range output.ContentParts {
		visible += part.Text
	}
	if strings.Contains(visible, secret) || strings.Contains(visible, `"arguments"`) || strings.Contains(visible, `"guardian_reviews"`) {
		t.Fatalf("private intermediate data leaked: %s", visible)
	}
	if len(output.ChildCalls) != 1 || !strings.Contains(string(output.ChildCalls[0].Arguments), secret) || len(output.ChildCalls[0].GuardianReviews) != 1 {
		t.Fatalf("private audit was lost: %+v", output.ChildCalls)
	}
	if !strings.Contains(string(fake.execResult.Calls[0].Arguments), secret) {
		t.Fatal("render mutated the original audit")
	}
}
