package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionMessageEntriesPreserveInlinePartTimestamps(t *testing.T) {
	const start int64 = 1800000000000
	parts := []core.Part{
		{Type: core.PartText, Text: "before", CreatedAt: start},
		{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "call", Name: "shell"}, CreatedAt: start + 1000},
		{Type: core.PartText, Text: "final", CreatedAt: start + 600000},
	}
	// Parts are stored as JSON; no database schema change is necessary.
	encoded, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	var restored []core.Part
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	entries := (&serveServer{}).sessionMessageEntries([]session.Message{{
		ID: 1, Role: core.RoleAssistant, CreatedAt: time.UnixMilli(start), Parts: restored,
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
