package live

import (
	"encoding/json"
	"testing"
)

func TestGeminiInterimInputTranscription(t *testing.T) {
	s := newGeminiSession(nil)
	handle := func(wire string) {
		t.Helper()
		var message geminiServerMessage
		if err := json.Unmarshal([]byte(wire), &message); err != nil {
			t.Fatal(err)
		}
		s.handleMessage(message)
	}
	expect := func(kind EventKind, text string) {
		t.Helper()
		event := receiveEvent(t, s.Events())
		if event.Kind != kind || event.Text != text {
			t.Fatalf("event=%+v, want %s %q", event, kind, text)
		}
	}
	for _, text := range []string{"check the file", "check the files", ""} {
		data, _ := json.Marshal(map[string]any{"serverContent": map[string]any{"interimInputTranscription": map[string]string{"text": text}}})
		handle(string(data))
		expect(EventUserTranscriptInterim, text)
		if s.userTranscript.Len() != 0 {
			t.Fatal("interim preview entered authoritative transcript")
		}
	}
	// A final transcription takes precedence over an interim result in the same frame.
	handle(`{"serverContent":{"inputTranscription":{"text":"check "},"interimInputTranscription":{"text":"stale guess"}}}`)
	expect(EventUserTranscript, "check ")
	handle(`{"serverContent":{"inputTranscription":{"text":"the files"}}}`)
	expect(EventUserTranscript, "the files")
	handle(`{"serverContent":{"turnComplete":true}}`)
	expect(EventTurnDone, "check the files")
	if len(s.events) != 0 {
		t.Fatal("unexpected duplicate transcript events")
	}
	// A turn with no authoritative result clears the preview, never promotes it.
	handle(`{"serverContent":{"interimInputTranscription":{"text":"unconfirmed"}}}`)
	expect(EventUserTranscriptInterim, "unconfirmed")
	handle(`{"serverContent":{"turnComplete":true}}`)
	expect(EventUserTranscriptInterim, "")
	if len(s.events) != 0 || s.userTranscript.Len() != 0 {
		t.Fatal("preview became a final turn")
	}
}

func TestControllerInterimTranscriptIsPreviewOnly(t *testing.T) {
	var updates []Update
	c := NewController(ControllerOptions{Observer: func(u Update) { updates = append(updates, u) }})
	for _, text := range []string{"open the wrong file", "open the right file"} {
		c.handleEvent(Event{Kind: EventUserTranscriptInterim, Text: text})
		got := updates[len(updates)-1]
		if got.Kind != UpdateTranscript || !got.Interim || got.Final || got.Text != text || got.Role != RoleUser {
			t.Fatalf("preview=%+v", got)
		}
	}
	if c.partial(RoleUser) != "" || len(c.transcript) != 0 || c.lastUserTurn != "" {
		t.Fatal("preview leaked into delegation context")
	}
	c.handleEvent(Event{Kind: EventUserTranscript, Text: "open the "})
	c.handleEvent(Event{Kind: EventUserTranscript, Text: "right file"})
	got := updates[len(updates)-1]
	if got.Interim || got.Text != "open the right file" {
		t.Fatalf("authoritative partial=%+v", got)
	}
	c.handleEvent(Event{Kind: EventTurnDone, Role: RoleUser})
	got = updates[len(updates)-1]
	if !got.Final || got.Interim || got.Text != "open the right file" {
		t.Fatalf("final=%+v", got)
	}
	if len(c.transcript) != 1 || c.transcript[0] != "user: open the right file" {
		t.Fatalf("transcript=%v", c.transcript)
	}
}
