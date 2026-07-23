package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func newTranscriptHandlerServer(t *testing.T) (*serveServer, session.Store, *session.Session) {
	t.Helper()
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sess := &session.Session{
		ID: "sess-transcript", Provider: "mock", Model: "mock-model", Mode: session.ModeChat,
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return &serveServer{store: store}, store, sess
}

func TestHandleSessionTranscriptReturnsCompleteCompactIdentityIndex(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	ctx := context.Background()
	messages := []*session.Message{
		session.NewMessage(sess.ID, llm.SystemText("secret"), -1),
		session.NewMessage(sess.ID, llm.UserText("hello"), -1),
		session.NewMessage(sess.ID, llm.AssistantText("answer"), -1),
		session.NewMessage(sess.ID, llm.Message{Role: llm.RoleEvent}, -1),
	}
	for _, msg := range messages {
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/transcript", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Rev  int64 `json:"rev"`
		Rows struct {
			Seqs  []int   `json:"seqs"`
			IDs   []int64 `json:"ids"`
			Roles string  `json:"roles"`
			Flags []uint8 `json:"flags"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Rev != 4 || body.Rows.Roles != "uae" {
		t.Fatalf("body=%+v", body)
	}
	if len(body.Rows.IDs) != 3 || body.Rows.IDs[0] != messages[1].ID || body.Rows.IDs[2] != messages[3].ID {
		t.Fatalf("ids=%v", body.Rows.IDs)
	}
	if len(body.Rows.Seqs) != len(body.Rows.IDs) || len(body.Rows.Flags) != len(body.Rows.IDs) {
		t.Fatalf("parallel arrays differ: seqs=%d ids=%d flags=%d", len(body.Rows.Seqs), len(body.Rows.IDs), len(body.Rows.Flags))
	}
	var wireBody struct {
		Rows struct {
			Flags json.RawMessage `json:"flags"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &wireBody); err != nil {
		t.Fatalf("decode wire shape: %v", err)
	}
	if len(wireBody.Rows.Flags) == 0 || wireBody.Rows.Flags[0] != '[' {
		t.Fatalf("flags must be a JSON number array for browser clients, got %s", wireBody.Rows.Flags)
	}
	if got := rr.Header().Get("X-Transcript-Rev"); got != "4" {
		t.Fatalf("X-Transcript-Rev=%q", got)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/transcript", nil)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("conditional status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleSessionTranscriptDoesNotTruncateIdentityIndex(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	ctx := context.Background()
	for i := 0; i < sessionMessagesPageSize+17; i++ {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText(fmt.Sprintf("row-%d", i)), -1)); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/transcript", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	var body struct {
		Rows struct {
			IDs []int64 `json:"ids"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got, want := len(body.Rows.IDs), sessionMessagesPageSize+17; got != want {
		t.Fatalf("identity index truncated: got=%d want=%d", got, want)
	}
}

func TestHandleSessionTranscriptBodiesExpandsWholeTurnAndMarksToolError(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	ctx := context.Background()
	callID := "call-broken"
	messages := []*session.Message{
		session.NewMessage(sess.ID, llm.UserText("do it"), -1),
		session.NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: callID, Name: "shell"}}}}, -1),
		session.NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: callID, Name: "shell", IsError: true}}}}, -1),
		session.NewMessage(sess.ID, llm.AssistantText("failed"), -1),
		session.NewMessage(sess.ID, llm.UserText("next turn"), -1),
	}
	for _, msg := range messages {
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage: %v", err)
		}
	}

	url := "/v1/sessions/" + sess.ID + "/transcript/bodies?ids=" + strconv.FormatInt(messages[1].ID, 10)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Rev      int64                 `json:"rev"`
		Messages []sessionMessageEntry `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("whole turn not returned: got=%d body=%s", len(body.Messages), rr.Body.String())
	}
	if len(body.Messages[1].Parts) != 1 || !body.Messages[1].Parts[0].ToolError {
		t.Fatalf("tool call lacks whole-turn error context: %+v", body.Messages[1])
	}
	if body.Messages[3].ID != messages[3].ID {
		t.Fatalf("turn expansion crossed boundary or omitted tail: %+v", body.Messages)
	}
	if got := rr.Header().Get("X-Transcript-Rev"); got != strconv.FormatInt(body.Rev, 10) {
		t.Fatalf("revision header=%q body rev=%d", got, body.Rev)
	}
}

func TestHandleSessionTranscriptBodiesLoadsLargeTurnAtomicallyWithFarEndToolContext(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	ctx := context.Background()
	const toolRows = 700
	const toolCalls = toolRows / 2
	messages := make([]*session.Message, 0, toolRows+2)
	messages = append(messages, session.NewMessage(sess.ID, llm.UserText("large tool turn"), -1))
	for i := 0; i < toolCalls; i++ {
		callID := fmt.Sprintf("call-%03d", i)
		name := "read_file"
		if i == toolCalls-1 {
			name = "update_plan"
		}
		messages = append(messages, session.NewMessage(sess.ID, llm.Message{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{
				ID: callID, Name: name,
			}}},
		}, -1))
		messages = append(messages, session.NewMessage(sess.ID, llm.Message{
			Role: llm.RoleTool,
			Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{
				ID: callID, Name: name, IsError: i == toolCalls-1,
			}}},
		}, -1))
	}
	messages = append(messages, session.NewMessage(sess.ID, llm.UserText("next turn"), -1))
	for i, msg := range messages {
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage %d: %v", i, err)
		}
	}

	url := "/v1/sessions/" + sess.ID + "/transcript/bodies?ids=" + strconv.FormatInt(messages[0].ID, 10)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Rev      int64                 `json:"rev"`
		Messages []sessionMessageEntry `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got, want := len(body.Messages), toolRows+1; got != want {
		t.Fatalf("large turn rows=%d want=%d", got, want)
	}
	if body.Messages[0].ID != messages[0].ID || body.Messages[len(body.Messages)-1].ID != messages[len(messages)-2].ID {
		t.Fatalf("response did not preserve the complete user-bounded turn: first=%d last=%d", body.Messages[0].ID, body.Messages[len(body.Messages)-1].ID)
	}
	farCall := body.Messages[len(body.Messages)-2]
	farResult := body.Messages[len(body.Messages)-1]
	if len(farCall.Parts) != 1 || farCall.Parts[0].ToolName != "update_plan" || !farCall.Parts[0].ToolError {
		t.Fatalf("far-end tool call lost result/error context: %+v", farCall)
	}
	if len(farResult.Parts) != 1 || farResult.Parts[0].ToolName != "update_plan" || !farResult.Parts[0].ToolError {
		t.Fatalf("far-end update_plan result lost conversion semantics: %+v", farResult)
	}
}

func TestHandleSessionTranscriptBodiesCapsTurnAnchorsAndMalformedQuerySize(t *testing.T) {
	srv, store, sess := newTranscriptHandlerServer(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText(fmt.Sprintf("turn-%d", i)), -1)); err != nil {
			t.Fatal(err)
		}
	}

	anchors := make([]string, transcriptBodiesMaxAnchors+1)
	for i := range anchors {
		anchors[i] = strconv.Itoa(i + 1)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/transcript/bodies?ids="+strings.Join(anchors, ","), nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), strconv.Itoa(transcriptBodiesMaxAnchors)) {
		t.Fatalf("anchor cap status=%d body=%s", rr.Code, rr.Body.String())
	}

	oversized := strings.Repeat("1,", transcriptBodiesMaxQueryBytes) + "1"
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/transcript/bodies?ids="+oversized, nil)
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "too long") {
		t.Fatalf("query size status=%d body=%s", rr.Code, rr.Body.String())
	}
}
