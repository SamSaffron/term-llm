package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildResponsesInput_FilePolicyFallsBackToTextWhenNativeDisabled(t *testing.T) {
	policy := DefaultPortableTextFileUploadPolicy()
	messages := []Message{{Role: RoleUser, Parts: []Part{{
		Type: PartFile,
		Text: FormatEmbeddedFileText("data.csv", "text/csv", "a,b\n"),
		FileData: &ToolFileData{
			MediaType: "text/csv",
			Base64:    "YSxiCg==",
			Filename:  "data.csv",
			SizeBytes: 4,
		},
	}}}}

	input := BuildResponsesInputWithFilePolicy(messages, &policy)
	if len(input) != 1 {
		t.Fatalf("len(input) = %d, want 1", len(input))
	}
	content, ok := input[0].Content.(string)
	if !ok {
		t.Fatalf("content type = %T, want string", input[0].Content)
	}
	if !strings.Contains(content, "a,b") || strings.Contains(content, "input_file") {
		t.Fatalf("fallback content = %q", content)
	}
}

// TestBuildResponsesInput_EmbedsTextUploadsInsteadOfNativeFiles is the regression
// test for uploads such as common_helper.rb: Chrome labels them with a MIME type
// the API does not accept, so a native input_file made every request (and every
// replay of the stored turn) fail with "unsupported MIME type".
func TestBuildResponsesInput_EmbedsTextUploadsInsteadOfNativeFiles(t *testing.T) {
	for _, tc := range []struct{ name, filename, mediaType string }{
		{"ruby chrome label", "common_helper.rb", "text/x-ruby-script"},
		{"ruby linux label", "common_helper.rb", "application/x-ruby"},
		{"go label", "main.go", "text/x-go"},
		{"python label", "tool.py", "text/x-script.python"},
		{"shell label", "build.sh", "text/x-shellscript"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "puts 'hello'\n"
			part := Part{
				Type: PartFile,
				Text: FormatEmbeddedFileText(tc.filename, tc.mediaType, body),
				FileData: &ToolFileData{
					MediaType: tc.mediaType,
					Base64:    base64.StdEncoding.EncodeToString([]byte(body)),
					Filename:  tc.filename,
					SizeBytes: int64(len(body)),
				},
				FilePath: "/tmp/term-llm/" + tc.filename,
			}

			input := BuildResponsesInput([]Message{{Role: RoleUser, Parts: []Part{part}}})
			if len(input) != 1 {
				t.Fatalf("len(input) = %d, want 1", len(input))
			}
			content, ok := input[0].Content.(string)
			if !ok {
				t.Fatalf("content = %#v, want inline text instead of a native file part", input[0].Content)
			}
			if content != part.Text {
				t.Fatalf("content = %q, want stored text %q", content, part.Text)
			}
			if !strings.Contains(content, body) {
				t.Fatalf("content %q is missing the file body", content)
			}
			if strings.Contains(content, "Contents are not included") {
				t.Fatalf("content discarded the embedded body: %q", content)
			}
		})
	}
}

// TestBuildResponsesInput_KeepsSpreadsheetsOnNativeFilePath pins the spreadsheet
// carve-out: CSV/TSV keep travelling as input_file so provider-side spreadsheet
// handling still applies and a large table is not inlined verbatim.
func TestBuildResponsesInput_KeepsSpreadsheetsOnNativeFilePath(t *testing.T) {
	for _, tc := range []struct{ name, filename, mediaType, wantMediaType string }{
		{"csv", "data.csv", "text/csv", "text/csv"},
		{"tsv canonicalized", "data.tsv", "text/tab-separated-values", "text/tsv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "name,count\napples,3\n"
			message := Message{Role: RoleUser, Parts: []Part{{
				Type: PartFile,
				Text: FormatEmbeddedFileText(tc.filename, tc.mediaType, body),
				FileData: &ToolFileData{
					MediaType: tc.mediaType,
					Base64:    base64.StdEncoding.EncodeToString([]byte(body)),
					Filename:  tc.filename,
					SizeBytes: int64(len(body)),
				},
				FilePath: "/tmp/term-llm/" + tc.filename,
			}}}

			input := BuildResponsesInput([]Message{message})
			if len(input) != 1 {
				t.Fatalf("len(input) = %d, want 1", len(input))
			}
			parts, ok := input[0].Content.([]ResponsesContentPart)
			if !ok || len(parts) != 1 {
				t.Fatalf("content = %#v, want one native file part", input[0].Content)
			}
			if parts[0].Type != "input_file" || parts[0].Filename != tc.filename {
				t.Fatalf("file part = %#v", parts[0])
			}
			// The canonical token is both validated against the allowlist and
			// serialized into file_data, so the two can never disagree.
			want := fmt.Sprintf("data:%s;base64,%s", tc.wantMediaType, base64.StdEncoding.EncodeToString([]byte(body)))
			if parts[0].FileData != want {
				t.Fatalf("file_data = %q, want %q", parts[0].FileData, want)
			}
		})
	}
}

// TestBuildResponsesInput_CapsPreferredTextEmbedBySize pins the budget of the
// preferred inline path. Inlining is now the first choice for text-like uploads,
// so it needs its own conservative cap: a multi-megabyte log would otherwise be
// spliced into the prompt on every replay of the turn and blow the context window
// - the same permanently failing session the native-file repair removed.
func TestBuildResponsesInput_CapsPreferredTextEmbedBySize(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		wantNative bool
	}{
		{"small body inlines", strings.Repeat("log line\n", 128), false},
		{"body over the preferred cap travels natively", strings.Repeat("x", maxPreferredTextEmbedBytes+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			part := Part{
				Type: PartFile,
				Text: FormatEmbeddedFileText("server.log", "text/plain", tc.body),
				FileData: &ToolFileData{
					MediaType: "text/plain",
					Base64:    base64.StdEncoding.EncodeToString([]byte(tc.body)),
					Filename:  "server.log",
					SizeBytes: int64(len(tc.body)),
				},
				FilePath: "/tmp/term-llm/server.log",
			}

			input := BuildResponsesInput([]Message{{Role: RoleUser, Parts: []Part{part}}})
			if len(input) != 1 {
				t.Fatalf("len(input) = %d, want 1", len(input))
			}
			if tc.wantNative {
				parts, ok := input[0].Content.([]ResponsesContentPart)
				if !ok || len(parts) != 1 || parts[0].Type != "input_file" {
					t.Fatalf("content = %#v, want one native file part", input[0].Content)
				}
				want := fmt.Sprintf("data:text/plain;base64,%s", part.FileData.Base64)
				if parts[0].FileData != want {
					t.Fatalf("file_data = %q, want %q", parts[0].FileData, want)
				}
				return
			}
			content, ok := input[0].Content.(string)
			if !ok || content != part.Text {
				t.Fatalf("content = %#v, want the inline body", input[0].Content)
			}
			if strings.Contains(content, "Contents are not included") {
				t.Fatalf("content discarded the embedded body: %q", content)
			}
		})
	}
}

func TestBuildResponsesInput_FileFallbackIncludesPathAndMetadata(t *testing.T) {
	for _, mediaType := range []string{"application/zip", "application/x-unknown", "application/pdf"} {
		t.Run(mediaType, func(t *testing.T) {
			policy := DefaultPortableTextFileUploadPolicy()
			part := Part{Type: PartFile, Text: "legacy fallback without a path", FilePath: "/tmp/uploads/user-file", FileData: &ToolFileData{Filename: "user-file", MediaType: mediaType, Base64: "AP8BgA==", SizeBytes: 4}}
			input := BuildResponsesInputWithFilePolicy([]Message{{Role: RoleUser, Parts: []Part{part}}}, &policy)
			if len(input) != 1 {
				t.Fatalf("input = %#v", input)
			}
			text, ok := input[0].Content.(string)
			if !ok {
				t.Fatalf("unsupported native payload: %#v", input)
			}
			for _, want := range []string{"user-file", mediaType, "4 bytes", part.FilePath, "Contents are not included"} {
				if !strings.Contains(text, want) {
					t.Fatalf("text %q missing %q", text, want)
				}
			}
			if strings.Contains(text, part.FileData.Base64) {
				t.Fatalf("binary embedded: %q", text)
			}
		})
	}
}

func TestResponsesClientWebSocketTimeoutLogsLastProtocolProgress(t *testing.T) {
	const pingInterval = 10 * time.Millisecond

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_stalled"}}); err != nil {
			return
		}
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := conn.WriteControl(websocket.PingMessage, []byte("heartbeat"), time.Now().Add(time.Second)); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	logDir := t.TempDir()
	logger, err := NewDebugLogger(logDir, "ws-stall")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(withDebugDiagnosticSink(context.Background(), logger), 120*time.Millisecond)
	defer cancel()
	client := &ResponsesClient{
		BaseURL:                    server.URL,
		UseWebSocket:               true,
		WebSocketIdleTimeout:       40 * time.Millisecond,
		WebSocketFirstEventTimeout: 30 * time.Millisecond,
	}
	stream, err := client.Stream(ctx, ResponsesRequest{
		Model:          "gpt-test",
		SessionID:      "session-stalled",
		Input:          []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Stream:         true,
		ForceWebSocket: true,
	}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			if !errors.Is(recvErr, context.DeadlineExceeded) && !strings.Contains(recvErr.Error(), "response idle timeout") {
				t.Fatalf("Recv error = %v, want WebSocket idle timeout or outer fallback deadline", recvErr)
			}
			break
		}
		if event.Type == EventError {
			if !strings.Contains(event.Err.Error(), "response idle timeout") {
				t.Fatalf("stream error = %v, want application idle timeout", event.Err)
			}
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(logDir, "ws-stall.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["type"] == "diagnostic" && entry["name"] == "responses_websocket_terminated" {
			diagnostic = entry
		}
	}
	if diagnostic == nil {
		t.Fatalf("missing WebSocket termination diagnostic in %s", raw)
	}
	data, _ := diagnostic["data"].(map[string]any)
	if got, _ := data["error"].(string); !strings.Contains(got, "response idle timeout") {
		t.Fatalf("termination error = %q, want application idle timeout", got)
	}
	if got := data["last_protocol_event"]; got != "response.created" {
		t.Fatalf("last_protocol_event = %v, want response.created", got)
	}
	if got := data["meaningful_event_count"]; got != float64(0) {
		t.Fatalf("meaningful_event_count = %v, want 0", got)
	}
	if got, _ := data["ping_frame_count"].(float64); got < 1 {
		t.Fatalf("ping_frame_count = %v, want positive: %v", data["ping_frame_count"], data)
	}
	if got := data["context_error"]; got != "" {
		t.Fatalf("context_error = %v, want empty because application timeout fired first", got)
	}
	if data["pool_lease_admitted_at_start"] != true || data["pool_lease_active_at_start"] != true || data["pool_connections_for_key_at_start"] != float64(1) {
		t.Fatalf("pool start snapshot = admitted:%v active:%v count:%v, want true/true/1", data["pool_lease_admitted_at_start"], data["pool_lease_active_at_start"], data["pool_connections_for_key_at_start"])
	}
	if admitted, _ := data["pool_lease_admitted_at_diagnostic"].(bool); admitted {
		if data["pool_lease_active_at_diagnostic"] != true || data["pool_connections_at_diagnostic"] != float64(1) {
			t.Fatalf("admitted diagnostic pool snapshot is inconsistent: %v", data)
		}
	} else if data["pool_lease_active_at_diagnostic"] != false || data["pool_connections_at_diagnostic"] != float64(0) {
		t.Fatalf("released diagnostic pool snapshot is inconsistent: %v", data)
	}
}

func TestValidateResponsesOptionsGPT56ProviderGating(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		wantErr  bool
	}{
		{name: "OpenAI Sol", provider: "openai", model: "gpt-5.6-sol", wantErr: false},
		{name: "OpenAI variant effort suffix", provider: "openai", model: "gpt-5.6-terra-max", wantErr: false},
		{name: "older OpenAI", provider: "openai", model: "gpt-5.5", wantErr: true},
		{name: "ChatGPT does not support Pro", provider: "chatgpt", model: "gpt-5.6-sol-max", wantErr: true},
		{name: "compatible provider", provider: "openai-compatible", model: "gpt-5.6-sol", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateResponsesOptions(tt.provider, tt.model, &ResponsesOptions{ReasoningMode: "pro"}, nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateResponsesOptions(%q, %q) error = %v, wantErr %t", tt.provider, tt.model, err, tt.wantErr)
			}
		})
	}
}

func TestValidateResponsesOptionsNormalizesAndDefaults(t *testing.T) {
	opts, err := validateResponsesOptions("openai", "gpt-5.6-sol", &ResponsesOptions{
		ReasoningMode:    " Pro ",
		ReasoningContext: " ALL_TURNS ",
		MultiAgent:       MultiAgentOptions{Enabled: true, EnabledSet: true},
		PromptCache:      PromptCacheOptions{Mode: " EXPLICIT ", TTL: " 30M "},
	}, nil)
	if err != nil {
		t.Fatalf("validateResponsesOptions() error = %v", err)
	}
	if opts.ReasoningMode != "pro" || opts.ReasoningContext != "all_turns" {
		t.Fatalf("reasoning options = %q/%q", opts.ReasoningMode, opts.ReasoningContext)
	}
	if opts.MultiAgent.MaxConcurrentSubagents != 3 {
		t.Fatalf("MaxConcurrentSubagents = %d, want 3", opts.MultiAgent.MaxConcurrentSubagents)
	}
	if opts.PromptCache.Mode != "explicit" || opts.PromptCache.TTL != "30m" {
		t.Fatalf("prompt cache options = %+v", opts.PromptCache)
	}
}

func TestBuildResponsesInput_FilePolicyAllowsNativePDF(t *testing.T) {
	messages := []Message{{Role: RoleUser, Parts: []Part{{
		Type: PartFile,
		Text: "[User uploaded file: doc.pdf — saved locally]",
		FileData: &ToolFileData{
			MediaType: "application/pdf",
			Base64:    "aGVsbG8=",
			Filename:  "doc.pdf",
			SizeBytes: 5,
		},
		FilePath: "/tmp/term-llm/doc.pdf",
	}}}}

	input := BuildResponsesInput(messages)
	if len(input) != 1 {
		t.Fatalf("len(input) = %d, want 1", len(input))
	}
	parts, ok := input[0].Content.([]ResponsesContentPart)
	if !ok || len(parts) != 1 {
		t.Fatalf("content = %#v, want one content part", input[0].Content)
	}
	if parts[0].Type != "input_file" || parts[0].Filename != "doc.pdf" {
		t.Fatalf("file part = %#v", parts[0])
	}
	if parts[0].FileData != "data:application/pdf;base64,aGVsbG8=" {
		t.Fatalf("file_data = %q", parts[0].FileData)
	}
	if strings.Contains(fmt.Sprint(input[0].Content), "/tmp/term-llm") {
		t.Fatalf("native file content leaked local path: %#v", input[0].Content)
	}
}

// TestBuildResponsesInput_KeepsNativeFileWhenStoredTextIsOnlyANotice covers the
// other half of the embed decision: ingest stores a FormatUploadedFileNotice stub
// instead of a body whenever the upload bytes are not plain UTF-8 (UTF-16 text,
// binary documents, ...). The notice is not inline content, so the base64 the
// provider can decode itself must still travel as input_file rather than being
// replaced by "Contents are not included".
func TestBuildResponsesInput_KeepsNativeFileWhenStoredTextIsOnlyANotice(t *testing.T) {
	for _, tc := range []struct{ name, filename, mediaType string }{
		{"utf16 text", "notes.txt", "text/plain"},
		{"utf16 sql", "query.sql", "text/x-sql"},
		{"binary rtf", "brief.rtf", "application/rtf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base64Data := base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe, 0x00, 0x41, 0x00, 0x42})
			notice := formatUploadedFileNotice(tc.filename, tc.mediaType, "/tmp/term-llm/"+tc.filename, 6)
			part := Part{
				Type:     PartFile,
				Text:     notice,
				FilePath: "/tmp/term-llm/" + tc.filename,
				FileData: &ToolFileData{
					MediaType: tc.mediaType,
					Base64:    base64Data,
					Filename:  tc.filename,
					SizeBytes: 6,
				},
			}

			input := BuildResponsesInput([]Message{{Role: RoleUser, Parts: []Part{part}}})
			if len(input) != 1 {
				t.Fatalf("len(input) = %d, want 1", len(input))
			}
			if content, ok := input[0].Content.(string); ok {
				t.Fatalf("content = %q, want a native input_file instead of the notice", content)
			}
			parts, ok := input[0].Content.([]ResponsesContentPart)
			if !ok || len(parts) != 1 {
				t.Fatalf("content = %#v, want one native file part", input[0].Content)
			}
			if parts[0].Type != "input_file" {
				t.Fatalf("file part = %#v", parts[0])
			}
			want := fmt.Sprintf("data:%s;base64,%s", tc.mediaType, base64Data)
			if parts[0].FileData != want {
				t.Fatalf("file_data = %q, want %q", parts[0].FileData, want)
			}
		})
	}
}

// TestBuildResponsesInput_NeverRelabelsBinaryUploadsAsText guards the canonicalizer
// boundary: a binary MIME label with a text-looking filename must not turn into a
// text part, because that would send data:text/markdown;base64,<png bytes>, which
// the API accepts and the model then reads as garbage.
func TestBuildResponsesInput_NeverRelabelsBinaryUploadsAsText(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03}
	for _, filename := range []string{"photo.md", "photo.txt"} {
		t.Run(filename, func(t *testing.T) {
			part := Part{
				Type:     PartFile,
				Text:     formatUploadedFileNotice(filename, "image/png", "/tmp/term-llm/"+filename, int64(len(png))),
				FilePath: "/tmp/term-llm/" + filename,
				FileData: &ToolFileData{
					MediaType: "image/png",
					Base64:    base64.StdEncoding.EncodeToString(png),
					Filename:  filename,
					SizeBytes: int64(len(png)),
				},
			}

			input := BuildResponsesInput([]Message{{Role: RoleUser, Parts: []Part{part}}})
			if len(input) != 1 {
				t.Fatalf("len(input) = %d, want 1", len(input))
			}
			if rendered := fmt.Sprint(input[0].Content); strings.Contains(rendered, "data:") {
				t.Fatalf("content = %#v, want no file payload for an upload labelled image/png", input[0].Content)
			}
			content, ok := input[0].Content.(string)
			if !ok || !strings.Contains(content, "Contents are not included") {
				t.Fatalf("content = %#v, want the uploaded-file notice", input[0].Content)
			}
		})
	}
}

func TestBuildResponsesToolsStrictOptIn(t *testing.T) {
	specs := []ToolSpec{
		{
			Name:   "get_weather",
			Strict: true,
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"location": map[string]interface{}{"type": "string"},
				},
			},
		},
	}
	tools := BuildResponsesTools(specs)
	tool := tools[0].(ResponsesTool)
	if !tool.Strict {
		t.Error("expected Strict to be true when opted in")
	}
	if _, ok := tool.Parameters["required"]; !ok {
		t.Error("expected 'required' field to be added by strict normalization")
	}
	if tool.Parameters["additionalProperties"] != false {
		t.Error("expected 'additionalProperties' to be false in strict mode")
	}
}

// Test-only shims for unexported names that remaining root tests still use.

type responsesOutputItem = responses.OutputItem

const (
	responsesWebSocketBetaHeader = responses.WebSocketBetaHeader
	maxPreferredTextEmbedBytes   = responses.MaxPreferredTextEmbedBytes
)

func composeBetaHeader(existing, addition string) string {
	return responses.ComposeBetaHeader(existing, addition)
}

func buildResponsesAssistantItems(parts []Part) []ResponsesInputItem {
	return responses.BuildAssistantItems(parts)
}

func trimResponsesContinuationToDiscoveryOutput(input []ResponsesInputItem) []ResponsesInputItem {
	return responses.TrimContinuationToDiscoveryOutput(input)
}

func isResponsesWebSearchCall(raw json.RawMessage) bool { return responses.IsWebSearchCall(raw) }

func formatUploadedFileNotice(filename, mediaType, localPath string, sizeBytes int64) string {
	return FormatUploadedFileNotice(filename, mediaType, localPath, sizeBytes)
}

// Shared test helpers (also kept inside providers/responses for its own tests).

const testWebSearchCallRaw = `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"example"}}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func drainStreamToDone(t *testing.T, stream Stream) {
	t.Helper()
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		if event.Type == EventError {
			t.Fatalf("stream returned error event: %v", event.Err)
		}
		if event.Type == EventDone {
			return
		}
	}
}

func cumulativeReasoningEvents(t *testing.T) []Event {
	t.Helper()
	return streamResponsesFixture(t, cumulativeReasoningSSEFixture(), ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Stream: true,
		StreamOptions: &ResponsesStreamOptions{
			ReasoningSummaryDelivery: "sequential_cutoff",
		},
	})
}

func replayPart(raw string) Part {
	return Part{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: json.RawMessage(raw)}}
}

func cumulativeReasoningSSEFixture() string {
	return strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_0","encrypted_content":"enc_0"}}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":0,"item_id":"rs_0","summary_index":0,"text":"**A**"}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":0,"item_id":"rs_0","summary_index":1,"text":"**B**"}`,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_0","encrypted_content":"enc_0","summary":[{"type":"summary_text","text":"**A**"},{"type":"summary_text","text":"**B**"}]}}`,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1"}}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":1,"item_id":"rs_1","summary_index":0,"text":"**A**"}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":1,"item_id":"rs_1","summary_index":1,"text":"**B**"}`,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[{"type":"summary_text","text":"**A**"},{"type":"summary_text","text":"**B**"}]}}`,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"reasoning","id":"rs_2","encrypted_content":"enc_2"}}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":2,"item_id":"rs_2","summary_index":0,"text":"**A**"}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":2,"item_id":"rs_2","summary_index":1,"text":"**B**"}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":2,"item_id":"rs_2","summary_index":2,"text":"**C**"}`,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"reasoning","id":"rs_2","encrypted_content":"enc_2","summary":[{"type":"summary_text","text":"**A**"},{"type":"summary_text","text":"**B**"},{"type":"summary_text","text":"**C**"}]}}`,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":3,"item":{"type":"reasoning","id":"rs_3","encrypted_content":"enc_3"}}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":3,"item_id":"rs_3","summary_index":0,"text":"**A**"}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":3,"item_id":"rs_3","summary_index":1,"text":"**B**"}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":3,"item_id":"rs_3","summary_index":2,"text":"**C**"}`,
		`event: response.reasoning_summary_text.done`,
		`data: {"type":"response.reasoning_summary_text.done","output_index":3,"item_id":"rs_3","summary_index":3,"text":"**D**"}`,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":3,"item":{"type":"reasoning","id":"rs_3","encrypted_content":"enc_3","summary":[{"type":"summary_text","text":"**A**"},{"type":"summary_text","text":"**B**"},{"type":"summary_text","text":"**C**"},{"type":"summary_text","text":"**D**"}]}}`,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	}, "\n")
}

func streamResponsesFixture(t *testing.T, sse string, req ResponsesRequest) []Event {
	t.Helper()
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})}
	client := &ResponsesClient{
		BaseURL:    "https://example.test/v1/responses",
		HTTPClient: httpClient,
	}
	stream, err := client.Stream(context.Background(), req, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var events []Event
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return events
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		events = append(events, event)
		if event.Type == EventDone {
			return events
		}
	}
}
