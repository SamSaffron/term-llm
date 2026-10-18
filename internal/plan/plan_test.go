package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestParseAndValidateSnapshot(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "valid", raw: `{"explanation":" moving on ","plan":[{"step":" Inspect code ","status":"completed"},{"step":"Add tests","status":"in_progress"}]}`},
		{name: "empty clears", raw: `{"plan":[]}`},
		{name: "missing plan", raw: `{}`, wantErr: "plan is required"},
		{name: "too many", raw: `{"plan":[` + strings.Repeat(`{"step":"x","status":"pending"},`, 20) + `{"step":"last","status":"pending"}]}`, wantErr: "at most 20"},
		{name: "blank step", raw: `{"plan":[{"step":"  ","status":"pending"}]}`, wantErr: "step 1 is required"},
		{name: "bad status", raw: `{"plan":[{"step":"x","status":"done"}]}`, wantErr: "invalid status"},
		{name: "two current", raw: `{"plan":[{"step":"x","status":"in_progress"},{"step":"y","status":"in_progress"}]}`, wantErr: "at most one"},
		{name: "normalized duplicate", raw: `{"plan":[{"step":"Inspect   code","status":"completed"},{"step":" inspect code ","status":"pending"}]}`, wantErr: "duplicates"},
		{name: "unknown field", raw: `{"plan":[],"extra":true}`, wantErr: "unknown field"},
		{name: "null explanation", raw: `{"explanation":null,"plan":[]}`, wantErr: "must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(json.RawMessage(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Parse() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got.Plan == nil {
				t.Fatal("Parse() returned nil plan")
			}
			if tt.name == "valid" && (got.Explanation != "moving on" || got.Plan[0].Step != "Inspect code") {
				t.Fatalf("Parse() did not trim fields: %#v", got)
			}
		})
	}
}

func TestParseUsesUnicodeCodePointLimits(t *testing.T) {
	valid := strings.Repeat("🙂", MaxStepRunes)
	if _, err := Parse(json.RawMessage(`{"plan":[{"step":"` + valid + `","status":"pending"}]}`)); err != nil {
		t.Fatalf("valid Unicode length rejected: %v", err)
	}
	tooLong := valid + "🙂"
	if _, err := Parse(json.RawMessage(`{"plan":[{"step":"` + tooLong + `","status":"pending"}]}`)); err == nil {
		t.Fatal("oversized Unicode step accepted")
	}
}

func TestSnapshotSummaryAndContext(t *testing.T) {
	snapshot := Snapshot{
		Explanation: "Moving to verification.",
		Plan: []Step{
			{Step: "Inspect patterns", Status: StatusCompleted},
			{Step: "Add tests", Status: StatusInProgress},
			{Step: "Run suite", Status: StatusPending},
		},
	}
	if !snapshot.IsActive() {
		t.Fatal("snapshot should be active")
	}
	summary := snapshot.Summary()
	if summary.Completed != 1 || summary.Total != 3 || summary.CurrentStep != "Add tests" {
		t.Fatalf("Summary() = %#v", summary)
	}
	context := snapshot.ContextMessage()
	for _, want := range []string{"<current_execution_plan>", "Explanation: Moving to verification.", "- [in_progress] Add tests", "restored execution state"} {
		if !strings.Contains(context, want) {
			t.Fatalf("ContextMessage() missing %q:\n%s", want, context)
		}
	}
	completed := Snapshot{Plan: []Step{{Step: "Done", Status: StatusCompleted}}}
	if completed.IsActive() || completed.ContextMessage() != "" {
		t.Fatal("completed snapshot should be inactive and have no restoration context")
	}
}

func TestLatestSuccessfulSnapshot(t *testing.T) {
	call := func(id, args string) core.Message {
		return core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: id, Name: ToolName, Arguments: json.RawMessage(args)}}}}
	}
	result := func(id string, failed bool) core.Message {
		return core.Message{Role: core.RoleTool, Parts: []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: id, Name: ToolName, Content: "Plan updated", IsError: failed}}}}
	}
	messages := []core.Message{
		call("one", `{"plan":[{"step":"old","status":"pending"}]}`), result("one", false),
		call("two", `{"plan":[{"step":"failed","status":"pending"}]}`), result("two", true),
	}
	got, ok := LatestSuccessfulSnapshot(messages)
	if !ok || len(got.Plan) != 1 || got.Plan[0].Step != "old" {
		t.Fatalf("LatestSuccessfulSnapshot() = %#v, %v", got, ok)
	}
}
