package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/ask"
	"github.com/samsaffron/term-llm/internal/ui"
)

// TestAskJSONFlagIsRegistered verifies the --json flag is wired onto askCmd.
func TestAskJSONFlagIsRegistered(t *testing.T) {
	f := askCmd.Flags().Lookup("json")
	if f == nil {
		t.Fatal("expected --json flag to be registered on askCmd")
	}
	if f.Value.Type() != "bool" {
		t.Fatalf("expected --json to be bool, got %s", f.Value.Type())
	}
	if f.DefValue != "false" {
		t.Fatalf("expected --json default = false, got %s", f.DefValue)
	}
}

// TestAskJSONEndToEndSmoke drives a realistic ask session through the JSON
// emitter and verifies the full JSONL stream is well-formed with the
// expected ordering and event types.
func TestAskJSONEndToEndSmoke(t *testing.T) {
	events := []ui.StreamEvent{
		ui.TextEvent("Let me check that file.\n"),
		ui.ToolStartEvent("call-1", "read_file", "(announcement.md)", json.RawMessage(`{"path":"announcement.md"}`)),
		ui.ToolEndEvent("call-1", "read_file", "(announcement.md)", true),
		ui.TextEvent("Here is what I found:\n"),
		ui.TextEvent("Ruby 4.0 was released.\n"),
		ui.UsageEvent(120, 45, 30, 0),
		ui.DoneEvent(45),
	}

	ch := make(chan ui.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	var buf bytes.Buffer
	emitter := ask.NewJSONEmitter(&buf)
	stats := ui.NewSessionStats()

	info := ask.SessionInfo{
		SessionID: "smoke-1",
		Provider:  "mock",
		Model:     "mock-model",
		Tools:     "read_file",
		Search:    false,
	}

	if err := ask.StreamJSON(context.Background(), ch, emitter, stats, info); err != nil {
		t.Fatalf("streamJSON returned error: %v", err)
	}

	var decoded []map[string]any
	for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %q", i, err, line)
		}
		decoded = append(decoded, obj)
	}

	var gotTypes []string
	for _, ev := range decoded {
		gotTypes = append(gotTypes, ev["type"].(string))
	}

	wantTypes := []string{
		"session.started",
		"text.delta",
		"tool.started",
		"tool.completed",
		"text.delta",
		"text.delta",
		"usage",
		"stats",
		"done",
	}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event count = %d (%v), want %d (%v)", len(gotTypes), gotTypes, len(wantTypes), wantTypes)
	}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Errorf("event %d type = %q, want %q", i, gotTypes[i], want)
		}
	}

	for i, ev := range decoded {
		if got, ok := ev["seq"].(float64); !ok || int(got) != i {
			t.Errorf("event %d seq = %v, want %d", i, ev["seq"], i)
		}
		if _, ok := ev["ts"].(string); !ok {
			t.Errorf("event %d missing ts", i)
		}
	}

	last := decoded[len(decoded)-1]
	secondLast := decoded[len(decoded)-2]
	if secondLast["type"] != "stats" || last["type"] != "done" {
		t.Fatalf("last two events must be stats,done; got %v,%v", secondLast["type"], last["type"])
	}
}
