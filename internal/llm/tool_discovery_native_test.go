package llm

import (
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/credentials"
)

func TestChatGPTNativeToolDiscoveryCapabilityIsExact(t *testing.T) {
	creds := &credentials.ChatGPTCredentials{AccessToken: "test", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	for _, tc := range []struct {
		name      string
		model     string
		websocket bool
		want      bool
	}{
		{name: "luna websocket", model: "gpt-5.6-luna-medium", websocket: true, want: true},
		{name: "luna HTTP", model: "gpt-5.6-luna-medium", websocket: false},
		{name: "6.1 sol websocket", model: "gpt-6.1-sol-medium", websocket: true, want: true},
		{name: "6 luna websocket", model: "gpt-6-luna-high", websocket: true, want: true},
		{name: "6 astra websocket", model: "gpt-6-astra-xhigh", websocket: true, want: true},
		{name: "6 astra HTTP", model: "gpt-6-astra", websocket: false},
		{name: "5.6 sol websocket", model: "gpt-5.6-sol-medium", websocket: true},
		{name: "6 sol websocket (unverified)", model: "gpt-6-sol", websocket: true},
		{name: "older websocket", model: "gpt-5.5-medium", websocket: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := NewChatGPTProviderWithCredsAndOptions(creds, tc.model, ChatGPTProviderOptions{UseWebSocket: tc.websocket})
			if got := provider.NativeToolDiscoverySupport(tc.model).Supported; got != tc.want {
				t.Fatalf("supported = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNamespaceMetadataDoesNotChangePortableFunctionName(t *testing.T) {
	spec := ToolSpec{
		Name:      "federation__lookup",
		Schema:    map[string]any{"type": "object"},
		Namespace: &ToolNamespaceIdentity{Name: "federation", ChildName: "lookup"},
	}
	compat, err := buildCompatTools([]ToolSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	if len(compat) != 1 || compat[0].Function.Name != "federation__lookup" {
		t.Fatalf("portable tools = %#v", compat)
	}
	responses := BuildResponsesTools([]ToolSpec{spec})
	if len(responses) != 1 || responses[0].(ResponsesTool).Name != "federation__lookup" {
		t.Fatalf("ordinary Responses tools = %#v", responses)
	}
}
