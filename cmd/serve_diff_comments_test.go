package cmd

import (
	"context"
	"encoding/json"
	"fmt"
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
	if got := message.Parts[0].DiffComment; got.Path != comment.Path || got.Scope != "last_turn" || got.Side != "old" || got.Line != 17 || got.FileChangeSeq != 42 {
		t.Fatalf("diff comment = %#v", got)
	}
}

func TestParseUserMessageContentAcceptsGitDiffCommentsWithoutFileChangeSequence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		wantScope string
	}{
		{name: "uncommitted", input: "uncommitted", wantScope: "uncommitted"},
		{name: "unstaged", input: "unstaged", wantScope: "unstaged"},
		{name: "trimmed uppercase staged", input: "  STAGED ", wantScope: "staged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comment := testDiffComment()
			comment.Scope = tc.input
			comment.FileChangeSeq = 0
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
			got := message.Parts[0].DiffComment
			if got == nil || got.Scope != tc.wantScope || got.FileChangeSeq != 0 {
				t.Fatalf("git diff comment = %#v", got)
			}
		})
	}
}

func TestParseUserMessageContentAcceptsDiffCommentBatchAndEnforcesCap(t *testing.T) {
	parts := make([]any, 0, diffCommentMaxPartsPerMessage+1)
	for i := 0; i < diffCommentMaxPartsPerMessage; i++ {
		comment := *testDiffComment()
		comment.ID = fmt.Sprintf("diff-comment-%d", i)
		parts = append(parts, map[string]any{"type": "diff_comment", "diff_comment": &comment})
	}
	parts = append(parts, map[string]any{"type": "input_text", "text": "one provider-facing batch"})
	content, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	message, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parse batch at cap: %v", err)
	}
	if len(message.Parts) != diffCommentMaxPartsPerMessage+1 {
		t.Fatalf("parts = %d, want %d", len(message.Parts), diffCommentMaxPartsPerMessage+1)
	}

	extra := *testDiffComment()
	extra.ID = "diff-comment-over-cap"
	parts = append(parts[:len(parts)-1], map[string]any{"type": "diff_comment", "diff_comment": &extra}, parts[len(parts)-1])
	content, err = json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseUserMessageContent(content); err == nil || !strings.Contains(err.Error(), "too many diff_comment parts (max 25)") {
		t.Fatalf("error = %v", err)
	}
}

func TestParseUserMessageContentRejectsDiffCommentAggregatePayloadOverCap(t *testing.T) {
	parts := make([]any, 0, 12)
	for i := 0; i < 11; i++ {
		comment := *testDiffComment()
		comment.ID = fmt.Sprintf("large-diff-comment-%d", i)
		comment.LineText = strings.Repeat("a", diffCommentMaxLineBytes)
		comment.Instruction = strings.Repeat("b", diffCommentMaxInstructionBytes)
		comment.ContextBefore = []llm.DiffCommentContextLine{{Side: "old", Line: 16, Text: strings.Repeat("c", diffCommentMaxLineBytes)}}
		comment.ContextAfter = nil
		parts = append(parts, map[string]any{"type": "diff_comment", "diff_comment": &comment})
	}
	parts = append(parts, map[string]any{"type": "input_text", "text": "provider-facing batch"})
	content, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseUserMessageContent(content); err == nil || !strings.Contains(err.Error(), "aggregate payload limit (262144 bytes)") {
		t.Fatalf("error = %v", err)
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

func TestParseUserMessageContentAcceptsLast3TurnsDiffComment(t *testing.T) {
	comment := testDiffComment()
	comment.Scope = fileChangeScopeLast3Turns
	comment.FileChangeSeq = 42
	content, err := json.Marshal([]any{
		map[string]any{"type": "diff_comment", "diff_comment": comment},
		map[string]any{"type": "input_text", "text": "provider text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseUserMessageContent(content); err != nil {
		t.Fatalf("parse last-three-turn comment: %v", err)
	}
}

func TestParseUserMessageContentRejectsInvalidDiffCommentScopeAndSequence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scope   string
		seq     int64
		wantErr string
	}{
		{name: "unknown scope", scope: "committed", seq: 0, wantErr: "scope must be"},
		{name: "last turn zero sequence", scope: "last_turn", seq: 0, wantErr: "must be positive for last_turn"},
		{name: "last three turns zero sequence", scope: "last_3_turns", seq: 0, wantErr: "must be positive for last_3_turns"},
		{name: "negative git sequence", scope: "staged", seq: -1, wantErr: "must be zero for Git diff scopes"},
		{name: "positive git sequence", scope: "unstaged", seq: 7, wantErr: "must be zero for Git diff scopes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comment := testDiffComment()
			comment.Scope = tc.scope
			comment.FileChangeSeq = tc.seq
			content, err := json.Marshal([]any{
				map[string]any{"type": "diff_comment", "diff_comment": comment},
				map[string]any{"type": "input_text", "text": "provider text"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseUserMessageContent(content); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
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
