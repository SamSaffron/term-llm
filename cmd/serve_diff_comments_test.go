package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func testDiffComment() *llm.DiffComment {
	return &llm.DiffComment{
		ID:            "diff-comment-1",
		Path:          "internal/example.go",
		Side:          "old",
		Line:          17,
		FileChangeSeq: 42,
		LineText:      "return obsolete",
		ContextBefore: []llm.DiffCommentContextLine{{Side: "old", Line: 16, Text: "if stale {"}},
		ContextAfter:  []llm.DiffCommentContextLine{{Side: "old", Line: 18, Text: "}"}},
		Instruction:   "Keep the compatibility fallback.",
	}
}

func TestParseUserMessageContentAcceptsDiffCommentMetadata(t *testing.T) {
	comment := testDiffComment()
	content, err := json.Marshal([]any{
		map[string]any{"type": "diff_comment", "diff_comment": comment},
		map[string]any{"type": "input_text", "text": "provider-facing anchored instruction"},
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent: %v", err)
	}
	if len(message.Parts) != 2 || message.Parts[0].Type != llm.PartDiffComment || message.Parts[0].DiffComment == nil {
		t.Fatalf("parts = %#v", message.Parts)
	}
	if got := message.Parts[0].DiffComment; got.Path != comment.Path || got.Side != "old" || got.Line != 17 || got.FileChangeSeq != 42 {
		t.Fatalf("diff comment = %#v", got)
	}
}

func TestParseUserMessageContentRejectsInvalidDiffCommentMetadata(t *testing.T) {
	content := json.RawMessage(`[
		{"type":"diff_comment","diff_comment":{"id":"bad","path":"x.go","side":"middle","line":1,"file_change_seq":2,"instruction":"fix"}},
		{"type":"input_text","text":"provider text"}
	]`)
	if _, err := parseUserMessageContent(content); err == nil || !strings.Contains(err.Error(), "side must be old or new") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseUserMessageContentRejectsOversizedDiffCommentPayloads(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*llm.DiffComment)
		wantErr string
	}{
		{
			name: "anchor line",
			mutate: func(comment *llm.DiffComment) {
				comment.LineText = strings.Repeat("x", diffCommentMaxLineBytes+1)
			},
			wantErr: "line_text",
		},
		{
			name: "context line",
			mutate: func(comment *llm.DiffComment) {
				comment.ContextBefore = []llm.DiffCommentContextLine{{Side: "old", Line: 1, Text: strings.Repeat("x", diffCommentMaxLineBytes+1)}}
			},
			wantErr: "context line text",
		},
		{
			name: "total text",
			mutate: func(comment *llm.DiffComment) {
				comment.LineText = strings.Repeat("a", diffCommentMaxLineBytes)
				comment.Instruction = strings.Repeat("b", diffCommentMaxInstructionBytes)
				comment.ContextBefore = make([]llm.DiffCommentContextLine, 4)
				comment.ContextAfter = make([]llm.DiffCommentContextLine, 4)
				for i := range comment.ContextBefore {
					comment.ContextBefore[i] = llm.DiffCommentContextLine{Side: "old", Line: i + 1, Text: strings.Repeat("c", 2200)}
					comment.ContextAfter[i] = llm.DiffCommentContextLine{Side: "new", Line: i + 1, Text: strings.Repeat("d", 2200)}
				}
			},
			wantErr: "text payload",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comment := testDiffComment()
			tt.mutate(comment)
			raw, err := json.Marshal(comment)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseDiffCommentPart(raw); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestHandleSessionInterruptAcceptsTypedDiffComment(t *testing.T) {
	manager := newServeSessionManager(time.Minute, 10, nil)
	defer manager.Close()
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	runtime := &serveRuntime{engine: engine, providerKey: "mock"}
	active := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	runtime.setActiveInterrupt(active)
	defer runtime.clearActiveInterrupt(active)
	putTestSession(manager, "session-interrupt-comment", runtime)

	comment := testDiffComment()
	content, err := json.Marshal([]any{
		map[string]any{"type": "diff_comment", "diff_comment": comment},
		map[string]any{"type": "input_text", "text": "provider-facing anchored instruction"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"message":           "provider-facing anchored instruction",
		"content":           json.RawMessage(content),
		"interjection_id":   "interrupt-comment-1",
		"client_message_id": "interrupt-comment-1",
		"delivery":          "steer",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/session-interrupt-comment/interrupt", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	(&serveServer{sessionMgr: manager}).handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	pending := engine.ListPendingInterjections()
	if len(pending) != 1 || len(pending[0].Message.Parts) != 2 || pending[0].Message.Parts[0].Type != llm.PartDiffComment {
		t.Fatalf("pending interjections = %#v", pending)
	}
}

func TestDiffCommentMetadataIsStrippedFromProviderMessages(t *testing.T) {
	comment := testDiffComment()
	stored := session.NewMessage("session-comments", llm.Message{Role: llm.RoleUser, Parts: []llm.Part{
		{Type: llm.PartDiffComment, DiffComment: comment},
		{Type: llm.PartText, Text: "provider-facing anchored instruction"},
	}}, -1)
	providerMessage := stored.ToLLMMessage()
	if len(providerMessage.Parts) != 1 || providerMessage.Parts[0].Type != llm.PartText {
		t.Fatalf("provider parts = %#v", providerMessage.Parts)
	}
}

func TestHandleSessionDiffCommentsScansTypedTranscriptParts(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const sessionID = "session-comments"
	if err := store.Create(ctx, &session.Session{ID: sessionID, Provider: "test", Model: "test", Mode: session.ModeChat}); err != nil {
		t.Fatal(err)
	}
	message := session.NewMessage(sessionID, llm.Message{Role: llm.RoleUser, ClientMessageID: "client-comment-1", Parts: []llm.Part{
		{Type: llm.PartDiffComment, DiffComment: testDiffComment()},
		{Type: llm.PartText, Text: "provider-facing anchored instruction"},
	}}, -1)
	if err := store.AddMessage(ctx, sessionID, message); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sessionID, session.NewMessage(sessionID, llm.UserText("prose mentioning diff_comment must not count"), -1)); err != nil {
		t.Fatal(err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sessionID+"/diff-comments", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload struct {
		Comments      []sessionDiffCommentEntry `json:"comments"`
		TranscriptRev int64                     `json:"transcript_rev"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Comments) != 1 || payload.Comments[0].Comment == nil || payload.TranscriptRev <= 0 {
		t.Fatalf("payload = %#v", payload)
	}
	if got := payload.Comments[0]; got.ClientMessageID != "client-comment-1" || got.Comment.Instruction != "Keep the compatibility fallback." {
		t.Fatalf("comment = %#v", got)
	}

	entries := srv.sessionMessageEntries([]session.Message{*message})
	if len(entries) != 1 || len(entries[0].Parts) != 2 || entries[0].Parts[0].Type != "diff_comment" {
		t.Fatalf("transcript projection = %#v", entries)
	}
}
