package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestCompatConfiguredOutputLimitReachesWire(t *testing.T) {
	for _, kind := range []config.ProviderType{config.ProviderTypeOpenAICompat, config.ProviderTypeVLLM} {
		for _, tc := range []struct {
			name                     string
			provider, model, request int
			want                     int
		}{
			{"provider default", 4096, 0, 0, 4096},
			{"model overrides provider", 4096, 2048, 0, 2048},
			{"explicit smaller request", 4096, 0, 1024, 1024},
			{"explicit request clamped", 4096, 0, 8192, 4096},
			{"unconfigured stays omitted", 0, 0, 0, 0},
		} {
			t.Run(fmt.Sprintf("%s/%s", kind, tc.name), func(t *testing.T) {
				captured := make(chan *int, 1)
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body struct {
						MaxTokens *int `json:"max_tokens"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					captured <- body.MaxTokens
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				t.Cleanup(backend.Close)
				cfg := &config.ProviderConfig{Type: kind, BaseURL: backend.URL, Model: "flashnext", MaxOutputTokens: tc.provider,
					ModelConfigs: []config.ProviderModelConfig{{ID: "flashnext", MaxOutputTokens: tc.model, ReasoningEfforts: []string{"none", "low", "medium", "xhigh"}, DefaultReasoningEffort: "none"}}}
				provider, err := Create("flashnext", cfg, Options{})
				if err != nil {
					t.Fatal(err)
				}
				stream, err := provider.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hello")}, ReasoningEffort: "xhigh", MaxOutputTokens: tc.request})
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				for {
					event, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil || event.Type == core.EventError {
						t.Fatalf("stream failed: event=%+v err=%v", event, err)
					}
				}
				got := <-captured
				if tc.want == 0 {
					if got != nil {
						t.Fatalf("unconfigured max_tokens should be omitted, got %d", *got)
					}
				} else if got == nil || *got != tc.want {
					t.Fatalf("max_tokens = %v, want %d", got, tc.want)
				}
			})
		}
	}
}
