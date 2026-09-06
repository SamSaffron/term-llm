package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestChatGPTModelSuffixes(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if model, tier := chatGPTModelServiceTier("gpt-reserve-low-fast"); model != "gpt-reserve-low-fast" || tier != "" {
		t.Fatalf("without metadata, got (%q, %q)", model, tier)
	}
	models := []ModelInfo{
		{ID: "gpt-reserve", InputLimit: 272000, ReasoningEfforts: []string{"low", "medium", "max"}, ServiceTiers: []ModelServiceTier{{ID: "priority"}}},
		{ID: "slow", ReasoningEfforts: []string{"low"}},
		{ID: "speed", ReasoningEfforts: []string{"low"}, AdditionalSpeedTiers: []string{"fast"}},
		{ID: "literal", ServiceTiers: []ModelServiceTier{{ID: "priority"}}},
		{ID: "literal-fast"},
	}
	if err := saveChatGPTModelsCache(chatGPTModelsCache{ClientVersion: chatGPTModelsClientVersion, Models: models}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ input, model, effort, tier string }{
		{"gpt-reserve-low", "gpt-reserve", "low", ""},
		{"gpt-reserve-low-fast", "gpt-reserve", "low", ServiceTierFast},
		{"gpt-reserve-fast", "gpt-reserve", "", ServiceTierFast},
		{"gpt-reserve-max-fast", "gpt-reserve", "max", ServiceTierFast},
		{"gpt-reserve-high-fast", "gpt-reserve-high-fast", "", ""},
		{"slow-low-fast", "slow-low-fast", "", ""},
		{"speed-low-fast", "speed", "low", ServiceTierFast},
		{"literal-fast", "literal-fast", "", ""},
		{"unknown-low-fast", "unknown-low-fast", "", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			p := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{}, tc.input)
			if p.model != tc.model || p.effort != tc.effort || p.serviceTier != tc.tier {
				t.Fatalf("got (%s, %s, %s), want (%s, %s, %s)", p.model, p.effort, p.serviceTier, tc.model, tc.effort, tc.tier)
			}
		})
	}
	if got := InputLimitForProviderModel("chatgpt", "gpt-reserve-low-fast"); got != 272000 {
		t.Fatalf("input limit = %d", got)
	}
	if got := ReasoningEffortsForProviderModel("chatgpt", "gpt-reserve-low"); !equalSlice(got, models[0].ReasoningEfforts) {
		t.Fatalf("efforts = %v", got)
	}

	for _, tc := range []struct {
		prefix string
		want   []string
	}{
		{"", []string{"gpt-reserve", "slow", "speed", "literal", "literal-fast"}},
		{"gpt-res", []string{"gpt-reserve"}},
		{"gpt-reserve", []string{"gpt-reserve", "gpt-reserve-fast", "gpt-reserve-low", "gpt-reserve-medium", "gpt-reserve-max"}},
		{"gpt-reserve-l", []string{"gpt-reserve-low"}},
		{"gpt-reserve-low", []string{"gpt-reserve-low", "gpt-reserve-low-fast"}},
		{"gpt-reserve-low-f", []string{"gpt-reserve-low-fast"}},
		{"gpt-reserve-low-fast", []string{"gpt-reserve-low-fast"}},
	} {
		got := GetProviderCompletions("chatgpt:"+tc.prefix, false, nil)
		want := make([]string, len(tc.want))
		for i, model := range tc.want {
			want[i] = "chatgpt:" + model
		}
		if !equalSlice(got, want) {
			t.Fatalf("completion for %q = %v, want %v", tc.prefix, got, want)
		}
	}

	for _, prefix := range []string{"gpt-reserve-low", "gpt-reserve-low-f", "speed-low"} {
		got := GetProviderCompletions("chatgpt:"+prefix, false, nil)
		want := "chatgpt:" + strings.TrimSuffix(prefix, "-f") + "-fast"
		if !slices.Contains(got, want) {
			t.Fatalf("completion for %q = %v, missing %q", prefix, got, want)
		}
	}
	if got := GetProviderCompletions("chatgpt:slow-low", false, nil); slices.Contains(got, "chatgpt:slow-low-fast") {
		t.Fatalf("unsupported fast completion: %v", got)
	}
	if got := GetProviderCompletions("chatgpt:literal-fast", false, nil); !equalSlice(got, []string{"chatgpt:literal-fast"}) {
		t.Fatalf("literal ID completion duplicated or expanded: %v", got)
	}

	origClient := chatGPTHTTPClient
	t.Cleanup(func() { chatGPTHTTPClient = origClient })
	var captured ResponsesRequest
	chatGPTHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = ResponsesRequest{}
		if err := json.NewDecoder(req.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\ndata: [DONE]\n\n"))}, nil
	})}
	for _, tc := range []struct {
		name, model  string
		req          Request
		tier, effort string
	}{
		{"constructor", "gpt-reserve-low-fast", Request{}, ServiceTierFast, "low"},
		{"request", "gpt-reserve", Request{Model: "gpt-reserve-low-fast"}, ServiceTierFast, "low"},
		{"explicit overrides", "gpt-reserve-low-fast", Request{ReasoningEffort: "max", ServiceTierSet: true}, "", "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewChatGPTProviderWithCreds(&credentials.ChatGPTCredentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}, tc.model)
			tc.req.Messages = []Message{UserText("hi")}
			stream, err := p.Stream(context.Background(), tc.req)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			drainStreamToDone(t, stream)
			if captured.Model != "gpt-reserve" || captured.Reasoning.Effort != tc.effort || captured.ServiceTier != tc.tier {
				t.Fatalf("unexpected request: %+v", captured)
			}
		})
	}
}
