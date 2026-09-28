package live

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseEventNormalisesProtocolFrames(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		wantKind EventKind
		wantRole string
		wantText string
		wantID   string
	}{
		{
			name:     "session started",
			payload:  `{"type":"session.started","session":{"model":"gpt-live-1-codex"}}`,
			wantKind: EventSessionStarted,
		},
		{
			name:     "user transcript delta",
			payload:  `{"type":"input_transcript.added","item":{"text":"list the "}}`,
			wantKind: EventUserTranscript,
			wantRole: RoleUser,
			wantText: "list the ",
		},
		{
			name:     "assistant transcript delta",
			payload:  `{"type":"output_transcript.added","item":{"text":"sure"}}`,
			wantKind: EventAssistantTranscript,
			wantRole: RoleAssistant,
			wantText: "sure",
		},
		{
			name:     "turn done",
			payload:  `{"type":"turn.done","turn":{"role":"user","transcript":"list the files"}}`,
			wantKind: EventTurnDone,
			wantRole: RoleUser,
			wantText: "list the files",
		},
		{
			name:     "delegation created",
			payload:  `{"type":"delegation.created","item":{"id":"item_1","type":"delegation","target":"client","content":[{"type":"input_text","text":"list "},{"type":"output_text","text":"skip"},{"type":"input_text","text":"files"}]}}`,
			wantKind: EventDelegationCreated,
			wantText: "list files",
			wantID:   "item_1",
		},
		{
			name:     "delegation for another target is ignored",
			payload:  `{"type":"delegation.created","item":{"id":"item_1","type":"delegation","target":"server"}}`,
			wantKind: EventUnknown,
		},
		{
			name:     "error object",
			payload:  `{"type":"error","error":{"message":"model unavailable"}}`,
			wantKind: EventError,
			wantText: "model unavailable",
		},
		{
			name:     "error string",
			payload:  `{"type":"error","error":"boom"}`,
			wantKind: EventError,
			wantText: "boom",
		},
		{
			name:     "audio deltas are ignored",
			payload:  `{"type":"output_audio.delta","audio":"AAAA"}`,
			wantKind: EventUnknown,
		},
		{
			name:     "unknown types are ignored",
			payload:  `{"type":"session.usage.updated","usage":{}}`,
			wantKind: EventUnknown,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event, err := ParseEvent([]byte(tc.payload))
			if err != nil {
				t.Fatalf("ParseEvent: %v", err)
			}
			if event.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", event.Kind, tc.wantKind)
			}
			if event.Role != tc.wantRole {
				t.Fatalf("role = %q, want %q", event.Role, tc.wantRole)
			}
			if event.Text != tc.wantText {
				t.Fatalf("text = %q, want %q", event.Text, tc.wantText)
			}
			if event.DelegationID != tc.wantID {
				t.Fatalf("delegation id = %q, want %q", event.DelegationID, tc.wantID)
			}
		})
	}
}

func TestParseEventReportsMalformedJSON(t *testing.T) {
	if _, err := ParseEvent([]byte("{")); err == nil {
		t.Fatal("expected an error for malformed json")
	}
}

func TestOutboundMessagesMatchProtocol(t *testing.T) {
	encoded, err := json.Marshal(DelegationContextAppend("item_1", ChannelQuiet, "hi"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"delegation.context.append","delegation_item_id":"item_1","channel":"commentary","content":[{"type":"input_text","text":"hi"}]}`
	if string(encoded) != want {
		t.Fatalf("delegation append = %s, want %s", encoded, want)
	}

	encoded, err = json.Marshal(SessionContextAppend("bogus", "[USER] hi"))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"type":"session.context.append","channel":"speakable","content":[{"type":"input_text","text":"[USER] hi"}]}`
	if string(encoded) != want {
		t.Fatalf("session append = %s, want %s", encoded, want)
	}

	encoded, err = json.Marshal(SessionClose())
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"type":"session.close"}` {
		t.Fatalf("session close = %s", encoded)
	}
}

func TestChunkTextSplitsOnRuneBoundaries(t *testing.T) {
	if chunks := ChunkText("", 10); chunks != nil {
		t.Fatalf("empty text produced %v", chunks)
	}
	if chunks := ChunkText("short", 10); len(chunks) != 1 || chunks[0] != "short" {
		t.Fatalf("short text produced %v", chunks)
	}

	text := strings.Repeat("é", 400) // 800 bytes of two-byte runes
	chunks := ChunkText(text, MaxAppendBytes)
	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) > MaxAppendBytes {
			t.Fatalf("chunk of %d bytes exceeds the limit", len(chunk))
		}
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %q is not valid UTF-8", chunk)
		}
	}
	if strings.Join(chunks, "") != text {
		t.Fatal("chunks did not reassemble to the original text")
	}

	// A single rune wider than the limit is emitted whole rather than split.
	chunks = ChunkText("😀a", 2)
	if len(chunks) != 2 || chunks[0] != "😀" || chunks[1] != "a" {
		t.Fatalf("oversized rune chunking = %q", chunks)
	}
}
