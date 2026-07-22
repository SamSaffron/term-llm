package cmd

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionMessageEntriesExposeErrorOnlyToolResults(t *testing.T) {
	srv := &serveServer{}
	entries := srv.sessionMessageEntries([]session.Message{{
		ID:        1,
		Sequence:  2,
		Role:      llm.RoleTool,
		CreatedAt: time.Now(),
		Parts: []llm.Part{{
			Type: llm.PartToolResult,
			ToolResult: &llm.ToolResult{
				ID:      "call-plan",
				Name:    "update_plan",
				IsError: true,
				Content: "database unavailable",
			},
		}},
	}})
	if len(entries) != 1 || len(entries[0].Parts) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	part := entries[0].Parts[0]
	if part.Type != "tool_result" || part.ToolCallID != "call-plan" || part.ToolName != "update_plan" || !part.ToolError {
		t.Fatalf("tool result = %#v", part)
	}
}

func TestSessionMessageEntriesExposeSuccessfulPlanResultOnly(t *testing.T) {
	now := time.Now()
	srv := &serveServer{}
	entries := srv.sessionMessageEntries([]session.Message{
		{
			ID:        1,
			Sequence:  1,
			Role:      llm.RoleAssistant,
			CreatedAt: now,
			Parts: []llm.Part{
				{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-plan", Name: "update_plan"}},
				{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-grep", Name: "grep"}},
			},
		},
		{
			ID:        2,
			Sequence:  2,
			Role:      llm.RoleTool,
			CreatedAt: now,
			Parts: []llm.Part{
				{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-plan", Name: "update_plan"}},
				{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-grep", Name: "grep"}},
			},
		},
	})
	if len(entries) != 2 || len(entries[0].Parts) != 2 || len(entries[1].Parts) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	part := entries[1].Parts[0]
	if part.Type != "tool_result" || part.ToolCallID != "call-plan" || part.ToolName != "update_plan" || part.ToolError {
		t.Fatalf("successful plan result = %#v", part)
	}

	isolated := srv.sessionMessageEntries([]session.Message{{
		ID: 3, Sequence: 3, Role: llm.RoleTool, CreatedAt: now,
		Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-plan-page", Name: "update_plan"}}},
	}})
	if len(isolated) != 1 || len(isolated[0].Parts) != 1 || isolated[0].Parts[0].ToolCallID != "call-plan-page" {
		t.Fatalf("page-boundary plan result = %#v", isolated)
	}
}

func TestSessionMessageEntriesDoNotCorrelateEmptyToolResultIDs(t *testing.T) {
	srv := &serveServer{}
	entries := srv.sessionMessageEntries([]session.Message{
		{
			ID:        1,
			Sequence:  1,
			Role:      llm.RoleAssistant,
			CreatedAt: time.Now(),
			Parts: []llm.Part{{
				Type:     llm.PartToolCall,
				ToolCall: &llm.ToolCall{Name: "update_plan"},
			}},
		},
		{
			ID:        2,
			Sequence:  2,
			Role:      llm.RoleTool,
			CreatedAt: time.Now(),
			Parts: []llm.Part{{
				Type:       llm.PartToolResult,
				ToolResult: &llm.ToolResult{Name: "update_plan", IsError: true},
			}},
		},
	})
	if len(entries) != 2 || len(entries[0].Parts) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].Parts[0].ToolError {
		t.Fatalf("empty-ID error result was correlated to empty-ID call: %#v", entries[0].Parts[0])
	}
}
