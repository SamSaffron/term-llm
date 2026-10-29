package debuglog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportUnsafeOptOut(t *testing.T) {
	for _, format := range []ExportFormat{FormatJSON, FormatMarkdown, FormatRaw} {
		t.Run(string(format), func(t *testing.T) {
			session := exportFixture(t)
			if format == FormatRaw {
				original, err := os.ReadFile(session.FilePath)
				if err != nil {
					t.Fatal(err)
				}
				// Unsafe raw diagnostics must also preserve malformed records and
				// a missing final newline, rather than decode/re-encode the log.
				original = append(original, []byte("malformed private diagnostic line\nno final newline")...)
				if err := os.WriteFile(session.FilePath, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			if err := Export(&out, session, ExportOptions{Format: format, IncludeSensitive: true}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "private customer prompt") {
				t.Fatal("explicit local diagnostic export lost content")
			}
			if format == FormatRaw {
				original, err := os.ReadFile(session.FilePath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(out.Bytes(), original) {
					t.Fatal("unsafe raw export must preserve original bytes")
				}
			}
			out.Reset()
			if err := Export(&out, session, ExportOptions{Format: format, Redact: true, IncludeSensitive: true}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "private customer prompt") {
				t.Fatal("explicit redaction did not win over unsafe opt-out")
			}
		})
	}
}

func TestExportRawByIDDefaultsToRedacted(t *testing.T) {
	session := exportFixture(t)
	var out bytes.Buffer
	if err := ExportRawByID(filepath.Dir(session.FilePath), session.ID, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private customer prompt") || !strings.Contains(out.String(), redacted) {
		t.Fatal("raw convenience API bypassed default redaction")
	}
}

func TestExportRedactsCredentialsInMetadata(t *testing.T) {
	credentials := append([]string{}, exportCredentials...)
	credentials = append(credentials, "ASIAIOSFODNN7EXAMPLE", "gho_abcdefghijklmnopqrstuvwxyz0123456789", "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789")
	for _, secret := range credentials {
		for _, format := range []ExportFormat{FormatJSON, FormatMarkdown, FormatRaw} {
			t.Run(string(format)+"/"+secret[:8], func(t *testing.T) {
				session := exportFixture(t)
				// Secrets may accidentally be copied to labels; guard both wrapper and
				// entry metadata, not just the conventional credential fields.
				session.ID, session.Provider, session.Model = secret, secret, secret
				session.Entries = []any{RequestEntry{Provider: secret, Model: secret, Request: RequestData{Messages: []Message{{Role: "user", Content: "hidden"}}}}, EventEntry{EventType: "tool_call", Data: map[string]any{"name": secret, "arguments": "hidden"}}}
				entry := map[string]any{"type": "request", "provider": secret, "model": secret, "session_id": secret, "request": map[string]any{"tools": []any{map[string]any{"name": secret}}}}
				data, err := json.Marshal(entry)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(session.FilePath, append(data, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				if err := Export(&out, session, ExportOptions{Format: format}); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(out.String(), secret) {
					t.Fatalf("credential leaked in metadata: %s", out.String())
				}
			})
		}
	}
}

func TestExportRedactionRejectsUnexpectedMetadataTypes(t *testing.T) {
	session := exportFixture(t)
	secret := "private nested metadata"
	entry := map[string]any{
		"type": map[string]any{"secret": secret}, "timestamp": secret, "provider": []any{secret},
		"request": map[string]any{"messages": []any{map[string]any{"role": map[string]any{"text": secret}, "content": map[string]any{"input_tokens": 123456789012345}}}, "max_output_tokens": secret, "search": secret},
		"data":    map[string]any{"input_tokens": secret, "success": map[string]any{"text": secret}, "tool_name": map[string]any{"name": secret}},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(session.FilePath, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []ExportFormat{FormatJSON, FormatRaw} {
		var out bytes.Buffer
		if err := Export(&out, session, ExportOptions{Format: format}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), secret) || strings.Contains(out.String(), "123456789012345") {
			t.Fatalf("unexpected types or numeric content leaked: %s", out.String())
		}
	}
}
