package inspector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func inspectorCall(id, command string) llm.Part {
	args, _ := json.Marshal(map[string]string{"command": command})
	return llm.Part{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: id, Name: "shell", Arguments: args}}
}

func inspectorResult(id string) llm.Part {
	return llm.Part{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: id, Name: "shell", Content: "result-" + id, IsError: true}}
}

func TestInspectorBatchedCallsHaveLocalResultContext(t *testing.T) {
	for _, separate := range []bool{false, true} {
		t.Run(fmt.Sprintf("separate_rows=%t", separate), func(t *testing.T) {
			// The long preface hides the original calls in the collapsed assistant box.
			messages := []session.Message{
				{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: strings.Repeat("preface\n", 60)}, inspectorCall("first", "first-command"), inspectorCall("second", "second-command")}},
				// Results can arrive in a different order from calls.
				{Role: llm.RoleTool, Parts: []llm.Part{inspectorResult("second"), inspectorResult("first")}},
			}
			if separate {
				messages[1].Parts = []llm.Part{inspectorResult("second")}
				messages = append(messages, session.Message{Role: llm.RoleTool, Parts: []llm.Part{inspectorResult("first")}})
			}
			before, _ := json.Marshal(messages)
			r := NewContentRenderer(100, ui.DefaultStyles(), nil, nil, "", "", nil, config.ReasoningConfig{})
			content, items := r.RenderMessages(messages)
			view := ui.StripANSI(content)
			last := -1
			for _, want := range []string{"second-command", "result-second", "first-command", "result-first"} {
				pos := strings.Index(view, want)
				if pos <= last {
					t.Fatalf("missing or misplaced %q in:\n%s", want, view)
				}
				last = pos
			}
			seen := map[string]bool{}
			for _, item := range items {
				if seen[item.ID] {
					t.Fatalf("duplicate item ID %q", item.ID)
				}
				seen[item.ID] = true
				if item.ItemType == "tool_call" && strings.Contains(item.ID, "-tr-") && strings.HasSuffix(item.ID, "-call") {
					lines := strings.Split(view, "\n")
					if item.StartLine < 0 || item.EndLine >= len(lines) {
						t.Fatalf("context range out of bounds: %+v", item)
					}
					if !strings.Contains(lines[item.StartLine], "Tool Call (context):") || !strings.Contains(lines[item.EndLine], "Tool Result:") {
						t.Fatalf("bad context range: %+v", item)
					}
				}
			}
			after, _ := json.Marshal(messages)
			if !bytes.Equal(before, after) {
				t.Fatal("render mutated stored messages")
			}
			// Reusing the renderer must not retain a call from a previous transcript.
			view, _ = r.RenderMessages(messages[1:])
			if strings.Contains(view, "Tool Call:") {
				t.Fatal("stale call context leaked across renders")
			}
		})
	}
}

func TestInspectorResultCallMatching(t *testing.T) {
	for _, tt := range []struct {
		name   string
		middle []session.Message
		result llm.Part
		want   bool
	}{
		{name: "matching", result: inspectorResult("call"), want: true},
		{name: "unnamed result", result: llm.Part{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call"}}, want: true},
		{name: "orphan", result: inspectorResult("missing")},
		{name: "new user turn", middle: []session.Message{{Role: llm.RoleUser}}, result: inspectorResult("call")},
		{name: "wrong tool", result: llm.Part{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call", Name: "read_file"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			messages := []session.Message{{Role: llm.RoleAssistant, Parts: []llm.Part{inspectorCall("call", "command")}}}
			messages = append(messages, tt.middle...)
			messages = append(messages, session.Message{Role: llm.RoleTool, Parts: []llm.Part{tt.result}})
			r := NewContentRenderer(100, ui.DefaultStyles(), nil, nil, "", "", nil, config.ReasoningConfig{})
			_, items := r.RenderMessages(messages)
			found := false
			for _, item := range items {
				if item.ItemType == "tool_call" && strings.Contains(item.ID, "-tr-") && strings.HasSuffix(item.ID, "-call") {
					found = true
				}
			}
			if found != tt.want {
				t.Fatalf("call context = %v, want %v", found, tt.want)
			}
		})
	}
}

func TestInspectorResultCallContextExpandAll(t *testing.T) {
	command := strings.Repeat("argument ", 1000) + "command-end-marker"
	messages := []session.Message{
		{Role: llm.RoleAssistant, Parts: []llm.Part{inspectorCall("call", command)}},
		{Role: llm.RoleTool, Parts: []llm.Part{inspectorResult("call")}},
	}
	m := New(messages, 100, 24, ui.DefaultStyles())
	found := false
	for _, item := range m.items {
		if (item.ItemType == "tool_call" && strings.Contains(item.ID, "-tr-") && strings.HasSuffix(item.ID, "-call")) && item.IsTruncated {
			found = true
		}
	}
	if !found {
		t.Fatal("expected truncated result call context")
	}
	m.expandAllItems()
	for _, item := range m.items {
		if item.IsTruncated {
			t.Fatalf("item remains truncated: %+v", item)
		}
	}
	if !strings.Contains(ui.StripANSI(strings.Join(m.contentLines, "\n")), "command-end-marker") {
		t.Fatal("expanded call missing command tail")
	}
}
func TestInspectorEmptyToolRowsDoNotShiftCallContext(t *testing.T) {
	messages := []session.Message{
		{Role: llm.RoleAssistant, Parts: []llm.Part{inspectorCall("call", "command")}},
		{Role: llm.RoleTool},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult}}},
		{Role: llm.RoleTool, Parts: []llm.Part{inspectorResult("call")}},
	}
	r := NewContentRenderer(100, ui.DefaultStyles(), nil, nil, "", "", nil, config.ReasoningConfig{})
	content, items := r.RenderMessages(messages)
	lines := strings.Split(ui.StripANSI(content), "\n")
	for _, item := range items {
		if item.ItemType == "tool_call" && strings.Contains(item.ID, "-tr-") {
			if item.StartLine < 0 || item.EndLine >= len(lines) {
				t.Fatalf("context range out of bounds: %+v", item)
			}
			if !strings.Contains(lines[item.StartLine], "Tool Call (context):") || !strings.Contains(lines[item.EndLine], "Tool Result:") {
				t.Fatalf("shifted context range: %+v\n%s", item, content)
			}
			return
		}
	}
	t.Fatal("missing context item")
}
