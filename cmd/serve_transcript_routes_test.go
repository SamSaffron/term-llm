package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestHandleSessionMessages_ReturnsStructuredParts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-parts",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Message with text + multiple tool calls (the case that was lossy before)
	msg := session.NewMessage("sess-parts", llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "Let me search for that"},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-1", Name: "web_search", Arguments: json.RawMessage(`{"query":"go"}`)}},
			{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "call-2", Name: "read_url", Arguments: json.RawMessage(`{"url":"https://go.dev"}`)}},
			{Type: llm.PartToolActivity, ToolActivity: &llm.ToolActivity{ID: "ws-native", Name: llm.WebSearchToolName, Info: "(discourse news)", Arguments: json.RawMessage(`{"query":"discourse news"}`), Status: llm.ToolActivityCompleted}},
		},
	}, -1)
	if err := store.AddMessage(ctx, "sess-parts", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-parts/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				ToolName   string `json:"tool_name"`
				ToolInfo   string `json:"tool_info"`
				ToolArgs   string `json:"tool_arguments"`
				ToolCallID string `json:"tool_call_id"`
				ToolStatus string `json:"tool_status"`
				ImageURL   string `json:"image_url"`
				MimeType   string `json:"mime_type"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	m := body.Messages[0]
	if m.Role != "assistant" {
		t.Fatalf("role = %q, want assistant", m.Role)
	}
	if len(m.Parts) != 4 {
		t.Fatalf("parts count = %d, want 4", len(m.Parts))
	}

	// Text part
	if m.Parts[0].Type != "text" || m.Parts[0].Text != "Let me search for that" {
		t.Fatalf("part[0] = %+v, want text part", m.Parts[0])
	}
	// First tool call
	if m.Parts[1].Type != "tool_call" || m.Parts[1].ToolName != "web_search" || m.Parts[1].ToolCallID != "call-1" {
		t.Fatalf("part[1] = %+v, want web_search tool_call", m.Parts[1])
	}
	if m.Parts[1].ToolArgs != `{"query":"go"}` {
		t.Fatalf("part[1].args = %q", m.Parts[1].ToolArgs)
	}
	// Second tool call (was lost before due to break)
	if m.Parts[2].Type != "tool_call" || m.Parts[2].ToolName != "read_url" || m.Parts[2].ToolCallID != "call-2" {
		t.Fatalf("part[2] = %+v, want read_url tool_call", m.Parts[2])
	}
	// Provider-managed activity is display-only but must reach web history.
	if m.Parts[3].Type != "tool_activity" || m.Parts[3].ToolName != "web_search" || m.Parts[3].ToolCallID != "ws-native" ||
		m.Parts[3].ToolInfo != "(discourse news)" || m.Parts[3].ToolArgs != `{"query":"discourse news"}` || m.Parts[3].ToolStatus != llm.ToolActivityCompleted {
		t.Fatalf("part[3] = %+v, want native web search activity", m.Parts[3])
	}
}

func TestHandleSessionMessages_DefaultPagination(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-page",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var latestVisibleID int64
	for i := 0; i < sessionMessagesPageSize+5; i++ {
		msg := session.NewMessage("sess-page", llm.UserText(fmt.Sprintf("msg-%03d", i)), -1)
		if err := store.AddMessage(ctx, "sess-page", msg); err != nil {
			t.Fatalf("AddMessage(%d): %v", i, err)
		}
		latestVisibleID = msg.ID
	}

	srv := &serveServer{store: store}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-page/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		LastResponseID string `json:"lastResponseId"`
		Messages       []struct {
			Sequence int `json:"sequence"`
			Parts    []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
		HasMore    bool `json:"has_more"`
		NextOffset int  `json:"next_offset"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Messages) != sessionMessagesPageSize {
		t.Fatalf("message count = %d, want %d", len(body.Messages), sessionMessagesPageSize)
	}
	if got := body.LastResponseID; got != durableResponseIDForMessageID(latestVisibleID) {
		t.Fatalf("first page lastResponseId = %q, want %q", got, durableResponseIDForMessageID(latestVisibleID))
	}
	if !body.HasMore {
		t.Fatal("expected has_more=true for truncated history page")
	}
	if body.NextOffset != sessionMessagesPageSize {
		t.Fatalf("next_offset = %d, want %d", body.NextOffset, sessionMessagesPageSize)
	}
	if body.Messages[0].Sequence != 0 {
		t.Fatalf("first message sequence = %d, want 0", body.Messages[0].Sequence)
	}
	if got := body.Messages[0].Parts[0].Text; got != "msg-000" {
		t.Fatalf("first message text = %q, want msg-000", got)
	}
	if got := body.Messages[len(body.Messages)-1].Parts[0].Text; got != fmt.Sprintf("msg-%03d", sessionMessagesPageSize-1) {
		t.Fatalf("last message text = %q, want %q", got, fmt.Sprintf("msg-%03d", sessionMessagesPageSize-1))
	}

	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/sessions/sess-page/messages?limit=%d&offset=%d", sessionMessagesPageSize, sessionMessagesPageSize), nil)
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("page 2 status = %d, want 200", rr.Code)
	}
	body = struct {
		LastResponseID string `json:"lastResponseId"`
		Messages       []struct {
			Sequence int `json:"sequence"`
			Parts    []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
		HasMore    bool `json:"has_more"`
		NextOffset int  `json:"next_offset"`
	}{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode page 2: %v", err)
	}
	if len(body.Messages) != 5 {
		t.Fatalf("page 2 message count = %d, want 5", len(body.Messages))
	}
	if got := body.LastResponseID; got != durableResponseIDForMessageID(latestVisibleID) {
		t.Fatalf("page 2 lastResponseId = %q, want %q", got, durableResponseIDForMessageID(latestVisibleID))
	}
	if body.HasMore {
		t.Fatal("expected has_more=false on final page")
	}
	if body.NextOffset != 0 {
		t.Fatalf("final next_offset = %d, want 0", body.NextOffset)
	}
	if got := body.Messages[0].Parts[0].Text; got != fmt.Sprintf("msg-%03d", sessionMessagesPageSize) {
		t.Fatalf("page 2 first message text = %q, want %q", got, fmt.Sprintf("msg-%03d", sessionMessagesPageSize))
	}
}

func TestHandleSessionMessages_TailPagination(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-tail",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < sessionMessagesPageSize+5; i++ {
		msg := session.NewMessage("sess-tail", llm.UserText(fmt.Sprintf("msg-%03d", i)), -1)
		if err := store.AddMessage(ctx, "sess-tail", msg); err != nil {
			t.Fatalf("AddMessage(%d): %v", i, err)
		}
	}

	srv := &serveServer{store: store}
	type messagePage struct {
		Messages []struct {
			Sequence       int  `json:"sequence"`
			CompactionTail bool `json:"compaction_tail"`
			Parts          []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
		HasMore       bool `json:"has_more"`
		NextOffset    int  `json:"next_offset"`
		NextBeforeSeq int  `json:"next_before_seq"`
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-tail/messages?tail=1&limit=50", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("tail status = %d, want 200", rr.Code)
	}
	var body messagePage
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode tail: %v", err)
	}
	if len(body.Messages) != 50 {
		t.Fatalf("tail message count = %d, want 50", len(body.Messages))
	}
	if !body.HasMore {
		t.Fatal("expected tail has_more=true")
	}
	if body.NextOffset != 0 {
		t.Fatalf("tail next_offset = %d, want 0", body.NextOffset)
	}
	if body.NextBeforeSeq != 155 {
		t.Fatalf("tail next_before_seq = %d, want 155", body.NextBeforeSeq)
	}
	if body.Messages[0].Sequence != 155 || body.Messages[len(body.Messages)-1].Sequence != 204 {
		t.Fatalf("tail sequences = [%d..%d], want [155..204]", body.Messages[0].Sequence, body.Messages[len(body.Messages)-1].Sequence)
	}
	if got := body.Messages[0].Parts[0].Text; got != "msg-155" {
		t.Fatalf("tail first message text = %q, want msg-155", got)
	}
	if got := body.Messages[len(body.Messages)-1].Parts[0].Text; got != "msg-204" {
		t.Fatalf("tail last message text = %q, want msg-204", got)
	}

	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/sessions/sess-tail/messages?before_seq=%d&limit=50", body.NextBeforeSeq), nil)
	rr = httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("older page status = %d, want 200", rr.Code)
	}
	var older messagePage
	if err := json.Unmarshal(rr.Body.Bytes(), &older); err != nil {
		t.Fatalf("decode older page: %v", err)
	}
	if len(older.Messages) != 50 {
		t.Fatalf("older page message count = %d, want 50", len(older.Messages))
	}
	if !older.HasMore {
		t.Fatal("expected older page has_more=true")
	}
	if older.NextBeforeSeq != 105 {
		t.Fatalf("older next_before_seq = %d, want 105", older.NextBeforeSeq)
	}
	if older.Messages[0].Sequence != 105 || older.Messages[len(older.Messages)-1].Sequence != 154 {
		t.Fatalf("older page sequences = [%d..%d], want [105..154]", older.Messages[0].Sequence, older.Messages[len(older.Messages)-1].Sequence)
	}
	if got := older.Messages[0].Parts[0].Text; got != "msg-105" {
		t.Fatalf("older first message text = %q, want msg-105", got)
	}
}

func TestHandleSessionMessages_IncludesCompactionMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-compact-page",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText("old prompt"), -1)); err != nil {
		t.Fatalf("AddMessage old: %v", err)
	}
	compactionTail := session.NewMessage(sess.ID, llm.AssistantText("recent answer"), -1)
	compactionTail.CompactionTail = true
	if err := store.CompactMessages(ctx, sess.ID, []session.Message{
		*session.NewMessage(sess.ID, llm.UserText("[Context Compaction]\ninternal summary"), -1),
		*compactionTail,
	}); err != nil {
		t.Fatalf("CompactMessages: %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-compact-page/messages?tail=1&limit=1", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Messages []struct {
			Sequence       int  `json:"sequence"`
			CompactionTail bool `json:"compaction_tail"`
			Parts          []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
		CompactionSeq   *int `json:"compaction_seq"`
		CompactionCount int  `json:"compaction_count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.CompactionSeq == nil || *body.CompactionSeq != 1 {
		if body.CompactionSeq == nil {
			t.Fatalf("compaction_seq missing, want 1; body=%s", rr.Body.String())
		}
		t.Fatalf("compaction_seq = %d, want 1", *body.CompactionSeq)
	}
	if body.CompactionCount != 1 {
		t.Fatalf("compaction_count = %d, want 1", body.CompactionCount)
	}
	if len(body.Messages) != 1 || body.Messages[0].Sequence != 2 || !body.Messages[0].CompactionTail || len(body.Messages[0].Parts) == 0 || body.Messages[0].Parts[0].Text != "recent answer" {
		t.Fatalf("tail messages = %+v, want only hidden recent answer at sequence 2", body.Messages)
	}
}

func TestHandleSessionMessages_OmitsCompactionMetadataWhenUncompacted(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "sess-uncompact-page",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText("hello"), -1)); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-uncompact-page/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "compaction_seq") || strings.Contains(rr.Body.String(), "compaction_count") {
		t.Fatalf("uncompacted session should omit compaction metadata, got body=%s", rr.Body.String())
	}
}

func TestHandleSessionMessages_IncludesOrientationCorrectImageParts(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-image", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rawImage := makeOrientation6JPEG(t, 400, 200)
	content, err := json.Marshal([]map[string]any{
		{
			"type": "input_image", "image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(rawImage),
			"filename": "rotated.jpg", "width": 200, "height": 400,
		},
		{"type": "input_text", "text": "describe this"},
	})
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	parsed, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent: %v", err)
	}
	msg := session.NewMessage("sess-image", parsed, -1)
	if err := store.AddMessage(ctx, "sess-image", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	outputDir := t.TempDir()
	srv := &serveServer{store: store, cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = outputDir

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-image/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL string `json:"image_url"`
				MimeType string `json:"mime_type"`
				Width    int    `json:"width"`
				Height   int    `json:"height"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	m := body.Messages[0]
	if m.Role != "user" {
		t.Fatalf("role = %q, want user", m.Role)
	}
	if len(m.Parts) != 2 {
		t.Fatalf("parts count = %d, want 2", len(m.Parts))
	}
	if m.Parts[0].Type != "image" {
		t.Fatalf("part[0].type = %q, want image", m.Parts[0].Type)
	}
	if !strings.HasPrefix(m.Parts[0].ImageURL, "/images/") {
		t.Fatalf("part[0].image_url = %q, want /images/*", m.Parts[0].ImageURL)
	}
	if m.Parts[0].MimeType != "image/jpeg" {
		t.Fatalf("part[0].mime_type = %q, want image/jpeg", m.Parts[0].MimeType)
	}
	if m.Parts[0].Width != 200 || m.Parts[0].Height != 400 {
		t.Fatalf("part[0] dimensions = %dx%d, want EXIF-oriented 200x400", m.Parts[0].Width, m.Parts[0].Height)
	}
	if m.Parts[1].Type != "text" || m.Parts[1].Text != "describe this" {
		t.Fatalf("part[1] = %+v, want trailing text part", m.Parts[1])
	}

	imgReq := httptest.NewRequest(http.MethodGet, m.Parts[0].ImageURL, nil)
	imgRR := httptest.NewRecorder()
	srv.handleImage(imgRR, imgReq)
	if imgRR.Code != http.StatusOK {
		t.Fatalf("image status = %d, want 200", imgRR.Code)
	}
	if got := imgRR.Body.Bytes(); !bytes.Equal(got, rawImage) {
		t.Fatalf("image body differs from persisted orientation-6 JPEG")
	}
}

func TestSessionMessageImageDimensionsPrefersInlineBase64AndAppliesEXIF(t *testing.T) {
	fallback, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(onePixelPNGDataURL, "data:image/png;base64,"))
	if err != nil {
		t.Fatalf("decode PNG fallback: %v", err)
	}
	fallbackPath := filepath.Join(t.TempDir(), "fallback.png")
	if err := os.WriteFile(fallbackPath, fallback, 0o600); err != nil {
		t.Fatalf("write PNG fallback: %v", err)
	}
	rotated := makeOrientation6JPEG(t, 400, 200)
	part := llm.Part{
		Type:      llm.PartImage,
		ImagePath: filepath.Join(t.TempDir(), "untrusted-session-path.jpg"),
		ImageData: &llm.ToolImageData{MediaType: "image/jpeg", Base64: base64.StdEncoding.EncodeToString(rotated)},
	}
	width, height := sessionMessageImageDimensions(part, fallbackPath)
	if width != 200 || height != 400 {
		t.Fatalf("session image dimensions = %dx%d, want inline EXIF-oriented 200x400", width, height)
	}
}

func TestHandleSessionMessages_OmitsToolResults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-tr", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := session.NewMessage("sess-tr", llm.Message{
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "result msg"},
			{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "call-1", Content: "verbose tool output"}},
		},
	}, -1)
	if err := store.AddMessage(ctx, "sess-tr", msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-tr/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Parts []struct{ Type string } `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(body.Messages))
	}
	for _, p := range body.Messages[0].Parts {
		if p.Type == "tool_result" {
			t.Fatal("tool_result parts should be omitted from API response")
		}
	}
	if len(body.Messages[0].Parts) != 1 {
		t.Fatalf("parts count = %d, want 1 (text only)", len(body.Messages[0].Parts))
	}
}

func TestHandleSessionMessages_IncludesToolResultImages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-tool-image", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	imgDir := t.TempDir()
	imgPath := filepath.Join(imgDir, "generated.png")
	if err := os.WriteFile(imgPath, []byte("image-data"), 0644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	assistant := session.NewMessage("sess-tool-image", llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type: llm.PartToolCall,
			ToolCall: &llm.ToolCall{
				ID:        "call-img",
				Name:      "image_generate",
				Arguments: json.RawMessage(`{"prompt":"a cat"}`),
			},
		}},
	}, -1)
	if err := store.AddMessage(ctx, "sess-tool-image", assistant); err != nil {
		t.Fatalf("AddMessage assistant: %v", err)
	}
	toolResult := session.NewMessage("sess-tool-image", llm.ToolResultMessageFromOutput("call-img", "image_generate", llm.ToolOutput{
		Content: "Generated image successfully.",
		Images:  []string{imgPath},
	}, nil), -1)
	if err := store.AddMessage(ctx, "sess-tool-image", toolResult); err != nil {
		t.Fatalf("AddMessage tool result: %v", err)
	}

	srv := &serveServer{store: store, cfgRef: &config.Config{}}
	srv.cfgRef.Image.OutputDir = imgDir

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-tool-image/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role  string `json:"role"`
			Parts []struct {
				Type       string   `json:"type"`
				ToolName   string   `json:"tool_name"`
				ToolCallID string   `json:"tool_call_id"`
				Images     []string `json:"images"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(body.Messages))
	}
	if got := body.Messages[1].Role; got != "tool" {
		t.Fatalf("tool result role = %q, want tool", got)
	}
	if len(body.Messages[1].Parts) != 1 {
		t.Fatalf("tool result parts count = %d, want 1", len(body.Messages[1].Parts))
	}
	part := body.Messages[1].Parts[0]
	if part.Type != "tool_result" || part.ToolName != "image_generate" || part.ToolCallID != "call-img" {
		t.Fatalf("tool result part = %+v, want image_generate tool_result", part)
	}
	if len(part.Images) != 1 || !strings.HasPrefix(part.Images[0], "/images/") {
		t.Fatalf("tool result images = %v, want /images/*", part.Images)
	}
}

func TestHandleSessionMessages_OmitsSystemAndDeveloperMessages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID: "sess-sys", Provider: "mock", Model: "mock-model",
		Mode: session.ModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Add a system message (as persisted by TUI chat sessions)
	sysMsg := session.NewMessage("sess-sys", llm.SystemText("You are a helpful assistant."), -1)
	if err := store.AddMessage(ctx, "sess-sys", sysMsg); err != nil {
		t.Fatalf("AddMessage(system): %v", err)
	}
	// Add a developer message (platform developer prompt, persisted with role=developer)
	devMsg := session.NewMessage("sess-sys", llm.Message{
		Role:  llm.RoleDeveloper,
		Parts: []llm.Part{{Type: llm.PartText, Text: "You are Jarvis, a personal AI assistant."}},
	}, -1)
	if err := store.AddMessage(ctx, "sess-sys", devMsg); err != nil {
		t.Fatalf("AddMessage(developer): %v", err)
	}
	// Add a user message
	userMsg := session.NewMessage("sess-sys", llm.UserText("hello"), -1)
	if err := store.AddMessage(ctx, "sess-sys", userMsg); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	// Add an assistant message
	assistantMsg := session.NewMessage("sess-sys", llm.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartText, Text: "hi there"}},
	}, -1)
	if err := store.AddMessage(ctx, "sess-sys", assistantMsg); err != nil {
		t.Fatalf("AddMessage(assistant): %v", err)
	}

	srv := &serveServer{store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-sys/messages", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Messages) != 2 {
		t.Fatalf("message count = %d, want 2 (system+developer should be filtered)", len(body.Messages))
	}
	for _, m := range body.Messages {
		if m.Role == "system" {
			t.Fatal("system messages should be filtered from API response")
		}
		if m.Role == "developer" {
			t.Fatal("developer messages should be filtered from API response")
		}
	}
}

func TestRun_PersistsInjectedSystemPromptInHistoryAndStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock").AddTextResponse("hi there")
	rt := &serveRuntime{
		store:        store,
		provider:     provider,
		engine:       llm.NewEngine(provider, nil),
		defaultModel: "mock-model",
		systemPrompt: "server system prompt",
	}

	ctx := context.Background()
	_, err = rt.Run(ctx, true, false, []llm.Message{llm.UserText("hello")}, llm.Request{SessionID: "persist-system"})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if len(rt.history) != 3 {
		t.Fatalf("history len = %d, want 3", len(rt.history))
	}
	if rt.history[0].Role != llm.RoleSystem || rt.history[0].Parts[0].Text != "server system prompt" {
		t.Fatalf("history[0] = %+v, want injected system prompt", rt.history[0])
	}

	msgs, err := store.GetMessages(ctx, "persist-system", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("stored message count = %d, want 3", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].TextContent != "server system prompt" {
		t.Fatalf("stored first message = %+v, want injected system prompt", msgs[0])
	}
}

func TestEnsurePersistedSession_RestoresHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "restore-test",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msgs := []session.Message{
		*session.NewMessage("restore-test", llm.UserText("hello"), -1),
		*session.NewMessage("restore-test", llm.Message{
			Role:  llm.RoleAssistant,
			Parts: []llm.Part{{Type: llm.PartText, Text: "hi there"}},
		}, -1),
	}
	if err := store.ReplaceMessages(ctx, "restore-test", msgs); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	// Simulate a fresh runtime with empty history
	rt := &serveRuntime{
		store:        store,
		defaultModel: "mock-model",
		provider:     llm.NewMockProvider("mock"),
	}

	ok := rt.ensurePersistedSession(ctx, "restore-test", nil)
	if !ok {
		t.Fatalf("ensurePersistedSession returned false")
	}
	if len(rt.history) != 2 {
		t.Fatalf("history len = %d, want 2", len(rt.history))
	}
	if rt.history[0].Role != llm.RoleUser {
		t.Fatalf("history[0].role = %s, want user", rt.history[0].Role)
	}
	if rt.history[1].Role != llm.RoleAssistant {
		t.Fatalf("history[1].role = %s, want assistant", rt.history[1].Role)
	}
	if rt.history[1].Parts[0].Text != "hi there" {
		t.Fatalf("history[1].text = %q, want %q", rt.history[1].Parts[0].Text, "hi there")
	}
}

func TestEnsurePersistedSession_RestoresHistoryWhenMetadataAlreadyLoaded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "metadata-before-history",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.ReplaceMessages(ctx, sess.ID, []session.Message{
		*session.NewMessage(sess.ID, llm.UserText("persisted question"), -1),
		*session.NewMessage(sess.ID, llm.AssistantText("persisted answer"), -1),
	}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	// Metadata-only request setup can run before the first post-restart turn.
	rt := &serveRuntime{
		store:        store,
		defaultModel: "mock-model",
		provider:     llm.NewMockProvider("mock"),
		sessionMeta:  sess,
	}
	if ok := rt.ensurePersistedSession(ctx, sess.ID, nil); !ok {
		t.Fatal("ensurePersistedSession returned false")
	}
	if !rt.historyPersisted {
		t.Fatal("historyPersisted = false, want true after restore")
	}
	if len(rt.history) != 2 || rt.history[0].Parts[0].Text != "persisted question" || rt.history[1].Parts[0].Text != "persisted answer" {
		t.Fatalf("restored history = %#v, want persisted conversation", rt.history)
	}
}

func TestEnsurePersistedSession_SkipsRestoreWhenHistoryExists(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &session.Session{
		ID:        "skip-restore",
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dbMsg := session.NewMessage("skip-restore", llm.UserText("db message"), -1)
	if err := store.ReplaceMessages(ctx, "skip-restore", []session.Message{*dbMsg}); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	// Runtime already has history — should NOT overwrite
	existing := []llm.Message{llm.UserText("existing")}
	rt := &serveRuntime{
		store:        store,
		defaultModel: "mock-model",
		provider:     llm.NewMockProvider("mock"),
		history:      existing,
	}

	ok := rt.ensurePersistedSession(ctx, "skip-restore", nil)
	if !ok {
		t.Fatalf("ensurePersistedSession returned false")
	}
	if len(rt.history) != 1 {
		t.Fatalf("history len = %d, want 1 (unchanged)", len(rt.history))
	}
	if rt.history[0].Parts[0].Text != "existing" {
		t.Fatalf("history was overwritten")
	}
}

type testServeDelayTool struct {
	delay time.Duration
}

func (d *testServeDelayTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "slow_tool", Description: "delay for steering test"}
}

func (d *testServeDelayTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	select {
	case <-ctx.Done():
		return llm.ToolOutput{}, ctx.Err()
	case <-time.After(d.delay):
	}
	return llm.ToolOutput{Content: "slept"}, nil
}

func (d *testServeDelayTool) Preview(args json.RawMessage) string {
	return ""
}

// newTestServeServer creates a serveServer with a mock factory for testing.
// Each runtime gets its own mock provider with the given responses.
func newTestServeServer(responses ...string) *serveServer {
	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		for _, r := range responses {
			provider.AddTextResponse(r)
		}
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	return srv
}

// doResponses sends a non-streaming /v1/responses request and returns the parsed response body.
func doResponses(t *testing.T, srv *serveServer, bodyJSON string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	var result map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &result)
	}
	return rr.Code, result
}

// doResponsesFirstParty sends a non-streaming first-party UI request.
func doResponsesFirstParty(t *testing.T, srv *serveServer, bodyJSON string, sessionID ...string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	if len(sessionID) > 0 && sessionID[0] != "" {
		req.Header.Set("session_id", sessionID[0])
	}
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	var result map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &result)
	}
	return rr.Code, result
}

// doResponsesWithHeader sends a non-streaming /v1/responses request with a session_id header.
func doResponsesWithHeader(t *testing.T, srv *serveServer, bodyJSON, sessionID string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("session_id", sessionID)
	}
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)
	var result map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &result)
	}
	return rr.Code, result
}

func responseOutputText(t *testing.T, resp map[string]any) string {
	t.Helper()
	output, ok := resp["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("response output = %#v, want assistant message", resp["output"])
	}
	msg, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("first output item = %#v, want object", output[0])
	}
	content, ok := msg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("message content = %#v, want output_text", msg["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("first content part = %#v, want object", content[0])
	}
	text, _ := part["text"].(string)
	if text == "" {
		t.Fatalf("response text missing from %#v", part)
	}
	return text
}

// waitForServeCondition polls until fn returns true or timeout elapses.
func waitForServeCondition(t *testing.T, timeout time.Duration, fn func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func TestHandleResponses_UIAskUserResumeSurvivesDisconnect(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call_ask_1", tools.AskUserToolName, map[string]any{
		"questions": []map[string]any{{
			"header":   "Theme",
			"question": "Pick a theme",
			"options": []map[string]any{
				{"label": "Dark", "description": "Use dark mode"},
				{"label": "Light", "description": "Use light mode"},
			},
		}},
	})
	provider.AddTextResponse("All set.")

	engine := llm.NewEngine(provider, nil)
	engine.RegisterTool(tools.NewAskUserTool())

	rt := &serveRuntime{
		provider:     provider,
		engine:       engine,
		store:        store,
		defaultModel: "mock-model",
	}
	rt.Touch()

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr, store: store}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body := `{"stream":true,"input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("session_id", "resume-session")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		srv.handleResponses(rr, req)
	}()

	waitForServeCondition(t, time.Second, func() bool {
		return len(rt.pendingAskUserPrompts()) == 1
	}, "pending ask_user prompt")

	stateReq := httptest.NewRequest(http.MethodGet, "/v1/sessions/resume-session/state", nil)
	stateRR := httptest.NewRecorder()
	srv.handleSessionByID(stateRR, stateReq)
	if stateRR.Code != http.StatusOK {
		t.Fatalf("state status = %d, want 200", stateRR.Code)
	}
	var stateBody struct {
		ActiveRun        bool                `json:"active_run"`
		ActiveResponseID string              `json:"active_response_id"`
		PendingAskUser   *serveAskUserPrompt `json:"pending_ask_user"`
	}
	if err := json.Unmarshal(stateRR.Body.Bytes(), &stateBody); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !stateBody.ActiveRun {
		t.Fatal("expected active_run=true while ask_user is pending")
	}
	if !strings.HasPrefix(stateBody.ActiveResponseID, "resp_") {
		t.Fatalf("active_response_id = %q, want resp_ prefix", stateBody.ActiveResponseID)
	}
	if stateBody.PendingAskUser == nil || stateBody.PendingAskUser.CallID != "call_ask_1" {
		t.Fatalf("unexpected pending ask_user state: %#v", stateBody.PendingAskUser)
	}

	waitForServeCondition(t, time.Second, func() bool {
		msgs, err := store.GetMessages(context.Background(), "resume-session", 0, 0)
		if err != nil || len(msgs) < 2 {
			return false
		}
		for _, msg := range msgs {
			if msg.Role != llm.RoleAssistant {
				continue
			}
			for _, part := range msg.Parts {
				if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.Name == tools.AskUserToolName {
					return true
				}
			}
		}
		return false
	}, "persisted ask_user tool call snapshot")

	cancel()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for streaming request to detach")
	}

	if !rt.hasActiveRun() {
		t.Fatal("expected runtime to remain active after client disconnect")
	}

	submitBody := `{"call_id":"call_ask_1","answers":[{"question_index":0,"header":"Theme","selected":"Dark","is_custom":false}]}`
	submitReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/resume-session/ask_user", strings.NewReader(submitBody))
	submitReq.Header.Set("Content-Type", "application/json")
	submitRR := httptest.NewRecorder()
	srv.handleSessionByID(submitRR, submitReq)
	if submitRR.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body=%s", submitRR.Code, submitRR.Body.String())
	}

	waitForServeCondition(t, time.Second, func() bool {
		if rt.hasActiveRun() {
			return false
		}
		msgs, err := store.GetMessages(context.Background(), "resume-session", 0, 0)
		if err != nil {
			return false
		}
		for _, msg := range msgs {
			if msg.Role == llm.RoleAssistant && strings.Contains(msg.TextContent, "All set.") {
				return true
			}
		}
		return false
	}, "completed resumed ask_user run")
}

func TestHandleSessionState_ConsumesDeferredUIRunError(t *testing.T) {
	rt := &serveRuntime{}
	rt.setLastUIRunError("resume failed")
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions["sess-state"] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr}

	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-state/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first state status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "resume failed") {
		t.Fatalf("expected first state response to include deferred error, got %s", rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	srv.handleSessionByID(rr2, req)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second state status = %d", rr2.Code)
	}
	if strings.Contains(rr2.Body.String(), "resume failed") {
		t.Fatalf("expected deferred error to be consumed, got %s", rr2.Body.String())
	}
}

func TestHandleSessionState_ReturnsModelAndEffortFromRuntime(t *testing.T) {
	rt := &serveRuntime{
		providerKey:  "openai",
		defaultModel: "gpt-5.6-sol",
	}
	rt.mu.Lock()
	rt.sessionMeta = &session.Session{
		Model:           "gpt-5.6-terra",
		ReasoningEffort: "high",
		ReasoningMode:   "pro",
	}
	rt.mu.Unlock()

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions["sess-model"] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-model/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"model":"gpt-5.6-terra"`) {
		t.Errorf("expected model from sessionMeta in state, got %s", body)
	}
	if !strings.Contains(body, `"reasoning_effort":"high"`) {
		t.Errorf("expected reasoning_effort from sessionMeta in state, got %s", body)
	}
	if !strings.Contains(body, `"reasoning_mode":"pro"`) {
		t.Errorf("expected reasoning_mode from sessionMeta in state, got %s", body)
	}
	if !strings.Contains(body, `"provider":"openai"`) {
		t.Errorf("expected provider in state, got %s", body)
	}
}

func TestHandleSessionState_ExposesPendingSteering(t *testing.T) {
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	engine.Steer("hi there")

	rt := &serveRuntime{engine: engine}
	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions["sess-steer"] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-steer/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"pending_steering"`) {
		t.Fatalf("expected pending_steering in body, got %s", body)
	}
	if !strings.Contains(body, `"text":"hi there"`) {
		t.Fatalf("expected pending_steering text, got %s", body)
	}

	// Peek must be non-destructive: a subsequent call should still see it.
	rr2 := httptest.NewRecorder()
	srv.handleSessionByID(rr2, req)
	if !strings.Contains(rr2.Body.String(), `"text":"hi there"`) {
		t.Fatalf("expected pending_steering to survive repeated peeks, got %s", rr2.Body.String())
	}

	// DrainSteeringText should still return the value.
	if got := engine.DrainSteeringText(); got != "hi there" {
		t.Fatalf("drain after peeks = %q, want %q", got, "hi there")
	}
}

func TestHandleSessionState_FallsBackToDBWhenRuntimeNotLoaded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	sess := &session.Session{
		ID:              "sess-db-fallback",
		ProviderKey:     "openai",
		Model:           "gpt-5",
		ReasoningEffort: "medium",
		Mode:            session.ModeChat,
		Origin:          session.OriginWeb,
		Goal:            session.NewGoal("persisted objective", 500, time.Now()),
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return &serveRuntime{}, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr, store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-db-fallback/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"model":"gpt-5"`) {
		t.Errorf("expected model from DB fallback in state, got %s", body)
	}
	if !strings.Contains(body, `"reasoning_effort":"medium"`) {
		t.Errorf("expected reasoning_effort from DB fallback in state, got %s", body)
	}
	if !strings.Contains(body, `"provider":"openai"`) {
		t.Errorf("expected provider from DB fallback in state, got %s", body)
	}
	if !strings.Contains(body, `"goal"`) || !strings.Contains(body, `"objective":"persisted objective"`) {
		t.Errorf("expected goal from DB fallback in state, got %s", body)
	}
}

func TestHandleSessionState_NormalizesSuffixedModelWithExplicitEffort(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	sess := &session.Session{
		ID:              "sess-normalize-effort",
		ProviderKey:     "chatgpt",
		Model:           "gpt-5.5-medium",
		ReasoningEffort: "xhigh",
		Mode:            session.ModeChat,
		Origin:          session.OriginWeb,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return &serveRuntime{}, nil
	})
	defer mgr.Close()

	srv := &serveServer{sessionMgr: mgr, store: store}
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions/sess-normalize-effort/state", nil)
	rr := httptest.NewRecorder()
	srv.handleSessionByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"model":"gpt-5.5"`) {
		t.Errorf("expected canonical model in state, got %s", body)
	}
	if !strings.Contains(body, `"reasoning_effort":"xhigh"`) {
		t.Errorf("expected explicit reasoning_effort to win, got %s", body)
	}
	if strings.Contains(body, "gpt-5.5-medium") {
		t.Errorf("state leaked suffixed model despite separate effort: %s", body)
	}
}

func TestHandleSessionState_DoesNotBlockWhileRunHoldsRuntimeMutex(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()

	sess := &session.Session{
		ID:              "sess-busy",
		ProviderKey:     "openai",
		Model:           "gpt-5",
		ReasoningEffort: "medium",
		Mode:            session.ModeChat,
		Origin:          session.OriginWeb,
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	rt := &serveRuntime{
		providerKey:  "openai",
		defaultModel: "gpt-5",
		store:        store,
		sessionMeta:  sess,
	}

	mgr := newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return rt, nil
	})
	defer mgr.Close()
	mgr.mu.Lock()
	mgr.sessions[sess.ID] = rt
	mgr.mu.Unlock()

	srv := &serveServer{sessionMgr: mgr, store: store}

	// Simulate an active run by holding rt.mu while the handler runs. Release
	// before mgr.Close() so the runtime can finalize without deadlocking.
	rt.mu.Lock()
	lockReleased := false
	releaseLock := func() {
		if !lockReleased {
			lockReleased = true
			rt.mu.Unlock()
		}
	}
	defer releaseLock()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/state", nil)
		rr := httptest.NewRecorder()
		srv.handleSessionByID(rr, req)
		done <- rr
	}()

	select {
	case rr := <-done:
		releaseLock()
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d", rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, `"model":"gpt-5"`) {
			t.Errorf("expected model from DB fallback while rt.mu held, got %s", body)
		}
		if !strings.Contains(body, `"reasoning_effort":"medium"`) {
			t.Errorf("expected reasoning_effort from DB fallback while rt.mu held, got %s", body)
		}
		if !strings.Contains(body, `"provider":"openai"`) {
			t.Errorf("expected provider while rt.mu held, got %s", body)
		}
	case <-time.After(2 * time.Second):
		releaseLock()
		t.Fatal("handleSessionState blocked while rt.mu was held by a run")
	}
}

func TestHandleResponses_ReturnsStableResponseID(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	code, resp := doResponses(t, srv, `{"input":"hi"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	id, _ := resp["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Fatalf("response id = %q, want resp_ prefix", id)
	}
}

func TestHandleResponses_NonStreamingResponseIDCanBeFetchedAndReplayed(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	code, resp := doResponses(t, srv, `{"input":"hi"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	responseID, _ := resp["id"].(string)
	if responseID == "" {
		t.Fatal("response missing id")
	}

	statusResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID)
	if err != nil {
		t.Fatalf("get response by id failed: %v", err)
	}
	statusBody, _ := io.ReadAll(statusResp.Body)
	statusResp.Body.Close()
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200 (body=%s)", statusResp.StatusCode, string(statusBody))
	}
	var snapshot map[string]any
	if err := json.Unmarshal(statusBody, &snapshot); err != nil {
		t.Fatalf("unmarshal response snapshot: %v", err)
	}
	if got, _ := snapshot["id"].(string); got != responseID {
		t.Fatalf("snapshot id = %q, want %q", got, responseID)
	}
	if got, _ := snapshot["status"].(string); got != "completed" {
		t.Fatalf("snapshot status = %q, want completed", got)
	}

	eventsResp, err := ts.Client().Get(ts.URL + "/v1/responses/" + responseID + "/events")
	if err != nil {
		t.Fatalf("get response events failed: %v", err)
	}
	defer eventsResp.Body.Close()
	if eventsResp.StatusCode != http.StatusOK {
		eventsBody, _ := io.ReadAll(eventsResp.Body)
		t.Fatalf("events status code = %d, want 200 (body=%s)", eventsResp.StatusCode, string(eventsBody))
	}

	scanner := bufio.NewScanner(eventsResp.Body)
	sawCreated := false
	sawDelta := false
	sawCompleted := false
	for {
		eventName, data, ok := readSSEEvent(t, scanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}

		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal SSE payload: %v", err)
		}
		switch eventName {
		case "response.created":
			sawCreated = true
			response, _ := payload["response"].(map[string]any)
			if got, _ := response["id"].(string); got != responseID {
				t.Fatalf("created event id = %q, want %q", got, responseID)
			}
		case "response.output_text.delta":
			sawDelta = fmt.Sprint(payload["delta"]) == "hello"
		case "response.completed":
			sawCompleted = true
		}
	}
	if !sawCreated {
		t.Fatal("response.created event not found")
	}
	if !sawDelta {
		t.Fatal("response.output_text.delta event not found")
	}
	if !sawCompleted {
		t.Fatal("response.completed event not found")
	}
}

func TestHandleResponses_IncludesSessionUsage(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	code, resp := doResponses(t, srv, `{"input":"hi"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	sessionUsage, ok := resp["session_usage"].(map[string]any)
	if !ok {
		t.Fatalf("session_usage missing from response")
	}
	// Session usage should have the same structure as usage
	if _, ok := sessionUsage["input_tokens"]; !ok {
		t.Fatalf("session_usage missing input_tokens")
	}
	if _, ok := sessionUsage["output_tokens"]; !ok {
		t.Fatalf("session_usage missing output_tokens")
	}
	contextUsage, ok := resp["context_usage"].(map[string]any)
	if !ok {
		t.Fatalf("context_usage missing from response")
	}
	if used, _ := contextUsage["used_tokens"].(float64); used <= 0 {
		t.Fatalf("context_usage used_tokens = %v, want positive estimate", contextUsage["used_tokens"])
	}
	if estimated, _ := contextUsage["estimated"].(bool); !estimated {
		t.Fatalf("context_usage estimated = %v, want true", contextUsage["estimated"])
	}
}

func TestHandleResponses_UnknownPreviousResponseIDReturnsError(t *testing.T) {
	srv := newTestServeServer("hello")
	defer srv.sessionMgr.Close()

	body := `{"input":"hi","previous_response_id":"resp_does_not_exist"}`
	code, _ := doResponses(t, srv, body)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for unknown previous_response_id", code)
	}
}

func TestHandleResponses_UnknownPreviousResponseIDDoesNotOverwriteHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock").AddTextResponse("ok")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr, store: store}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()

	// Establish a session with history
	code, resp := doResponsesWithHeader(t, srv, `{"input":"first message"}`, "protect-me")
	if code != http.StatusOK {
		t.Fatalf("setup status = %d, want 200", code)
	}
	respID, _ := resp["id"].(string)
	_ = respID

	// Verify messages were persisted
	msgs, err := store.GetMessages(context.Background(), "protect-me", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	originalCount := len(msgs)
	if originalCount < 2 {
		t.Fatalf("expected at least 2 persisted messages, got %d", originalCount)
	}

	// Now send a request with an unknown previous_response_id and same session_id.
	// This MUST fail and NOT overwrite history.
	code2, _ := doResponsesWithHeader(t, srv,
		`{"input":"bad","previous_response_id":"resp_bogus"}`, "protect-me")
	if code2 != http.StatusBadRequest {
		t.Fatalf("unknown previous_response_id status = %d, want 400", code2)
	}

	// Verify persisted history is unchanged
	msgs2, err := store.GetMessages(context.Background(), "protect-me", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after bad request: %v", err)
	}
	if len(msgs2) != originalCount {
		t.Fatalf("persisted messages changed: was %d, now %d", originalCount, len(msgs2))
	}
}

func TestHandleResponses_StalePreviousResponseIDReturnsConflict(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2", "reply3")
	defer srv.sessionMgr.Close()

	// Send two messages to create two response IDs
	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "stale-test")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)

	body2 := `{"input":"msg2","previous_response_id":"` + respID1 + `"}`
	code, resp2 := doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	_ = resp2["id"].(string) // respID2 is now the latest

	// Try to chain from the OLD response ID (respID1) — should fail
	body3 := `{"input":"msg3","previous_response_id":"` + respID1 + `"}`
	code, _ = doResponses(t, srv, body3)
	if code != http.StatusConflict {
		t.Fatalf("stale previous_response_id status = %d, want 409", code)
	}
}

func TestHandleResponses_StalePreviousResponseIDReturnsConflictAfterRuntimeRecreation(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2", "reply3")
	defer srv.sessionMgr.Close()

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "stale-recreate")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	code, resp2 := doResponses(t, srv, `{"input":"msg2","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("second response missing id")
	}

	// Simulate runtime recreation without cleaning the server-wide response map.
	srv.sessionMgr.mu.Lock()
	evicted := srv.sessionMgr.sessions["stale-recreate"]
	delete(srv.sessionMgr.sessions, "stale-recreate")
	srv.sessionMgr.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, _ = doResponses(t, srv, `{"input":"msg3","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusConflict {
		t.Fatalf("stale previous_response_id after recreation status = %d, want 409", code)
	}
}

func TestHandleResponses_FreshConversationResetRemovesStalePreviousResponseIDs(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "fresh-reset")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	// Simulate a recreated runtime while the old response ID mapping survives.
	srv.sessionMgr.mu.Lock()
	evicted := srv.sessionMgr.sessions["fresh-reset"]
	delete(srv.sessionMgr.sessions, "fresh-reset")
	srv.sessionMgr.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}
	if _, ok := srv.responseToSession.Load(respID1); !ok {
		t.Fatalf("responseToSession should still contain %q before reset", respID1)
	}

	code, resp2 := doResponsesWithHeader(t, srv, `{"input":"reset"}`, "fresh-reset")
	if code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("reset response missing id")
	}

	if _, ok := srv.responseToSession.Load(respID1); ok {
		t.Fatalf("stale previous_response_id %q should be removed after fresh reset", respID1)
	}
	latest, ok := srv.sessionToResponse.Load("fresh-reset")
	if !ok {
		t.Fatal("sessionToResponse missing fresh-reset after reset")
	}
	latestID, _ := latest.(string)
	if latestID != respID2 {
		t.Fatalf("sessionToResponse = %q, want %q", latestID, respID2)
	}
}

func TestHandleResponses_FreshConversationResetPreservesPreviousResponseIDsWhenRunFails(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "fresh-reset-busy")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	rt, ok := srv.sessionMgr.Get("fresh-reset-busy")
	if !ok || rt == nil {
		t.Fatal("expected runtime for fresh-reset-busy")
	}

	busyState := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	rt.mu.Lock()
	rt.setActiveInterrupt(busyState)
	defer func() {
		rt.clearActiveInterrupt(busyState)
		rt.mu.Unlock()
	}()

	code, _ = doResponsesWithHeader(t, srv, `{"input":"reset"}`, "fresh-reset-busy")
	if code != http.StatusConflict {
		t.Fatalf("reset status = %d, want 409", code)
	}

	mapped, ok := srv.responseToSession.Load(respID1)
	if !ok {
		t.Fatalf("responseToSession missing %q after failed fresh reset", respID1)
	}
	mappedSessionID, _ := mapped.(string)
	if mappedSessionID != "fresh-reset-busy" {
		t.Fatalf("responseToSession[%q] = %q, want %q", respID1, mappedSessionID, "fresh-reset-busy")
	}
	latest, ok := srv.sessionToResponse.Load("fresh-reset-busy")
	if !ok {
		t.Fatal("sessionToResponse missing fresh-reset-busy after failed reset")
	}
	latestID, _ := latest.(string)
	if latestID != respID1 {
		t.Fatalf("sessionToResponse = %q, want %q", latestID, respID1)
	}
}

func TestHandleResponses_FreshConversationBusySessionDoesNotOverwritePersistedRuntimeMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	factory := func(ctx context.Context) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		provider.AddTextResponse("reply1")
		provider.AddTextResponse("reply2")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			store:        store,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}
	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{sessionMgr: mgr, store: store}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()

	code, resp := doResponsesWithHeader(t, srv, `{"input":"msg1","model":"original-model"}`, "fresh-reset-metadata")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID, _ := resp["id"].(string)
	if respID == "" {
		t.Fatal("first response missing id")
	}

	sess, err := store.Get(context.Background(), "fresh-reset-metadata")
	if err != nil {
		t.Fatalf("Get before busy reset: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session before busy reset")
	}
	if got := strings.TrimSpace(sess.Model); got != "original-model" {
		t.Fatalf("persisted model before busy reset = %q, want %q", got, "original-model")
	}

	rt, ok := srv.sessionMgr.Get("fresh-reset-metadata")
	if !ok || rt == nil {
		t.Fatal("expected runtime for fresh-reset-metadata")
	}
	busyState := &runtimeInterruptState{cancel: func() {}, done: make(chan struct{})}
	rt.setActiveInterrupt(busyState)
	defer rt.clearActiveInterrupt(busyState)

	code, _ = doResponsesWithHeader(t, srv, `{"input":"reset","model":"replacement-model"}`, "fresh-reset-metadata")
	if code != http.StatusConflict {
		t.Fatalf("reset status = %d, want 409", code)
	}

	sess, err = store.Get(context.Background(), "fresh-reset-metadata")
	if err != nil {
		t.Fatalf("Get after busy reset: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session after busy reset")
	}
	if got := strings.TrimSpace(sess.Model); got != "original-model" {
		t.Fatalf("persisted model after busy reset = %q, want %q", got, "original-model")
	}
	mapped, ok := srv.responseToSession.Load(respID)
	if !ok {
		t.Fatalf("responseToSession missing %q after busy reset", respID)
	}
	mappedSessionID, _ := mapped.(string)
	if mappedSessionID != "fresh-reset-metadata" {
		t.Fatalf("responseToSession[%q] = %q, want %q", respID, mappedSessionID, "fresh-reset-metadata")
	}
}

func TestHandleResponses_PreviousResponseIDChainsSession(t *testing.T) {
	// Each runtime gets 2 text responses so it can handle 2 messages
	srv := newTestServeServer("first reply", "second reply")
	defer srv.sessionMgr.Close()

	// First request: no chaining
	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "sess-chain")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatalf("first response missing id")
	}

	// Second request: chain via previous_response_id
	body2 := `{"input":"msg2","previous_response_id":"` + respID1 + `"}`
	code, resp2 := doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatalf("second response missing id")
	}
	if respID1 == respID2 {
		t.Fatalf("response IDs should be unique, both are %q", respID1)
	}
}

func TestHandleResponses_PreviousResponseIDFunctionCallOutputUsesPriorToolName(t *testing.T) {
	provider := llm.NewMockProvider("mock").
		AddToolCall("call_1", "read_file", map[string]any{"path": "a.txt"}).
		AddTextResponse("done")

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{sessionMgr: manager}
	manager.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hi","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`, "tool-chain")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	body2 := `{"input":[{"type":"function_call_output","call_id":"call_1","output":"content"}],"previous_response_id":"` + respID1 + `"}`
	code, _ = doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.Requests))
	}

	var sawUserHi bool
	var sawToolCall bool
	var toolResultName string
	for _, msg := range provider.Requests[1].Messages {
		if msg.Role == llm.RoleUser && len(msg.Parts) > 0 && msg.Parts[0].Type == llm.PartText && msg.Parts[0].Text == "hi" {
			sawUserHi = true
		}
		for _, part := range msg.Parts {
			if part.Type == llm.PartToolCall && part.ToolCall != nil && part.ToolCall.ID == "call_1" && part.ToolCall.Name == "read_file" {
				sawToolCall = true
			}
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.ID != "call_1" {
				continue
			}
			toolResultName = part.ToolResult.Name
		}
	}
	if !sawUserHi {
		t.Fatal("second provider request missing prior user message")
	}
	if !sawToolCall {
		t.Fatal("second provider request missing prior assistant tool call")
	}
	if toolResultName != "read_file" {
		t.Fatalf("tool result name = %q, want %q", toolResultName, "read_file")
	}
}

func TestHandleResponses_PreviousResponseIDFunctionCallOutputUsesPersistedToolNameAfterRuntimeRecreation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var created atomic.Int32
	providers := map[int32]*llm.MockProvider{}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := created.Add(1)
		provider := llm.NewMockProvider("mock")
		if createNum == 1 {
			provider.AddToolCall("call_1", "read_file", map[string]any{"path": "a.txt"})
		} else {
			provider.AddTextResponse("done")
		}
		providers[createNum] = provider
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  "mock",
			engine:       engine,
			defaultModel: "mock-model",
			store:        store,
		}
		rt.Touch()
		return rt, nil
	})
	defer manager.Close()

	srv := &serveServer{sessionMgr: manager, store: store}
	manager.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hi","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`, "tool-chain-store")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	manager.mu.Lock()
	evicted := manager.sessions["tool-chain-store"]
	delete(manager.sessions, "tool-chain-store")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	body2 := `{"input":[{"type":"function_call_output","call_id":"call_1","output":"content"}],"previous_response_id":"` + respID1 + `"}`
	code, _ = doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}
	provider := providers[2]
	if provider == nil {
		t.Fatal("expected recreated runtime provider")
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("recreated provider request count = %d, want 1", len(provider.Requests))
	}

	var toolResultName string
	for _, msg := range provider.Requests[0].Messages {
		for _, part := range msg.Parts {
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.ID != "call_1" {
				continue
			}
			toolResultName = part.ToolResult.Name
		}
	}
	if toolResultName != "read_file" {
		t.Fatalf("persisted tool result name = %q, want %q", toolResultName, "read_file")
	}
}

func TestHandleResponses_PreviousResponseIDRestoresPersistedProviderAfterRuntimeRecreation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var defaultCreates atomic.Int32
	var otherCreates atomic.Int32

	newRuntime := func(providerName, response string) *serveRuntime {
		provider := llm.NewMockProvider(providerName).AddTextResponse(response)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := defaultCreates.Add(1)
		return newRuntime("default", fmt.Sprintf("default response %d", createNum)), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			createNum := otherCreates.Add(1)
			return newRuntime(providerName, fmt.Sprintf("%s response %d", providerName, createNum)), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"other"}`, "provider-chain")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	if respID1 == "" {
		t.Fatal("first response missing id")
	}

	sess, err := store.Get(context.Background(), "provider-chain")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session")
	}
	if sess.ProviderKey != "other" {
		t.Fatalf("ProviderKey = %q, want other", sess.ProviderKey)
	}

	manager.mu.Lock()
	evicted := manager.sessions["provider-chain"]
	delete(manager.sessions, "provider-chain")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, resp2 := doResponses(t, srv, `{"input":"resume","previous_response_id":"`+respID1+`"}`)
	if code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", code)
	}

	output, ok := resp2["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("response output = %#v, want assistant message", resp2["output"])
	}
	msg, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("first output item = %#v, want object", output[0])
	}
	content, ok := msg["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("message content = %#v, want output_text", msg["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("first content part = %#v, want object", content[0])
	}
	if got := part["text"]; got != "other response 2" {
		t.Fatalf("response text = %v, want %q", got, "other response 2")
	}
	if got := defaultCreates.Load(); got != 0 {
		t.Fatalf("default provider factory calls = %d, want 0", got)
	}
	if got := otherCreates.Load(); got != 2 {
		t.Fatalf("other provider factory calls = %d, want 2", got)
	}
}

func TestHandleResponses_ChainedRequestWithoutModelSwapRemainsPinnedToPersistedRuntime(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providers := map[string]*llm.MockProvider{}
	creates := map[string]int{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		creates[providerName]++
		provider := llm.NewMockProvider(providerName)
		provider.AddTextResponse(providerName + "-1")
		provider.AddTextResponse(providerName + "-2")
		providers[providerName] = provider
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: providerName + "-default", store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			modelName := request.Model
			return newRuntime(providerName, modelName), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"old","model":"old-model"}`, "swap-pin")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID, _ := resp1["id"].(string)
	if respID == "" {
		t.Fatal("first response missing id")
	}
	code, resp2 := doResponses(t, srv, `{"input":"continue","previous_response_id":"`+respID+`","provider":"new","model":"new-model"}`)
	if code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp2); got != "old-2" {
		t.Fatalf("response text = %q, want old-2", got)
	}
	oldProvider := providers["old"]
	oldRequests := 0
	if oldProvider != nil {
		oldRequests = len(oldProvider.Requests)
	}
	if oldRequests != 2 {
		t.Fatalf("old provider requests = %d, want 2", oldRequests)
	}
	if got := oldProvider.Requests[1].Model; got != "old-model" {
		t.Fatalf("second request model = %q, want old-model", got)
	}
	if creates["new"] != 0 {
		t.Fatalf("new provider creates = %d, want 0 without model_swap", creates["new"])
	}
}

func TestHandleResponses_ModelSwapNaiveSuccessCommitsTargetRuntime(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providers := map[string]*llm.MockProvider{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		provider := llm.NewMockProvider(providerName)
		switch providerName {
		case "old":
			provider.AddTextResponse("old-1")
		case "new":
			provider.AddTextResponse("new-1")
			provider.AddTextResponse("new-2")
		default:
			provider.AddTextResponse(providerName + "-1")
		}
		providers[providerName] = provider
		engine := llm.NewEngine(provider, nil)
		if modelName == "" {
			modelName = providerName + "-default"
		}
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: modelName, store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfg:        serveServerConfig{agentNames: []string{"developer"}},
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			modelName := request.Model
			return newRuntime(providerName, modelName), nil
		},
		agentRuntimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			modelName := request.Model
			agentName := request.Agent
			runtime := newRuntime(providerName, modelName)
			runtime.agentName = agentName
			return runtime, nil
		},
	}

	code, resp1 := doResponsesFirstParty(t, srv, `{"input":"hello","client_message_id":"swap-initial","provider":"old","model":"old-model","agent":"developer"}`, "swap-naive")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID := resp1["id"].(string)

	code, conflict := doResponsesFirstParty(t, srv, `{"input":"hello","client_message_id":"swap-initial","previous_response_id":"`+respID+`","provider":"new","model":"new-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusConflict {
		t.Fatalf("duplicate swap status = %d, want 409 body=%#v", code, conflict)
	}
	conflictErr, _ := conflict["error"].(map[string]any)
	if conflictErr["type"] != "client_message_already_committed" {
		t.Fatalf("duplicate swap error = %#v", conflict)
	}
	currentAfterConflict, ok := manager.Get("swap-naive")
	if !ok || currentAfterConflict == nil || currentAfterConflict.providerKey != "old" {
		t.Fatalf("duplicate swap replaced previous runtime: current=%#v ok=%v", currentAfterConflict, ok)
	}

	previousRuntime, ok := manager.Get("swap-naive")
	if !ok || previousRuntime == nil {
		t.Fatal("previous runtime missing before model swap")
	}
	previousRuntime.engine.QueueSteering(llm.QueuedSteering{
		ID:          "swap-follow-up",
		Message:     llm.UserText("continue"),
		DisplayText: "continue",
	})
	code, resp2 := doResponsesFirstParty(t, srv, `{"input":"continue","client_message_id":"swap-follow-up","previous_response_id":"`+respID+`","provider":"new","model":"new-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusOK {
		t.Fatalf("swap status = %d, want 200", code)
	}
	if pending := previousRuntime.engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("model swap claimed candidate instead of previous runtime: %#v", pending)
	}
	if got := responseOutputText(t, resp2); got != "new-1" {
		t.Fatalf("response text = %q, want new-1", got)
	}
	newProvider := providers["new"]
	newRequests := 0
	if newProvider != nil {
		newRequests = len(newProvider.Requests)
	}
	if newRequests != 1 {
		t.Fatalf("new provider requests = %d, want 1", newRequests)
	}
	if got := newProvider.Requests[0].Model; got != "new-model" {
		t.Fatalf("target request model = %q, want new-model", got)
	}
	if !requestContainsText(newProvider.Requests[0], "hello") || !requestContainsText(newProvider.Requests[0], "continue") {
		t.Fatalf("target request did not receive prior context plus pending user: %#v", newProvider.Requests[0].Messages)
	}
	sess, err := store.Get(context.Background(), "swap-naive")
	if err != nil || sess == nil {
		t.Fatalf("Get session after swap: %v", err)
	}
	if sess.ProviderKey != "new" || sess.Model != "new-model" {
		t.Fatalf("session runtime = %s/%s, want new/new-model", sess.ProviderKey, sess.Model)
	}
	if sess.Agent != "developer" {
		t.Fatalf("session agent = %q after model swap, want developer", sess.Agent)
	}
	currentRuntime, ok := manager.Get("swap-naive")
	if !ok || currentRuntime == nil || currentRuntime.agentName != "developer" {
		t.Fatalf("replacement runtime lost agent tether: current=%#v ok=%v", currentRuntime, ok)
	}
	storedMessages, err := store.GetMessages(context.Background(), "swap-naive", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after swap: %v", err)
	}
	markerIndex := -1
	for i, msg := range storedMessages {
		if msg.Role == llm.RoleEvent {
			markerIndex = i
			if marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage()); !ok || marker.Status != "succeeded" || marker.Strategy != "naive" {
				t.Fatalf("unexpected model-swap marker: ok=%v marker=%#v", ok, marker)
			}
		}
	}
	if markerIndex < 0 {
		t.Fatalf("expected persisted model-swap event marker, messages=%#v", storedMessages)
	}
	if markerIndex == 0 || markerIndex+1 >= len(storedMessages) || storedMessages[markerIndex-1].Role != llm.RoleUser || storedMessages[markerIndex-1].TextContent != "continue" || storedMessages[markerIndex+1].Role != llm.RoleAssistant {
		t.Fatalf("model-swap marker must remain between the triggering user message and its reply, marker index=%d messages=%#v", markerIndex, storedMessages)
	}

	respID2 := resp2["id"].(string)
	code, resp3 := doResponses(t, srv, `{"input":"again","previous_response_id":"`+respID2+`"}`)
	if code != http.StatusOK {
		t.Fatalf("third status = %d, want 200 body=%#v", code, resp3)
	}
	if got := responseOutputText(t, resp3); got != "new-2" {
		t.Fatalf("third response text = %q, want new-2", got)
	}
	if len(newProvider.Requests) != 2 {
		t.Fatalf("new provider requests after third turn = %d, want 2", len(newProvider.Requests))
	}
	for _, msg := range newProvider.Requests[1].Messages {
		if msg.Role == llm.RoleEvent {
			t.Fatalf("provider request included event marker: %#v", newProvider.Requests[1].Messages)
		}
	}
}

func TestHandleResponses_ModelSwapNaiveFailureFallsBackToHandover(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providersByCreate := map[string][]*llm.MockProvider{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		provider := llm.NewMockProvider(providerName)
		providersByCreate[providerName] = append(providersByCreate[providerName], provider)
		switch providerName {
		case "old":
			if len(providersByCreate[providerName]) == 1 {
				provider.AddTextResponse("old-1")
			} else {
				provider.AddTurn(llm.MockTurn{Text: "handover doc", Usage: llm.Usage{InputTokens: 11, OutputTokens: 3}})
			}
		case "new":
			provider.AddError(errors.New("400 invalid_request: messages contain incompatible reasoning"))
			provider.AddTextResponse("retry-ok")
		default:
			provider.AddTextResponse(providerName + "-1")
		}
		engine := llm.NewEngine(provider, nil)
		if modelName == "" {
			modelName = providerName + "-default"
		}
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: modelName, store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			modelName := request.Model
			return newRuntime(providerName, modelName), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"old","model":"old-model"}`, "swap-handover")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID := resp1["id"].(string)
	code, resp2 := doResponses(t, srv, `{"input":"continue","previous_response_id":"`+respID+`","provider":"new","model":"new-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusOK {
		t.Fatalf("swap status = %d, want 200 body=%#v", code, resp2)
	}
	if got := responseOutputText(t, resp2); got != "retry-ok" {
		t.Fatalf("response text = %q, want retry-ok", got)
	}
	newProvider := providersByCreate["new"][0]
	if len(newProvider.Requests) != 2 {
		t.Fatalf("new provider requests = %d, want naive + retry", len(newProvider.Requests))
	}
	if !requestContainsText(newProvider.Requests[1], "handover doc") || !requestContainsText(newProvider.Requests[1], "continue") {
		t.Fatalf("fallback request missing handover doc or pending user: %#v", newProvider.Requests[1].Messages)
	}
	if len(providersByCreate["old"]) < 2 || len(providersByCreate["old"][1].Requests) != 1 {
		t.Fatalf("expected helper old provider to generate handover")
	}
	persisted, err := store.Get(context.Background(), "swap-handover")
	if err != nil || persisted == nil {
		t.Fatalf("Get session after successful handover swap: %v", err)
	}
	if persisted.InputTokens != 11 || persisted.OutputTokens != 3 {
		t.Fatalf("persisted successful handover usage = %d/%d, want 11/3 exactly once", persisted.InputTokens, persisted.OutputTokens)
	}
	current, ok := manager.Get("swap-handover")
	if !ok || current.cumulativeUsage.InputTokens != 11 || current.cumulativeUsage.OutputTokens != 3 {
		t.Fatalf("successful runtime handover usage = %+v ok=%v, want 11 input/3 output", func() llm.Usage {
			if current == nil {
				return llm.Usage{}
			}
			return current.cumulativeUsage
		}(), ok)
	}

	storedMessages, err := store.GetMessages(context.Background(), "swap-handover", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after handover swap: %v", err)
	}
	markerIndex := -1
	for i, msg := range storedMessages {
		if marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage()); ok {
			if marker.Status != "succeeded" || marker.Strategy != "handover" {
				t.Fatalf("unexpected handover model-swap marker: %#v", marker)
			}
			markerIndex = i
		}
	}
	if markerIndex < 0 || markerIndex == 0 || markerIndex+1 >= len(storedMessages) || storedMessages[markerIndex-1].Role != llm.RoleUser || storedMessages[markerIndex-1].TextContent != "continue" || storedMessages[markerIndex+1].Role != llm.RoleAssistant {
		t.Fatalf("handover model-swap marker must remain between the triggering user message and its reply, marker index=%d messages=%#v", markerIndex, storedMessages)
	}
}

func TestHandleResponses_ModelSwapFallbackFailureRollsBackOriginalRuntimeAndHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var mu sync.Mutex
	providersByCreate := map[string][]*llm.MockProvider{}
	newRuntime := func(providerName, modelName string) *serveRuntime {
		mu.Lock()
		defer mu.Unlock()
		provider := llm.NewMockProvider(providerName)
		providersByCreate[providerName] = append(providersByCreate[providerName], provider)
		switch providerName {
		case "old":
			if len(providersByCreate[providerName]) == 1 {
				provider.AddTextResponse("old-1")
				provider.AddTextResponse("old-2")
			} else {
				provider.AddTurn(llm.MockTurn{Text: "handover doc", Usage: llm.Usage{InputTokens: 11, OutputTokens: 3}})
			}
		case "new":
			provider.AddError(errors.New("400 invalid_request: messages contain incompatible reasoning"))
			provider.AddError(errors.New("400 invalid_request: retry still rejected"))
		default:
			provider.AddTextResponse(providerName + "-1")
		}
		engine := llm.NewEngine(provider, nil)
		if modelName == "" {
			modelName = providerName + "-default"
		}
		rt := &serveRuntime{provider: provider, providerKey: providerName, engine: engine, defaultModel: modelName, store: store}
		rt.Touch()
		return rt
	}
	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default", ""), nil
	})
	defer manager.Close()
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			modelName := request.Model
			return newRuntime(providerName, modelName), nil
		},
	}

	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"old","model":"old-model"}`, "swap-fail")
	if code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", code)
	}
	respID := resp1["id"].(string)
	code, resp2 := doResponses(t, srv, `{"input":"continue","previous_response_id":"`+respID+`","provider":"new","model":"new-model","model_swap":{"mode":"auto","fallback":"handover"}}`)
	if code != http.StatusBadRequest {
		t.Fatalf("swap failure status = %d, want 400 body=%#v", code, resp2)
	}
	current, ok := manager.Get("swap-fail")
	if !ok || current.providerKey != "old" {
		t.Fatalf("session manager current provider = %v ok=%v, want old", func() string {
			if current == nil {
				return ""
			}
			return current.providerKey
		}(), ok)
	}
	sess, err := store.Get(context.Background(), "swap-fail")
	if err != nil || sess == nil {
		t.Fatalf("Get session after failed swap: %v", err)
	}
	if sess.ProviderKey != "old" || sess.Model != "old-model" {
		t.Fatalf("session runtime after rollback = %s/%s, want old/old-model", sess.ProviderKey, sess.Model)
	}
	if sess.InputTokens != 11 || sess.OutputTokens != 3 {
		t.Fatalf("session handover usage after rollback = %d/%d, want 11/3", sess.InputTokens, sess.OutputTokens)
	}
	if current.cumulativeUsage.InputTokens != 11 || current.cumulativeUsage.OutputTokens != 3 {
		t.Fatalf("runtime handover usage after rollback = %+v, want 11 input/3 output", current.cumulativeUsage)
	}
	storedMessages, err := store.GetMessages(context.Background(), "swap-fail", 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after failed swap: %v", err)
	}
	if !sessionMessagesContainText(storedMessages, "hello") || !sessionMessagesContainText(storedMessages, "old-1") {
		t.Fatalf("rollback did not restore original conversation messages: %#v", storedMessages)
	}
	if sessionMessagesContainText(storedMessages, "handover doc") {
		t.Fatalf("rollback history should not persist handover retry context: %#v", storedMessages)
	}
	foundFailedMarker := false
	for _, msg := range storedMessages {
		if msg.Role == llm.RoleEvent {
			marker, ok := llm.ParseModelSwapMarker(msg.ToLLMMessage())
			if ok && marker.Status == "failed" {
				foundFailedMarker = true
			}
		}
	}
	if !foundFailedMarker {
		t.Fatalf("expected failed model-swap marker after rollback: %#v", storedMessages)
	}

	code, resp3 := doResponses(t, srv, `{"input":"still old","previous_response_id":"`+respID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("post-rollback status = %d, want 200 body=%#v", code, resp3)
	}
	if got := responseOutputText(t, resp3); got != "old-2" {
		t.Fatalf("post-rollback response text = %q, want old-2", got)
	}
}

func sessionMessagesContainText(messages []session.Message, needle string) bool {
	for _, msg := range messages {
		if strings.Contains(msg.TextContent, needle) {
			return true
		}
		for _, part := range msg.Parts {
			if strings.Contains(part.Text, needle) {
				return true
			}
		}
	}
	return false
}

func requestContainsText(req llm.Request, needle string) bool {
	for _, msg := range req.Messages {
		for _, part := range msg.Parts {
			if strings.Contains(part.Text, needle) {
				return true
			}
		}
	}
	return false
}

// A fresh conversation may reuse an existing session ID and switch to a new
// provider. The replacement runtime must also update the persisted session row
// so later chained requests resume on the new provider instead of jumping back
// to stale DB metadata.
func TestHandleResponses_FreshConversationReusedSessionIDUpdatesPersistedProvider(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var defaultCreates atomic.Int32
	var otherCreates atomic.Int32

	newRuntime := func(providerName, response string) *serveRuntime {
		provider := llm.NewMockProvider(providerName).AddTextResponse(response)
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := defaultCreates.Add(1)
		return newRuntime("default", fmt.Sprintf("default response %d", createNum)), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			createNum := otherCreates.Add(1)
			return newRuntime(providerName, fmt.Sprintf("%s response %d", providerName, createNum)), nil
		},
	}

	code, _ := doResponsesWithHeader(t, srv, `{"input":"hello"}`, "provider-reuse")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}

	code, resp2 := doResponsesWithHeader(t, srv, `{"input":"fresh","provider":"other"}`, "provider-reuse")
	if code != http.StatusOK {
		t.Fatalf("fresh provider request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp2); got != "other response 1" {
		t.Fatalf("response text = %q, want %q", got, "other response 1")
	}
	respID2, _ := resp2["id"].(string)
	if respID2 == "" {
		t.Fatal("fresh provider response missing id")
	}

	sess, err := store.Get(context.Background(), "provider-reuse")
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session")
	}
	if sess.ProviderKey != "other" {
		t.Fatalf("ProviderKey = %q, want other", sess.ProviderKey)
	}

	manager.mu.Lock()
	evicted := manager.sessions["provider-reuse"]
	delete(manager.sessions, "provider-reuse")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, resp3 := doResponses(t, srv, `{"input":"resume","previous_response_id":"`+respID2+`"}`)
	if code != http.StatusOK {
		t.Fatalf("resume request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp3); got != "other response 2" {
		t.Fatalf("resume response text = %q, want %q", got, "other response 2")
	}
	if got := defaultCreates.Load(); got != 1 {
		t.Fatalf("default provider factory calls = %d, want 1", got)
	}
	if got := otherCreates.Load(); got != 2 {
		t.Fatalf("other provider factory calls = %d, want 2", got)
	}
}

// When a fresh conversation reuses an old session ID but omits provider, it
// should fall back to the server default provider rather than inheriting the
// persisted provider from the prior conversation.
func TestHandleResponses_FreshConversationWithoutProviderUsesDefaultNotPersistedProvider(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	var defaultCreates atomic.Int32
	var otherCreates atomic.Int32

	newRuntime := func(providerName string, createNum int32) *serveRuntime {
		provider := llm.NewMockProvider(providerName)
		provider.AddTextResponse(fmt.Sprintf("%s runtime %d response 1", providerName, createNum))
		provider.AddTextResponse(fmt.Sprintf("%s runtime %d response 2", providerName, createNum))
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		createNum := defaultCreates.Add(1)
		return newRuntime("default", createNum), nil
	})
	defer manager.Close()

	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		store:      store,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			if providerName == "" || providerName == "default" {
				createNum := defaultCreates.Add(1)
				return newRuntime("default", createNum), nil
			}
			createNum := otherCreates.Add(1)
			return newRuntime(providerName, createNum), nil
		},
	}

	code, resp := doResponsesWithHeader(t, srv, `{"input":"hello","provider":"other"}`, "provider-default-reuse")
	if code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp); got != "other runtime 1 response 1" {
		t.Fatalf("first response text = %q, want %q", got, "other runtime 1 response 1")
	}

	code, resp = doResponsesWithHeader(t, srv, `{"input":"fresh"}`, "provider-default-reuse")
	if code != http.StatusOK {
		t.Fatalf("fresh default request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp); got != "default runtime 1 response 1" {
		t.Fatalf("fresh default response text = %q, want %q", got, "default runtime 1 response 1")
	}
	respID, _ := resp["id"].(string)
	if respID == "" {
		t.Fatal("fresh default response missing id")
	}

	sess, err := store.Get(context.Background(), "provider-default-reuse")
	if err != nil {
		t.Fatalf("Get session after implicit default: %v", err)
	}
	if sess == nil {
		t.Fatal("expected persisted session after implicit default request")
	}
	if sess.ProviderKey != "default" {
		t.Fatalf("ProviderKey after implicit default = %q, want default", sess.ProviderKey)
	}

	manager.mu.Lock()
	evicted := manager.sessions["provider-default-reuse"]
	delete(manager.sessions, "provider-default-reuse")
	manager.mu.Unlock()
	if evicted != nil {
		evicted.Close()
	}

	code, resp = doResponses(t, srv, `{"input":"resume","previous_response_id":"`+respID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("resume request status = %d, want 200", code)
	}
	if got := responseOutputText(t, resp); got != "default runtime 2 response 1" {
		t.Fatalf("resume response text = %q, want %q", got, "default runtime 2 response 1")
	}
	if got := defaultCreates.Load(); got != 2 {
		t.Fatalf("default provider factory calls = %d, want 2", got)
	}
	if got := otherCreates.Load(); got != 1 {
		t.Fatalf("other provider factory calls = %d, want 1", got)
	}
}

func TestFreshProviderRequest_ConcurrentReplace(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	newRuntime := func(providerName string) *serveRuntime {
		provider := llm.NewMockProvider(providerName).AddTextResponse(providerName + " reply")
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			providerKey:  providerName,
			engine:       engine,
			defaultModel: providerName + "-model",
			store:        store,
		}
		rt.Touch()
		return rt
	}

	manager := newServeSessionManager(time.Minute, 100, func(ctx context.Context) (*serveRuntime, error) {
		return newRuntime("default"), nil
	})
	defer manager.Close()
	manager.onEvict = func(rt *serveRuntime) {}

	// Leave store nil so this test isolates ReplaceIdleWith provider races;
	// persisted-session base-dir/MCP restoration has separate coverage and may
	// legitimately return busy when many callers share the replacement runtime.
	srv := &serveServer{
		cfgRef:     &config.Config{DefaultProvider: "default"},
		sessionMgr: manager,
		runtimeFactory: func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
			providerName := request.Provider
			return newRuntime(providerName), nil
		},
	}

	// Seed a session with the "default" provider.
	_, err = manager.GetOrCreate(context.Background(), "race-session")
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Launch concurrent replacement requests with different providers.
	const goroutines = 10
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	providers := make([]string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			provName := fmt.Sprintf("provider-%d", idx%2)
			rt, _, rerr := srv.runtimeForFreshProviderRequest(context.Background(), "race-session", provName)
			errs[idx] = rerr
			if rt != nil {
				providers[idx] = runtimeProviderKey(rt)
			}
		}(i)
	}
	wg.Wait()

	// Verify: all goroutines either succeeded, got errServeSessionBusy,
	// or got the belt-and-suspenders provider mismatch error (expected when
	// a concurrent goroutine wins the in-flight race with a different provider).
	anySuccess := false
	for i, err := range errs {
		if err != nil {
			if errors.Is(err, errServeSessionBusy) {
				continue
			}
			if strings.Contains(err.Error(), "already uses provider") {
				continue
			}
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
		anySuccess = true
	}
	if !anySuccess {
		t.Fatalf("all goroutines failed; expected at least one success (errors=%v providers=%v)", errs, providers)
	}

	// The session should have a consistent provider after the race.
	// We can't predict which provider won (sequential replacements are valid),
	// but it must be one of the two requested providers.
	rt, ok := manager.Get("race-session")
	if !ok {
		t.Fatal("session missing after concurrent replace")
	}
	if got := runtimeProviderKey(rt); got != "provider-0" && got != "provider-1" {
		t.Fatalf("session provider = %q, want provider-0 or provider-1", got)
	}
}

func TestHandleResponses_NoPreviousResponseIDStartsFresh(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	// First request with session_id header, no previous_response_id
	code1, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "same-session")
	if code1 != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code1)
	}
	respID1, _ := resp1["id"].(string)
	if got := responseOutputText(t, resp1); got != "reply1" {
		t.Fatalf("first response text = %q, want %q", got, "reply1")
	}

	// Second request with same session_id header but no previous_response_id.
	// Should start a fresh conversation with a fresh runtime.
	code2, resp2 := doResponsesWithHeader(t, srv, `{"input":"msg2"}`, "same-session")
	if code2 != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200; without previous_response_id should start fresh", code2)
	}

	// Both should succeed and have different response IDs.
	respID2, _ := resp2["id"].(string)
	if respID1 == respID2 {
		t.Fatalf("response IDs should differ, both are %q", respID1)
	}
	if got := responseOutputText(t, resp2); got != "reply1" {
		t.Fatalf("second response text = %q, want %q from a fresh runtime", got, "reply1")
	}

	// Verify that the current runtime only saw the fresh request.
	rt, err := srv.sessionMgr.GetOrCreate(context.Background(), "same-session")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	provider := rt.provider.(*llm.MockProvider)
	if len(provider.Requests) != 1 {
		t.Fatalf("expected 1 provider request on the fresh runtime, got %d", len(provider.Requests))
	}
	freshReq := provider.Requests[0]
	userMsgCount := 0
	for _, msg := range freshReq.Messages {
		if msg.Role == llm.RoleUser {
			userMsgCount++
		}
	}
	if userMsgCount != 1 {
		t.Fatalf("fresh request has %d user messages, want 1", userMsgCount)
	}
}

func TestHandleResponses_CumulativeUsageGrows(t *testing.T) {
	srv := newTestServeServer("reply1", "reply2")
	defer srv.sessionMgr.Close()

	// First request
	code, resp1 := doResponsesWithHeader(t, srv, `{"input":"msg1"}`, "usage-test")
	if code != http.StatusOK {
		t.Fatalf("msg1 status = %d, want 200", code)
	}
	respID1, _ := resp1["id"].(string)
	su1, _ := resp1["session_usage"].(map[string]any)
	total1 := su1["total_tokens"].(float64)

	// Second request chained
	body2 := `{"input":"msg2","previous_response_id":"` + respID1 + `"}`
	code, resp2 := doResponses(t, srv, body2)
	if code != http.StatusOK {
		t.Fatalf("msg2 status = %d, want 200", code)
	}
	su2, _ := resp2["session_usage"].(map[string]any)
	total2 := su2["total_tokens"].(float64)

	if total2 < total1 {
		t.Fatalf("cumulative session_usage should grow: first=%v, second=%v", total1, total2)
	}
}

func TestStreamResponses_IncludesResponseIDAndSessionUsage(t *testing.T) {
	srv := newTestServeServer("streamed response")
	defer srv.sessionMgr.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.handleResponses(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	// Parse SSE events to find response.completed
	var completedData map[string]any
	scanner := bufio.NewScanner(rr.Body)
	var currentEvent string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if currentEvent == "response.completed" && data != "[DONE]" {
				if err := json.Unmarshal([]byte(data), &completedData); err != nil {
					t.Fatalf("parse response.completed data: %v", err)
				}
			}
		}
	}

	if completedData == nil {
		t.Fatalf("response.completed event not found in SSE stream")
	}

	response, ok := completedData["response"].(map[string]any)
	if !ok {
		t.Fatalf("response.completed missing response object")
	}

	respID, _ := response["id"].(string)
	if !strings.HasPrefix(respID, "resp_") {
		t.Fatalf("streaming response id = %q, want resp_ prefix", respID)
	}

	sessionUsage, ok := response["session_usage"].(map[string]any)
	if !ok {
		t.Fatalf("streaming response missing session_usage")
	}
	if _, ok := sessionUsage["input_tokens"]; !ok {
		t.Fatalf("session_usage missing input_tokens")
	}
	contextUsage, ok := response["context_usage"].(map[string]any)
	if !ok {
		t.Fatalf("streaming response missing context_usage")
	}
	if used, _ := contextUsage["used_tokens"].(float64); used <= 0 {
		t.Fatalf("context_usage used_tokens = %v, want positive estimate", contextUsage["used_tokens"])
	}
}

func TestStreamResponses_FailedRunDoesNotBecomeLatestResponseID(t *testing.T) {
	provider := newStagedProvider("hello ", "world")

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 100, factory)
	srv := &serveServer{
		sessionMgr:   mgr,
		responseRuns: newServeResponseRunManager(),
	}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}
	defer mgr.Close()
	defer srv.responseRuns.Close()

	ts := newServeHTTPTestServer(srv)
	defer ts.Close()

	firstReq, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"first","stream":true}`))
	if err != nil {
		t.Fatalf("new first request: %v", err)
	}
	firstReq.Header.Set("Content-Type", "application/json")
	firstReq.Header.Set("session_id", "busy-session")

	firstResp, err := ts.Client().Do(firstReq)
	if err != nil {
		t.Fatalf("first stream request failed: %v", err)
	}
	defer firstResp.Body.Close()

	firstScanner := bufio.NewScanner(firstResp.Body)
	var firstRespID string
	for {
		eventName, data, ok := readSSEEvent(t, firstScanner)
		if !ok {
			t.Fatal("first stream ended before first text delta")
		}
		if data == "[DONE]" {
			t.Fatal("first stream completed before first text delta")
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal first SSE payload: %v", err)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			firstRespID, _ = response["id"].(string)
		case "response.output_text.delta":
			goto firstStreamBusy
		}
	}

firstStreamBusy:
	if firstRespID == "" {
		t.Fatal("missing first response id")
	}

	secondReq, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(`{"input":"second","stream":true}`))
	if err != nil {
		t.Fatalf("new second request: %v", err)
	}
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("session_id", "busy-session")

	secondResp, err := ts.Client().Do(secondReq)
	if err != nil {
		t.Fatalf("second stream request failed: %v", err)
	}
	defer secondResp.Body.Close()

	secondScanner := bufio.NewScanner(secondResp.Body)
	var secondRespID string
	sawFailed := false
	sawDone := false
	for {
		eventName, data, ok := readSSEEvent(t, secondScanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal second SSE payload: %v", err)
		}
		switch eventName {
		case "response.created":
			response, _ := payload["response"].(map[string]any)
			secondRespID, _ = response["id"].(string)
		case "response.failed":
			sawFailed = true
			errPayload, _ := payload["error"].(map[string]any)
			if got := errPayload["type"]; got != "conflict_error" {
				t.Fatalf("response.failed error type = %v, want conflict_error", got)
			}
		}
	}
	if secondRespID == "" {
		t.Fatal("missing failed response id")
	}
	if !sawFailed {
		t.Fatal("second stream missing response.failed")
	}
	if !sawDone {
		t.Fatal("second stream missing [DONE]")
	}
	if _, ok := srv.responseToSession.Load(secondRespID); ok {
		t.Fatalf("failed response id %q should not be registered for chaining", secondRespID)
	}

	close(provider.releaseSecond)

	sawCompleted := false
	for {
		eventName, data, ok := readSSEEvent(t, firstScanner)
		if !ok {
			break
		}
		if data == "[DONE]" {
			break
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unmarshal completed first SSE payload: %v", err)
		}
		if eventName == "response.completed" {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatal("first stream missing response.completed")
	}
	if _, ok := srv.responseToSession.Load(firstRespID); !ok {
		t.Fatalf("successful response id %q should be registered for chaining", firstRespID)
	}

	code, _ := doResponses(t, srv, `{"input":"follow-up","previous_response_id":"`+firstRespID+`"}`)
	if code != http.StatusOK {
		t.Fatalf("chained request status = %d, want 200", code)
	}
}

func TestStreamResponses_AskUserRoundTrip(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddToolCall("call-ask", tools.AskUserToolName, map[string]any{
		"questions": []map[string]any{{
			"header":   "Color",
			"question": "Pick a color",
			"options": []map[string]any{
				{"label": "Red", "description": "Warm"},
				{"label": "Blue", "description": "Cool"},
			},
		}},
	})
	provider.AddTextResponse("Thanks for answering")

	factory := func(ctx context.Context) (*serveRuntime, error) {
		engine := llm.NewEngine(provider, nil)
		engine.RegisterTool(tools.NewAskUserTool())
		rt := &serveRuntime{
			provider:     provider,
			engine:       engine,
			defaultModel: "mock-model",
		}
		rt.Touch()
		return rt, nil
	}

	mgr := newServeSessionManager(time.Minute, 10, factory)
	defer mgr.Close()
	srv := &serveServer{sessionMgr: mgr}
	mgr.onEvict = func(rt *serveRuntime) {
		for _, rid := range rt.getResponseIDs() {
			srv.responseToSession.Delete(rid)
		}
	}

	sessionID := "sess-ask-user"
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session_id", sessionID)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.handleResponses(rr, req)
		close(done)
	}()

	var runtime *serveRuntime
	pendingReady := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rt, ok := mgr.Get(sessionID)
		if ok {
			runtime = rt
			rt.askUserMu.Lock()
			_, pendingReady = rt.pendingAskUsers["call-ask"]
			rt.askUserMu.Unlock()
			if pendingReady {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runtime == nil || !pendingReady {
		t.Fatal("timed out waiting for pending ask_user prompt")
	}

	submitReq := httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sessionID+"/ask_user",
		strings.NewReader(`{"call_id":"call-ask","answers":[{"selected":"Blue","is_custom":false}]}`))
	submitReq.Header.Set("Content-Type", "application/json")
	submitRR := httptest.NewRecorder()
	srv.handleSessionByID(submitRR, submitReq)
	if submitRR.Code != http.StatusOK {
		t.Fatalf("ask_user submit status = %d, body = %s", submitRR.Code, submitRR.Body.String())
	}

	var submitBody struct {
		Summary string                `json:"summary"`
		Answers []tools.AskUserAnswer `json:"answers"`
	}
	if err := json.Unmarshal(submitRR.Body.Bytes(), &submitBody); err != nil {
		t.Fatalf("decode ask_user submit response: %v", err)
	}
	if submitBody.Summary != "Color: Blue" {
		t.Fatalf("summary = %q, want %q", submitBody.Summary, "Color: Blue")
	}
	if len(submitBody.Answers) != 1 || submitBody.Answers[0].Selected != "Blue" {
		t.Fatalf("answers = %#v", submitBody.Answers)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for streaming response to finish")
	}

	if rr.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", rr.Code)
	}

	var sawPrompt bool
	var sawCompleted bool
	scanner := bufio.NewScanner(rr.Body)
	var currentEvent string
	var promptData serveAskUserPrompt
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		switch currentEvent {
		case "response.ask_user.prompt":
			sawPrompt = true
			if err := json.Unmarshal([]byte(data), &promptData); err != nil {
				t.Fatalf("decode ask_user prompt: %v", err)
			}
		case "response.completed":
			if data != "[DONE]" {
				sawCompleted = true
			}
		}
	}
	if !sawPrompt {
		t.Fatal("response.ask_user.prompt event not found")
	}
	if promptData.CallID != "call-ask" || len(promptData.Questions) != 1 || promptData.Questions[0].Header != "Color" {
		t.Fatalf("prompt data = %#v", promptData)
	}
	if !sawCompleted {
		t.Fatal("response.completed event not found")
	}
	if len(provider.Requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(provider.Requests))
	}

	var toolResult *tools.AskUserResult
	for _, msg := range provider.Requests[1].Messages {
		for _, part := range msg.Parts {
			if part.Type != llm.PartToolResult || part.ToolResult == nil || part.ToolResult.Name != tools.AskUserToolName {
				continue
			}
			var parsed tools.AskUserResult
			if err := json.Unmarshal([]byte(part.ToolResult.Content), &parsed); err != nil {
				t.Fatalf("decode tool result content: %v", err)
			}
			toolResult = &parsed
		}
	}
	if toolResult == nil {
		t.Fatal("second provider request missing ask_user tool result")
	}
	if len(toolResult.Answers) != 1 || toolResult.Answers[0].Selected != "Blue" {
		t.Fatalf("tool result answers = %#v", toolResult.Answers)
	}
}
