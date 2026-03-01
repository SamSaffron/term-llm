package llm

import (
	"context"
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

func TestBuildChatGPTInput_DropsDanglingToolCalls(t *testing.T) {
	messages := []Message{
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "Run a tool"}}},
		{Role: RoleAssistant, Parts: []Part{
			{Type: PartText, Text: "Working on it"},
			{Type: PartToolCall, ToolCall: &ToolCall{
				ID:        "fc_1",
				Name:      "wait_for_agent",
				Arguments: []byte(`{"job_id":"123"}`),
			}},
		}},
		{Role: RoleUser, Parts: []Part{{Type: PartText, Text: "what is the status?"}}},
	}

	_, input := buildChatGPTInput(messages)
	if len(input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(input))
	}

	assistant, ok := input[1].(map[string]interface{})
	if !ok {
		t.Fatalf("expected assistant input item as map, got %T", input[1])
	}
	if assistant["type"] != "message" || assistant["role"] != "assistant" {
		t.Fatalf("expected second item assistant message, got %#v", assistant)
	}
	content, ok := assistant["content"].([]map[string]string)
	if !ok || len(content) != 1 || content[0]["type"] != "output_text" || content[0]["text"] != "Working on it" {
		t.Fatalf("expected assistant text preserved in output_text content, got %#v", assistant["content"])
	}

	for _, itemAny := range input {
		item, ok := itemAny.(map[string]interface{})
		if !ok {
			t.Fatalf("expected map item, got %T", itemAny)
		}
		if item["type"] == "function_call" || item["type"] == "function_call_output" {
			t.Fatalf("expected dangling tool call/result to be removed, got %#v", item)
		}
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
