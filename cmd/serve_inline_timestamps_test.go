package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionMessageEntriesPreserveInlinePartTimestamps(t *testing.T) {
	const start int64 = 1800000000000
	parts := []llm.Part{
		{Type: llm.PartText, Text: "before", CreatedAt: start},
		{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call", Name: "shell"}, CreatedAt: start + 1000},
		{Type: llm.PartText, Text: "final", CreatedAt: start + 600000},
	}
	// Parts are stored as JSON; no database schema change is necessary.
	encoded, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	var restored []llm.Part
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	entries := (&serveServer{}).sessionMessageEntries([]session.Message{{
		ID: 1, Role: llm.RoleAssistant, CreatedAt: time.UnixMilli(start), Parts: restored,
	}})
	if len(entries) != 1 || len(entries[0].Parts) != len(parts) {
		t.Fatalf("entries = %#v", entries)
	}
	for i, part := range entries[0].Parts {
		if part.CreatedAt != parts[i].CreatedAt {
			t.Fatalf("part %d time = %d, want %d", i, part.CreatedAt, parts[i].CreatedAt)
		}
	}
}
