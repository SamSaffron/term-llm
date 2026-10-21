package opencompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestDeepSeekReplayPreservesEmptyReasoningOnWire(t *testing.T) {
	for _, tt := range []struct {
		name, model     string
		vllm, wantEmpty bool
	}{
		{"opencode", "deepseek-v4.1-flash-low", false, true},
		{"namespaced", "deepseek/deepseek-v4.1-flash", false, true},
		{"case insensitive", "DeepSeek-V4-Pro", false, true},
		{"other model", "other-thinking-model", false, false},
		{"vllm", "deepseek-v4.1-flash", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				Model    string                       `json:"model"`
				Messages []map[string]json.RawMessage `json:"messages"`
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer ts.Close()

			messages := []Message{core.SystemText("system"), UserText("Run tools")}
			for i := 0; i < 26; i++ {
				parts := []Part{{Type: PartToolCall, ToolCall: &ToolCall{
					ID: fmt.Sprintf("call-%d", i), Name: "tick", Arguments: json.RawMessage(`{}`),
				}}}
				if i == 0 {
					parts = append([]Part{{Type: PartText, ReasoningContent: "original reasoning"}}, parts...)
				}
				messages = append(messages, Message{Role: RoleAssistant, Parts: parts}, Message{
					Role: RoleTool, Parts: []Part{{Type: PartToolResult, ToolResult: &ToolResult{
						ID: fmt.Sprintf("call-%d", i), Content: "ok",
					}}},
				})
			}
			messages = append(messages, core.AssistantText("Progress"), UserText("Continue"))
			p := NewOpenAICompatProvider(ts.URL, "test-key", tt.model, "OpenCode Go")
			if tt.vllm {
				p = NewVLLMProvider(ts.URL, "test-key", tt.model, "vLLM").OpenAICompatProvider
			}
			stream, err := p.Stream(context.Background(), Request{Messages: messages})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			for {
				ev, err := stream.Recv()
				if err != nil {
					t.Fatal(err)
				}
				if ev.Type == EventDone {
					break
				}
			}
			if len(got.Messages) != len(messages) {
				t.Fatalf("message count = %d, want %d", len(got.Messages), len(messages))
			}
			for i, msg := range got.Messages {
				assistant := string(msg["role"]) == `"assistant"`
				value, present := msg["reasoning_content"]
				if i == 2 {
					if tt.vllm {
						if present || string(msg["reasoning"]) != `"original reasoning"` {
							t.Fatalf("vLLM replay changed: %s", msg)
						}
					} else if string(value) != `"original reasoning"` {
						t.Fatalf("nonempty reasoning changed: %s", msg)
					}
					continue
				}
				if assistant && tt.wantEmpty {
					if !present || string(value) != `""` {
						t.Errorf("message %d: expected explicit empty reasoning, got %s", i, msg)
					}
				} else if present {
					t.Errorf("message %d: unexpected reasoning_content: %s", i, value)
				}
			}
		})
	}
}
