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
					if p.Type == "file" && p.Text == tc.name && p.MimeType == tc.mime {
						files++
					}
				}
				if files != 1 {
					t.Errorf("want one visible file chip, got %#v", body.Messages[0].Parts)
				}
				if strings.Contains(rr.Body.String(), part.FilePath) || strings.Contains(rr.Body.String(), "private file body") {
					t.Error("history leaked provider-only file content/path")
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
				if len(atts) != 1 || atts[0]["name"] != tc.name || atts[0]["type"] != tc.mime || atts[0]["mention"] != true {
					t.Errorf("steering attachments = %#v", atts)
				}
			})
		}
	}
}
