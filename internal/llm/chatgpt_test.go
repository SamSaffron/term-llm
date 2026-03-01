package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestBuildChatGPTInput_AssistantReasoningReplay(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				{
					Type:                      PartText,
					Text:                      "Here is the answer.",
					ReasoningContent:          "I checked the relevant files first.",
					ReasoningItemID:           "rs_chatgpt_1",
					ReasoningEncryptedContent: "enc_chatgpt_1",
				},
			},
		},
	}

	_, input := buildChatGPTInput(messages)
	if len(input) != 2 {
		t.Fatalf("expected 2 input items (reasoning + message), got %d", len(input))
	}

	var sawReasoning bool
	var sawAssistantMessage bool

	for _, itemAny := range input {
		item, ok := itemAny.(map[string]interface{})
		if !ok {
			t.Fatalf("expected map item, got %T", itemAny)
		}

		itemType, _ := item["type"].(string)
		switch itemType {
		case "reasoning":
			sawReasoning = true
			if item["id"] != "rs_chatgpt_1" {
				t.Errorf("expected reasoning id rs_chatgpt_1, got %#v", item["id"])
			}
			if item["encrypted_content"] != "enc_chatgpt_1" {
				t.Errorf("expected encrypted content enc_chatgpt_1, got %#v", item["encrypted_content"])
			}

			summary, ok := item["summary"].([]map[string]string)
			if !ok {
				t.Fatalf("expected summary as []map[string]string, got %T", item["summary"])
			}
			if len(summary) != 1 {
				t.Fatalf("expected one summary item, got %d", len(summary))
			}
			if summary[0]["type"] != "summary_text" {
				t.Errorf("expected summary type summary_text, got %q", summary[0]["type"])
			}
			if summary[0]["text"] != "I checked the relevant files first." {
				t.Errorf("unexpected summary text: %q", summary[0]["text"])
			}

		case "message":
			role, _ := item["role"].(string)
			if role == "assistant" {
				sawAssistantMessage = true
			}
		}
	}

	if !sawReasoning {
		t.Fatal("expected reasoning item")
	}
	if !sawAssistantMessage {
		t.Fatal("expected assistant message item")
	}
}

func TestBuildChatGPTInput_AssistantReasoningReplayEmptySummary(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				{
					Type:                      PartText,
					Text:                      "Answer text",
					ReasoningItemID:           "rs_chatgpt_empty",
					ReasoningEncryptedContent: "enc_chatgpt_empty",
				},
			},
		},
	}

	_, input := buildChatGPTInput(messages)
	if len(input) != 2 {
		t.Fatalf("expected 2 input items (reasoning + message), got %d", len(input))
	}

	for _, itemAny := range input {
		item, ok := itemAny.(map[string]interface{})
		if !ok {
			t.Fatalf("expected map item, got %T", itemAny)
		}
		if item["type"] != "reasoning" {
			continue
		}

		summary, ok := item["summary"].([]map[string]string)
		if !ok {
			t.Fatalf("expected summary as []map[string]string, got %T", item["summary"])
		}
		if len(summary) != 0 {
			t.Fatalf("expected empty summary array, got %d items", len(summary))
		}
		return
	}

	t.Fatal("expected reasoning item")
}

func TestBuildChatGPTInput_SkipsUnresolvedToolCalls(t *testing.T) {
	messages := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{
				{Type: PartText, Text: "Queued."},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "fc_missing",
						Name:      "wait_for_agent",
						Arguments: json.RawMessage(`{"id":"job-1"}`),
					},
				},
				{
					Type: PartToolCall,
					ToolCall: &ToolCall{
						ID:        "fc_resolved",
						Name:      "queue_agent",
						Arguments: json.RawMessage(`{"task":"spec"}`),
					},
				},
			},
		},
		ToolResultMessage("fc_resolved", "queue_agent", "ok", nil),
	}

	_, input := buildChatGPTInput(messages)

	var functionCalls []map[string]interface{}
	var outputs []map[string]interface{}
	for _, itemAny := range input {
		item, ok := itemAny.(map[string]interface{})
		if !ok {
			t.Fatalf("expected map item, got %T", itemAny)
		}
		switch item["type"] {
		case "function_call":
			functionCalls = append(functionCalls, item)
		case "function_call_output":
			outputs = append(outputs, item)
		}
	}

	if len(functionCalls) != 1 {
		t.Fatalf("expected exactly one function_call, got %d", len(functionCalls))
	}
	if functionCalls[0]["call_id"] != "fc_resolved" {
		t.Fatalf("expected resolved call_id fc_resolved, got %#v", functionCalls[0]["call_id"])
	}

	if len(outputs) != 1 {
		t.Fatalf("expected exactly one function_call_output, got %d", len(outputs))
	}
	if outputs[0]["call_id"] != "fc_resolved" {
		t.Fatalf("expected output call_id fc_resolved, got %#v", outputs[0]["call_id"])
	}
}

func TestChatGPTStream_ReasoningSummaryByOutputIndex(t *testing.T) {
	origClient := chatGPTHTTPClient
	defer func() {
		chatGPTHTTPClient = origClient
	}()

	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_chatgpt_idx","encrypted_content":"enc_chatgpt_idx"}}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":1,"delta":"summary via output index"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_chatgpt_idx","encrypted_content":"enc_chatgpt_idx"}}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}}`,
		`data: [DONE]`,
	}, "\n")

	chatGPTHTTPClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(sse)),
				Header:     make(http.Header),
			}, nil
		}),
	}

	provider := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{
		AccessToken: "test-token",
		AccountID:   "test-account",
		ExpiresAt:   time.Now().Add(1 * time.Hour).Unix(),
	}, "gpt-5.2")

	stream, err := provider.Stream(context.Background(), Request{
		Model:    "gpt-5.2",
		Messages: []Message{UserText("hello")},
	})
	if err != nil {
		t.Fatalf("stream creation failed: %v", err)
	}
	defer stream.Close()

	var reasoningEvent *Event
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		if event.Type == EventReasoningDelta {
			ev := event
			reasoningEvent = &ev
		}
		if event.Type == EventDone {
			break
		}
	}

	if reasoningEvent == nil {
		t.Fatal("expected reasoning event")
	}
	if reasoningEvent.Text != "summary via output index" {
		t.Fatalf("expected reasoning summary from output_index delta, got %q", reasoningEvent.Text)
	}
	if reasoningEvent.ReasoningItemID != "rs_chatgpt_idx" {
		t.Fatalf("expected reasoning item id rs_chatgpt_idx, got %q", reasoningEvent.ReasoningItemID)
	}
	if reasoningEvent.ReasoningEncryptedContent != "enc_chatgpt_idx" {
		t.Fatalf("expected encrypted content enc_chatgpt_idx, got %q", reasoningEvent.ReasoningEncryptedContent)
	}
}
