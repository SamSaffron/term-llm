package llm

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDebugLogger_LogRequest(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	sessionID := "test-session"

	logger, err := NewDebugLogger(tmpDir, sessionID)
	if err != nil {
		t.Fatalf("failed to create debug logger: %v", err)
	}
	defer logger.Close()

	// Log a request
	req := Request{
		Model: "test-model",
		Messages: []Message{
			SystemText("You are a helpful assistant"),
			UserText("Hello, world!"),
		},
		Search:            true,
		ParallelToolCalls: true,
	}

	logger.LogRequest("test-provider", "test-model", req)

	// Close to flush
	if err := logger.Close(); err != nil {
		t.Fatalf("failed to close logger: %v", err)
	}

	// Read the log file
	logFile := filepath.Join(tmpDir, sessionID+".jsonl")
	file, err := os.Open(logFile)
	if err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		t.Fatal("expected at least one line in log file")
	}

	var entry map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log entry: %v", err)
	}

	// Verify entry structure
	if entry["type"] != "request" {
		t.Errorf("expected type=request, got %v", entry["type"])
	}
	if entry["provider"] != "test-provider" {
		t.Errorf("expected provider=test-provider, got %v", entry["provider"])
	}
	if entry["session_id"] != "test-session" {
		t.Errorf("expected session_id=test-session, got %v", entry["session_id"])
	}

	reqData, ok := entry["request"].(map[string]any)
	if !ok {
		t.Fatal("expected request to be an object")
	}
	if reqData["search"] != true {
		t.Errorf("expected search=true, got %v", reqData["search"])
	}

	messages, ok := reqData["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %v", reqData["messages"])
	}
}

func TestDebugLogger_LogEvent(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-event-session"

	logger, err := NewDebugLogger(tmpDir, sessionID)
	if err != nil {
		t.Fatalf("failed to create debug logger: %v", err)
	}
	defer logger.Close()

	// Log various events
	events := []Event{
		{Type: EventTextDelta, Text: "Hello"},
		{Type: EventUsage, Use: &Usage{InputTokens: 100, OutputTokens: 50}},
		{Type: EventDone},
	}

	for _, event := range events {
		logger.LogEvent(event)
	}

	// Close to flush
	if err := logger.Close(); err != nil {
		t.Fatalf("failed to close logger: %v", err)
	}

	// Read and count entries
	logFile := filepath.Join(tmpDir, sessionID+".jsonl")
	file, err := os.Open(logFile)
	if err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			t.Fatalf("failed to parse log entry: %v", err)
		}
		if entry["type"] != "event" {
			t.Errorf("expected type=event, got %v", entry["type"])
		}
		count++
	}

	if count != 3 {
		t.Errorf("expected 3 events, got %d", count)
	}
}

func TestDebugLogger_NilSafe(t *testing.T) {
	// Ensure nil logger doesn't panic
	var logger *DebugLogger = nil

	// These should not panic
	logger.LogRequest("provider", "model", Request{})
	logger.LogEvent(Event{Type: EventTextDelta, Text: "test"})
	logger.Close()
}

func TestDebugLogger_CloseIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-idempotent"

	logger, err := NewDebugLogger(tmpDir, sessionID)
	if err != nil {
		t.Fatalf("failed to create debug logger: %v", err)
	}

	// Log something
	logger.LogEvent(Event{Type: EventTextDelta, Text: "test"})

	// Close multiple times - should not error or panic
	if err := logger.Close(); err != nil {
		t.Errorf("first Close() returned error: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Errorf("second Close() returned error: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Errorf("third Close() returned error: %v", err)
	}

	// LogEvent after close should not panic (silently ignored)
	logger.LogEvent(Event{Type: EventDone})
}

func TestDebugLogger_LogRequestIncludesSessionAndReasoningReplay(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-request-reasoning"

	logger, err := NewDebugLogger(tmpDir, sessionID)
	if err != nil {
		t.Fatalf("failed to create debug logger: %v", err)
	}
	defer logger.Close()

	req := Request{
		Model:     "gpt-5.3-codex",
		SessionID: "session-cache-key-123",
		Messages: []Message{
			UserText("hello"),
			{
				Role: RoleAssistant,
				Parts: []Part{{
					Type:                      PartText,
					Text:                      "answer",
					ReasoningContent:          "summary text",
					ReasoningItemID:           "rs_123",
					ReasoningEncryptedContent: "enc_123456",
				}},
			},
		},
	}

	logger.LogRequest("test-provider", "test-model", req)
	if err := logger.Close(); err != nil {
		t.Fatalf("failed to close logger: %v", err)
	}

	logFile := filepath.Join(tmpDir, sessionID+".jsonl")
	file, err := os.Open(logFile)
	if err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		t.Fatal("expected at least one line in log file")
	}

	var entry map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log entry: %v", err)
	}

	reqData, ok := entry["request"].(map[string]any)
	if !ok {
		t.Fatal("expected request object")
	}
	if got := reqData["session_id"]; got != "session-cache-key-123" {
		t.Fatalf("expected request.session_id to be logged, got %#v", got)
	}

	msgs, ok := reqData["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %#v", reqData["messages"])
	}
	assistant, ok := msgs[1].(map[string]any)
	if !ok {
		t.Fatalf("expected assistant message object, got %#v", msgs[1])
	}
	content, ok := assistant["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("expected assistant content parts array, got %#v", assistant["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("expected content part object, got %#v", content[0])
	}
	if got := part["reasoning_item_id"]; got != "rs_123" {
		t.Fatalf("expected reasoning_item_id rs_123, got %#v", got)
	}
	if got := int(part["reasoning_encrypted_content_len"].(float64)); got != len("enc_123456") {
		t.Fatalf("expected reasoning_encrypted_content_len=%d, got %d", len("enc_123456"), got)
	}
}

func TestDebugLogger_LogReasoningDeltaEventData(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-reasoning-event"

	logger, err := NewDebugLogger(tmpDir, sessionID)
	if err != nil {
		t.Fatalf("failed to create debug logger: %v", err)
	}
	defer logger.Close()

	logger.LogEvent(Event{
		Type:                      EventReasoningDelta,
		Text:                      "reasoning summary",
		ReasoningItemID:           "rs_evt_1",
		ReasoningEncryptedContent: "enc_evt_1",
	})
	logger.LogEvent(Event{Type: EventDone})
	if err := logger.Close(); err != nil {
		t.Fatalf("failed to close logger: %v", err)
	}

	logFile := filepath.Join(tmpDir, sessionID+".jsonl")
	file, err := os.Open(logFile)
	if err != nil {
		t.Fatalf("failed to open log file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		t.Fatal("expected at least one line in log file")
	}

	var entry map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse first log entry: %v", err)
	}

	if entry["event_type"] != "reasoning_delta" {
		t.Fatalf("expected first event_type reasoning_delta, got %#v", entry["event_type"])
	}
	data, ok := entry["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected reasoning_delta event data object, got %#v", entry["data"])
	}
	if got := data["reasoning_item_id"]; got != "rs_evt_1" {
		t.Fatalf("expected reasoning_item_id rs_evt_1, got %#v", got)
	}
	if got := int(data["reasoning_encrypted_content_len"].(float64)); got != len("enc_evt_1") {
		t.Fatalf("expected reasoning_encrypted_content_len=%d, got %d", len("enc_evt_1"), got)
	}
	if got := data["text"]; got != "reasoning summary" {
		t.Fatalf("expected text reasoning summary, got %#v", got)
	}
}
