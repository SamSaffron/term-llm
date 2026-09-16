package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/samsaffron/term-llm/internal/config"
)

func testClientTool() ClientTool {
	return ClientTool{Name: "ui_navigate", Description: "Navigate the hosting UI", Parameters: map[string]any{"type": "object", "properties": map[string]any{"room": map[string]any{"type": "string"}}}}
}

func TestClientToolValidation(t *testing.T) {
	cfg := config.LiveConfig{Provider: config.LiveProviderOpenAI, OpenAI: config.LiveOpenAIConfig{Model: "gpt-realtime"}}
	for _, tc := range []struct {
		name    string
		mutate  func(*ClientTool)
		invalid bool
	}{
		{"valid", func(*ClientTool) {}, false},
		{"built-in", func(tool *ClientTool) { tool.Name = openAIDelegationTool }, true},
		{"invalid-name", func(tool *ClientTool) { tool.Name = "ui_x.y" }, true},
		{"missing-description", func(tool *ClientTool) { tool.Description = "" }, true},
		{"large-description", func(tool *ClientTool) { tool.Description = strings.Repeat("x", 1025) }, true},
		{"not-object", func(tool *ClientTool) { tool.Parameters["type"] = "string" }, true},
		{"missing-properties", func(tool *ClientTool) { delete(tool.Parameters, "properties") }, true},
		{"large-schema", func(tool *ClientTool) { tool.Parameters["description"] = strings.Repeat("x", 16384) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := testClientTool()
			tc.mutate(&tool)
			err := ValidateClientTools(cfg, []ClientTool{tool})
			if (err != nil) != tc.invalid {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	if err := ValidateClientTools(cfg, []ClientTool{testClientTool(), testClientTool()}); err == nil {
		t.Fatal("accepted duplicate")
	}
	if err := ValidateClientTools(cfg, make([]ClientTool, 17)); err == nil {
		t.Fatal("accepted too many tools")
	}
	for _, provider := range []string{"", config.LiveProviderChatGPT, config.LiveProviderOpenAI} {
		unsupported := config.LiveConfig{Provider: provider}
		if err := ValidateClientTools(unsupported, nil); err != nil {
			t.Fatalf("default changed: %v", err)
		}
		if err := ValidateClientTools(unsupported, []ClientTool{testClientTool()}); err == nil {
			t.Fatal("accepted unsupported mode")
		}
	}
}

func TestRealtimeClientToolAdvertisement(t *testing.T) {
	cfg := config.LiveConfig{Provider: config.LiveProviderOpenAI, OpenAI: config.LiveOpenAIConfig{Model: "gpt-realtime"}}
	data, err := openAIRealtimeSessionJSON(cfg, SessionOptions{ClientTools: []ClientTool{testClientTool()}})
	if err != nil {
		t.Fatal(err)
	}
	var got openAIRealtimeSessionPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 2 || got.Tools[0].Name != openAIDelegationTool || got.Tools[1].Name != "ui_navigate" || got.Tools[1].Type != "function" {
		t.Fatalf("tools = %+v", got.Tools)
	}
	event, err := parseOpenAIEvent([]byte(`{"type":"response.function_call_arguments.done","name":"ui_navigate","call_id":"ui_1","arguments":"{}"}`))
	if err != nil || event.Kind != EventUnknown {
		t.Fatalf("sideband must not execute custom tools: %+v / %v", event, err)
	}
}

func TestUnsupportedClientToolsRejectBeforeProviderNetwork(t *testing.T) {
	// No API key/auth or HTTP client needed: validation happens first.
	providers := []Provider{NewOpenAIProvider(config.LiveConfig{Provider: config.LiveProviderOpenAI}, nil), NewChatGPTProvider(config.LiveConfig{Provider: config.LiveProviderChatGPT}, nil, nil)}
	for _, provider := range providers {
		_, err := provider.Start(context.Background(), "offer", SessionOptions{ClientTools: []ClientTool{testClientTool()}})
		if err == nil || !strings.Contains(err.Error(), "client tools require") {
			t.Fatalf("%s: %v", provider.Name(), err)
		}
	}
}

func TestRealtimeFunctionResultContinuationOwnership(t *testing.T) {
	for _, clientContinuation := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-server", true: "opt-in-browser"}[clientContinuation], func(t *testing.T) {
			received := make(chan string, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					_, data, err := conn.ReadMessage()
					if err != nil {
						return
					}
					received <- string(data)
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			channel, err := dialOpenAISideband(ctx, openAISidebandConfig{BaseURL: server.URL, CallID: "test", APIKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			session := newOpenAISession("answer", channel)
			session.clientContinuation = clientContinuation
			if err := session.AppendDelegation(ctx, "call_1", DelegationChunk{Text: "result"}); err != nil {
				t.Fatal(err)
			}
			if err := session.CompleteDelegation(ctx, "call_1"); err != nil {
				t.Fatal(err)
			}
			// A sentinel gives a deterministic end boundary, without timing sleeps.
			if err := channel.Send(ctx, openAIClientEvent{Type: "test.sentinel"}); err != nil {
				t.Fatal(err)
			}
			var messages []string
			for {
				select {
				case data := <-received:
					if strings.Contains(data, "test.sentinel") {
						goto done
					}
					messages = append(messages, data)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
		done:
			want := 2
			if clientContinuation {
				want = 1
			}
			if len(messages) != want || !strings.Contains(messages[0], `"call_id":"call_1"`) || !strings.Contains(messages[0], `"output":"result"`) {
				t.Fatalf("messages = %v", messages)
			}
			if !clientContinuation && !strings.Contains(messages[1], "response.create") {
				t.Fatalf("missing default continuation: %v", messages)
			}
		})
	}
}

func TestRealtimeClientToolStart(t *testing.T) {
	for _, optedIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "client-tools"}[optedIn], func(t *testing.T) {
			payloads := make(chan openAIRealtimeSessionPayload, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/realtime/calls" {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					defer r.MultipartForm.RemoveAll()
					var payload openAIRealtimeSessionPayload
					if err := json.Unmarshal([]byte(r.FormValue("session")), &payload); err != nil {
						t.Error(err)
					}
					payloads <- payload
					w.Header().Set("Location", "/v1/realtime/calls/rtc_test")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte("answer"))
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			cfg := config.LiveConfig{Provider: config.LiveProviderOpenAI, OpenAI: config.LiveOpenAIConfig{Model: "gpt-realtime", APIKey: "test", BaseURL: server.URL + "/v1"}}
			opts := SessionOptions{}
			if optedIn {
				opts.ClientTools = []ClientTool{testClientTool()}
			}
			provider := NewOpenAIProvider(cfg, server.Client())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			started, err := provider.Start(ctx, "offer", opts)
			if err != nil {
				t.Fatal(err)
			}
			session := started.(*openAISession)
			defer session.channel.Close()
			if session.clientContinuation != optedIn || session.AnswerSDP() != "answer" {
				t.Fatalf("session = %+v", session)
			}
			payload := <-payloads
			if len(payload.Tools) != 1+len(opts.ClientTools) || payload.Tools[0].Name != openAIDelegationTool {
				t.Fatalf("tools = %+v", payload.Tools)
			}
			if optedIn && payload.Tools[1].Name != "ui_navigate" {
				t.Fatalf("missing UI tool: %+v", payload.Tools)
			}
		})
	}
}
