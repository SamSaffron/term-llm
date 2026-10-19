package subagentview

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestBuildUsesParentResultAndFoldsChildActivity(t *testing.T) {
	call := &core.ToolCall{ID: "spawn-1", Name: tools.SpawnAgentToolName, Arguments: json.RawMessage(`{"agent_name":"reviewer","prompt":"review this"}`)}
	result := &core.ToolResult{ID: "spawn-1", Name: tools.SpawnAgentToolName, Diffs: []core.DiffData{{File: "a.go", Old: "a", New: "b"}}}
	child := &session.Session{
		ID: "child-1", ParentID: "parent", Provider: "mock", Model: "review-model",
		Status: session.StatusComplete, CreatedAt: time.Unix(10, 0), UpdatedAt: time.Unix(20, 0),
		ToolCalls: 1, InputTokens: 10, CachedInputTokens: 2, OutputTokens: 3,
	}
	messages := []session.Message{
		*session.NewMessage(child.ID, core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "read-1", Name: "read_file", ToolInfo: "a.go"}}}}, 0),
		*session.NewMessage(child.ID, core.ToolResultMessage("read-1", "read_file", "ok", nil), 1),
	}
	run := Build(call, result, tools.SpawnAgentResult{AgentName: "reviewer", Output: "authoritative result", Duration: 123, SessionID: child.ID}, child, messages)
	if run.ParentCallID != "spawn-1" || run.ChildSessionID != child.ID || run.Prompt != "review this" {
		t.Fatalf("identity = %#v", run)
	}
	if run.Output != "authoritative result" || run.DurationMs != 123 || run.Provider != "mock" || run.Model != "review-model" {
		t.Fatalf("result metadata = %#v", run)
	}
	if run.ToolCalls != 1 || len(run.Activities) != 1 || !run.Activities[0].Done || !run.Activities[0].Success {
		t.Fatalf("activity = %#v", run.Activities)
	}
	if run.InputTokens != 12 || run.OutputTokens != 3 || len(run.Diffs) != 1 || run.Fingerprint == 0 {
		t.Fatalf("metrics/artifacts = %#v", run)
	}
}

func TestBuildFallsBackWithoutChildAndBoundsOutputPreview(t *testing.T) {
	call := &core.ToolCall{ID: "spawn-2", Name: tools.SpawnAgentToolName, Arguments: json.RawMessage(`{"agent_name":"codebase","prompt":"find it"}`)}
	run := Build(call, nil, tools.SpawnAgentResult{Output: "one\ntwo\nthree\nfour\nfive", Error: "timed out", Type: "timeout", SessionID: "missing"}, nil, nil)
	if run.ChildAvailable || !run.Partial || run.Error != "timed out" || len(run.TextPreview) != PreviewTextLineLimit {
		t.Fatalf("fallback = %#v", run)
	}
	if run.TextPreview[0] != "two" || run.TextPreview[3] != "five" {
		t.Fatalf("preview = %#v", run.TextPreview)
	}
}

func TestBuildBoundsActivitiesAndArtifacts(t *testing.T) {
	child := &session.Session{ID: "child", Status: session.StatusComplete}
	parts := make([]core.Part, 0, (MaxExpandedActivities+20)*2)
	for i := 0; i < MaxExpandedActivities+20; i++ {
		id := fmt.Sprintf("call-%d", i)
		parts = append(parts,
			core.Part{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: id, Name: "edit_file"}},
			core.Part{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: id, Name: "edit_file", Diffs: []core.DiffData{{File: fmt.Sprintf("%d.go", i)}}, Images: []string{fmt.Sprintf("%d.png", i)}}},
		)
	}
	messages := []session.Message{*session.NewMessage(child.ID, core.Message{Role: core.RoleAssistant, Parts: parts}, 0)}
	run := Build(&core.ToolCall{ID: "outer", Name: tools.SpawnAgentToolName}, nil, tools.SpawnAgentResult{Output: "done"}, child, messages)
	if len(run.Activities) != MaxExpandedActivities || len(run.Diffs) != MaxArtifacts || len(run.Images) != MaxArtifacts {
		t.Fatalf("bounds activities=%d diffs=%d images=%d", len(run.Activities), len(run.Diffs), len(run.Images))
	}
}

func TestBuildKeepsNestedSpawnAsDurableActivity(t *testing.T) {
	child := &session.Session{ID: "child", Status: session.StatusComplete}
	messages := []session.Message{
		*session.NewMessage(child.ID, core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "nested", Name: tools.SpawnAgentToolName, ToolInfo: "codebase"}}}}, 0),
		*session.NewMessage(child.ID, core.ToolResultMessage("nested", tools.SpawnAgentToolName, `{"agent_name":"codebase","output":"found"}`, nil), 1),
	}
	run := Build(&core.ToolCall{ID: "outer", Name: tools.SpawnAgentToolName}, nil, tools.SpawnAgentResult{Output: "outer result"}, child, messages)
	if len(run.Activities) != 1 || run.Activities[0].Name != tools.SpawnAgentToolName || !run.Activities[0].Done || !run.Activities[0].Success {
		t.Fatalf("nested activity = %#v", run.Activities)
	}
}

func TestBuildKeepsRepeatedAgentCallsIsolatedByCallID(t *testing.T) {
	first := Build(&core.ToolCall{ID: "one", Name: tools.SpawnAgentToolName}, nil, tools.SpawnAgentResult{AgentName: "reviewer", Output: "first"}, nil, nil)
	second := Build(&core.ToolCall{ID: "two", Name: tools.SpawnAgentToolName}, nil, tools.SpawnAgentResult{AgentName: "reviewer", Output: "second"}, nil, nil)
	if first.ParentCallID == second.ParentCallID || first.Fingerprint == second.Fingerprint {
		t.Fatalf("runs collided: first=%#v second=%#v", first, second)
	}
}

func TestCompletedSubagentRetainsAudioReferenceAndFingerprint(t *testing.T) {
	audio := core.MediaArtifact{Reference: "0123456789abcdef0123456789abcdef", StoredPath: "/store/speech.mp3", MediaType: "audio/mpeg", Name: "speech.mp3", Caption: "Listen"}
	child := &session.Session{ID: "child", Status: session.StatusComplete}
	result := &core.ToolResult{ID: "outer", Name: tools.SpawnAgentToolName, Media: []core.MediaArtifact{audio}}
	childMessage := session.NewMessage(child.ID, core.ToolResultMessageFromOutput("audio-call", "show_media", core.ToolOutput{Media: []core.MediaArtifact{audio}}, nil), 0)
	run := Build(&core.ToolCall{ID: "outer", Name: tools.SpawnAgentToolName}, result, tools.SpawnAgentResult{Output: "Listen", SessionID: child.ID}, child, []session.Message{*childMessage})
	if len(run.Media) != 1 || run.Media[0] != audio {
		t.Fatalf("media=%+v", run.Media)
	}
	result.Media[0].Caption = "New caption"
	updated := Build(&core.ToolCall{ID: "outer", Name: tools.SpawnAgentToolName}, result, tools.SpawnAgentResult{Output: "Listen", SessionID: child.ID}, child, nil)
	if updated.Fingerprint == run.Fingerprint {
		t.Fatal("caption did not invalidate completed-run projection")
	}
}
