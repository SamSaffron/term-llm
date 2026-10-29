package debuglog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These are deliberately not just recognizable credentials: a regex cannot tell
// that ordinary prose, tool output, or a schema description is confidential.
var exportSecrets = append([]string{
	"private customer prompt", "private assistant response", "private tool output",
	"private tool arguments", "private tool info", "private schema description",
	"private error detail", "private working directory", "private environment value",
	"private unknown payload", "private unknown field name", "private numeric content",
}, exportCredentials...)

var exportCredentials = []string{
	"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github_pat_11AAAAAAAAAAAAAAAAAAAA_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	"AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwcml2YXRlIn0.signature",
	"Bearer opaque-bearer-token", "password=ordinary-secret",
}

func exportFixture(t *testing.T) *Session {
	t.Helper()
	stamp := "2026-10-28T00:00:00Z"
	payload := strings.Join(exportSecrets, "\n")
	entries := []map[string]any{
		{"timestamp": stamp, "type": "session_start", "session_id": "session-1", "args": []any{payload}, "cwd": payload, "command": payload},
		{"timestamp": stamp, "type": "turn_request", "turn": 1, "session_id": "session-1", "provider": "OpenAI", "model": "gpt-test", "request": map[string]any{
			"session_id": payload, "temperature": 0.5, "max_output_tokens": 123,
			"messages": []any{
				map[string]any{"role": "user", "content": payload},
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"text": payload, "reasoning_content": payload, "image_url": payload},
					map[string]any{"tool_call": map[string]any{"name": "shell", "arguments": map[string]any{"nested": []any{payload}}}},
					map[string]any{"tool_result": map[string]any{"name": "shell", "content": map[string]any{"output": payload}}},
				}},
			},
			"tools": []any{map[string]any{"name": "shell", "description": payload, "schema": map[string]any{"description": payload}}},
		}},
		{"timestamp": stamp, "type": "event", "event_type": "tool_call", "data": map[string]any{"name": "shell", "arguments": payload}},
		{"timestamp": stamp, "type": "event", "event_type": "tool_exec_end", "data": map[string]any{"tool_name": "shell", "success": true, "output": payload, "tool_info": map[string]any{"nested": []any{payload}}, "private unknown field name": payload}},
		{"timestamp": stamp, "type": "event", "event_type": "error", "data": map[string]any{"error": payload, "env": map[string]any{"value": payload}, "stdin": payload, "stdout_tail": payload, "stderr_tail": payload}},
		{"timestamp": stamp, "type": "event", "event_type": "usage", "data": map[string]any{"input_tokens": 17, "output_tokens": 4, "cached_input_tokens": 2}},
		{"timestamp": stamp, "type": "future_entry", "unknown": map[string]any{"content": payload}},
	}
	var raw bytes.Buffer
	for _, entry := range entries {
		if err := json.NewEncoder(&raw).Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "session-1.jsonl")
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	session, err := ParseSession(path)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestExportStructuralRedaction(t *testing.T) {
	for _, format := range []ExportFormat{FormatJSON, FormatMarkdown, FormatRaw} {
		for _, explicit := range []bool{false, true} {
			name := string(format) + "/default"
			if explicit {
				name = string(format) + "/explicit"
			}
			t.Run(name, func(t *testing.T) {
				session := exportFixture(t)
				var out bytes.Buffer
				if err := Export(&out, session, ExportOptions{Format: format, Redact: explicit}); err != nil {
					t.Fatal(err)
				}
				for _, secret := range exportSecrets {
					if strings.Contains(out.String(), secret) {
						t.Errorf("export leaked %q", secret)
					}
				}
				for _, metadata := range []string{"OpenAI", "gpt-test", "shell", "17", "[REDACTED]"} {
					if !strings.Contains(out.String(), metadata) {
						t.Errorf("export lost diagnostic metadata %q: %s", metadata, out.String())
					}
				}
				// Redaction must not mutate a session later used for local inspection.
				req := session.Entries[0].(RequestEntry)
				if !strings.Contains(req.Request.Messages[0].Content.(string), "private customer prompt") {
					t.Fatal("export mutated source session")
				}
			})
		}
	}
}

func TestExportRawRedactsMalformedLines(t *testing.T) {
	session := exportFixture(t)
	f, err := os.OpenFile(session.FilePath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("malformed private customer prompt\nnull\n[\"private tool output\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Export(&out, session, ExportOptions{Format: FormatRaw, Redact: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private") {
		t.Fatal("malformed raw line leaked")
	}
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil || entry == nil {
			t.Fatalf("invalid redacted JSONL: %q (%v)", line, err)
		}
	}
}
