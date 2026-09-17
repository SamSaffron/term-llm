package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestUploadedFileRemainsVisibleAfterPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, mime string
		raw        []byte
	}{
		{"notes.txt", "text/plain", []byte("private file body")},
		{"archive.zip", "application/zip", []byte{'P', 'K', 0, 255}},
		{"icon.svg", "image/svg+xml", []byte("<svg></svg>")},
	} {
		for _, display := range []string{"", "inspect this file"} {
			t.Run(tc.name+"/"+display, func(t *testing.T) {
				t.Setenv("XDG_DATA_HOME", t.TempDir())
				content, err := json.Marshal([]map[string]string{{"type": "input_file", "filename": tc.name, "file_data": "data:" + tc.mime + ";base64," + base64.StdEncoding.EncodeToString(tc.raw)}})
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := parseUserMessageContent(content)
				if err != nil {
					t.Fatal(err)
				}
				parsed.DisplayText = display
				store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				ctx := context.Background()
				sess := &session.Session{ID: "upload", Provider: "mock", Model: "mock", Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive}
				if err := store.Create(ctx, sess); err != nil {
					t.Fatal(err)
				}
				if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, parsed, -1)); err != nil {
					t.Fatal(err)
				}
				stored, err := store.GetMessages(ctx, sess.ID, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				part := stored[0].Parts[0]
				if part.FileData == nil || part.FileData.Base64 != base64.StdEncoding.EncodeToString(tc.raw) {
					t.Fatalf("upload data lost: %#v", part)
				}
				raw, err := os.ReadFile(part.FilePath)
				if err != nil || string(raw) != string(tc.raw) {
					t.Fatalf("saved bytes = %q, err = %v", raw, err)
				}
				srv := &serveServer{store: store}
				rr := httptest.NewRecorder()
				srv.handleSessionByID(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/upload/messages", nil))
				if rr.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
				}
				var body sessionMessagesResponse
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if len(body.Messages) != 1 {
					t.Fatalf("messages: %#v", body.Messages)
				}
				files := 0
				for _, p := range body.Messages[0].Parts {
					// The name-only chip projected for embedded @mention context has
					// no MIME type, so match the structured upload by name + type.
					if p.Type != "file" || p.Text != tc.name || p.MimeType != tc.mime {
						continue
					}
					files++
					wantURL := srv.cfg.uploadsRoute() + filepath.Base(part.FilePath)
					if p.FileURL != wantURL {
						t.Errorf("file_url = %q, want %q", p.FileURL, wantURL)
					}
					if p.SizeBytes != int64(len(tc.raw)) {
						t.Errorf("size_bytes = %d, want %d", p.SizeBytes, len(tc.raw))
					}
					if p.Kind != filePartKindUpload {
						t.Errorf("kind = %q, want %q", p.Kind, filePartKindUpload)
					}
					// The projected URL must serve the stored bytes unchanged.
					download := httptest.NewRecorder()
					srv.handleUpload(download, httptest.NewRequest(http.MethodGet, p.FileURL, nil))
					if download.Code != http.StatusOK || download.Body.String() != string(tc.raw) {
						t.Errorf("download %s status=%d body=%q", p.FileURL, download.Code, download.Body.String())
					}
				}
				if files != 1 {
					t.Errorf("want one visible file chip, got %#v", body.Messages[0].Parts)
				}
				// Assert against this case's own bytes, not a fixed literal, so a
				// regression that echoed the raw or base64 body would fail here.
				for _, secret := range []string{part.FilePath, string(tc.raw), base64.StdEncoding.EncodeToString(tc.raw)} {
					if strings.Contains(rr.Body.String(), secret) {
						t.Errorf("history leaked provider-only file content/path: %q", secret)
					}
				}

				// Consumed guidance and recovery must project the same stored attachment,
				// even for an attachment-only interjection with no display text.
				run := newResponseRun("resp-file", sess.ID, "", "mock", 1, nil)
				err = srv.appendResponseSteering(nil, run, &responseRunStreamState{}, llm.Event{SteeringID: "file-steering", Text: display, Message: stored[0].ToLLMMessage()})
				if err != nil {
					t.Fatal(err)
				}
				recovery := run.recoveryPayloadLocked()
				messages := recovery["messages"].([]map[string]any)
				atts, _ := messages[0]["attachments"].([]map[string]any)
				if len(atts) != 1 || atts[0]["name"] != tc.name || atts[0]["type"] != tc.mime || atts[0]["kind"] != filePartKindUpload {
					t.Errorf("steering attachments = %#v", atts)
				}
				if want := srv.cfg.uploadsRoute() + filepath.Base(part.FilePath); atts[0]["file_url"] != want {
					t.Errorf("steering file_url = %v, want %q", atts[0]["file_url"], want)
				}
				if atts[0]["size_bytes"] != int64(len(tc.raw)) {
					t.Errorf("steering size_bytes = %v, want %d", atts[0]["size_bytes"], len(tc.raw))
				}
				if mention, ok := atts[0]["mention"]; ok {
					t.Errorf("downloadable steering chip is marked as an inert mention: %v", mention)
				}
			})
		}
	}
}

// TestEmbeddedMentionChipIsReference checks the other half of the file chip
// contract: a name recovered from embedded @mention context is a reference, so
// it carries no download route and its inlined body stays out of the payload.
func TestEmbeddedMentionChipIsReference(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const body = "embedded reference body"
	embedded := llm.FormatEmbeddedFileText("docs/notes.md", "text/markdown", body)
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &session.Session{ID: "mention", Provider: "mock", Model: "mock", Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	msg := llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: embedded}}}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, -1)); err != nil {
		t.Fatal(err)
	}

	srv := &serveServer{store: store}
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, httptest.NewRequest(http.MethodGet, "/v1/sessions/mention/messages", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var response sessionMessagesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Messages) != 1 {
		t.Fatalf("messages: %#v", response.Messages)
	}
	chips := 0
	for _, p := range response.Messages[0].Parts {
		if p.Type != "file" {
			continue
		}
		chips++
		if p.Text != "notes.md" {
			t.Errorf("reference chip text = %q, want notes.md", p.Text)
		}
		if p.Kind != filePartKindReference {
			t.Errorf("reference chip kind = %q, want %q", p.Kind, filePartKindReference)
		}
		if p.FileURL != "" || p.SizeBytes != 0 || p.MimeType != "" {
			t.Errorf("reference chip exposed download metadata: %#v", p)
		}
	}
	if chips != 1 {
		t.Fatalf("want one reference chip, got %#v", response.Messages[0].Parts)
	}
	if strings.Contains(rr.Body.String(), body) || strings.Contains(rr.Body.String(), "--- BEGIN USER-PROVIDED FILE:") {
		t.Error("history leaked the embedded reference body")
	}
}
