package inspect

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestInspectCompactionHelpers(t *testing.T) {
	messages := []session.Message{{Role: core.RoleSystem, Sequence: 0}, {Role: core.RoleUser, Sequence: 2}, {Role: core.RoleAssistant, Sequence: 3, CompactionTail: true}, {Role: core.RoleUser, Sequence: 4}}
	for _, tt := range []struct {
		name             string
		enabled          bool
		seq, index, want int
	}{
		{"disabled", false, 2, 1, -1}, {"sequence", true, 3, -1, 2}, {"skip system", true, 0, -1, 1}, {"fallback", true, 9, 1, 1}, {"index only", true, -1, 2, 2}, {"absent", true, 9, -1, -1}, {"out of range", true, -1, 4, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := CompactionBoundaryIndex(messages, tt.enabled, tt.seq, tt.index); got != tt.want {
				t.Fatalf("got %d want %d", got, tt.want)
			}
		})
	}
	if CompactionBoundaryIndex(messages[:1], true, 0, -1) != -1 || CompactionBoundaryIndex(nil, true, 0, 0) != -1 {
		t.Fatal("system-only or empty boundary")
	}
	if CompactionTailEnd(messages, 1) != 3 || CompactionTailEnd(messages, 3) != 4 {
		t.Fatal("non-contiguous tail end")
	}
	for _, tt := range []struct {
		name string
		msg  session.Message
		want bool
	}{
		{"text content", session.Message{TextContent: "[Context Compaction]\nsummary"}, true},
		{"part", session.Message{Parts: []core.Part{{Type: core.PartText, Text: "[Context Compaction]\nsummary"}}}, true},
		{"replay not text", session.Message{Parts: []core.Part{{Type: core.PartProviderReplay, Text: "[Context Compaction]"}}}, false},
		{"ordinary", session.Message{TextContent: "not compacted"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if IsCompactionSummary(tt.msg) != tt.want {
				t.Fatal("summary classification")
			}
		})
	}
}

func TestInspectExtractChildCandidate(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{`{"session_id":"child"}`, "child"},
		{`[{"session_id":"one"},{"session_id":"two"}]`, "one"},
		{`{"agent_id":"agent"}`, ""}, {`[]`, ""}, {`broken`, ""}, {`{}`, ""},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if got := ExtractSessionIDFromSpawnAgentResult(tt.input); got != tt.want {
				t.Fatalf("got %q want %q", got, tt.want)
			}
		})
	}
	messages := []session.Message{inspectMessage(1, core.RoleTool, core.Part{Type: core.PartToolResult, ToolResult: &core.ToolResult{Name: "spawn_agent", Content: `{"session_id":"unvalidated"}`}})}
	if got := Project(messages, Options{}).Blocks[0].ChildSessionIDs; len(got) != 0 {
		t.Fatal("unvalidated child emitted", got)
	}
}

func TestInspectFormatHelpers(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"", "{}"}, {"malformed", "malformed"}, {`{"a":1}`, "{\n  \"a\": 1\n}"}, {`null`, "null"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if got := FormatJSON(json.RawMessage(tt.input)); got != tt.want {
				t.Fatalf("got %q", got)
			}
		})
	}
	if FormatSkillActivationProvenance(nil) != "" {
		t.Fatal("nil provenance")
	}
	provenance := &core.SkillActivationProvenance{Name: "skill", Source: "disk", SourcePath: "path", Origin: "direct", Execution: "local", RawArguments: "args", Agent: "dev", Model: "mock", RunID: "run", ChildSessionID: "child", Status: "done", StartedAt: "start", CompletedAt: "end", AllowedToolsPresent: true, AllowedTools: []string{"shell", "read_file"}}
	got := FormatSkillActivationProvenance(provenance)
	for _, want := range []string{"Skill activation: skill", "Source: disk", "Source path: path", "Origin: direct", "Execution: local", "Arguments: args", "Agent: dev", "Model: mock", "Run ID: run", "Child session: child", "Status: done", "Started: start", "Completed: end", "Allowed tools: shell, read_file"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
}
