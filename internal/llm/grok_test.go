package llm

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/oauth"
)

func validGrokUUIDv4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' || value[14] != '4' {
		return false
	}
	variant := value[19]
	if variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil
}

func grokSSE(body ...string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strings.Join(body, "\n") + "\n"))}
}

type fakeGrokOAuthAPI struct {
	device      *oauth.GrokDeviceCode
	token       *oauth.GrokTokenResponse
	accountID   string
	refreshErr  error
	userInfoErr error
	revokeErr   error
	revoked     string
}

func (f *fakeGrokOAuthAPI) RequestDeviceCode(context.Context) (*oauth.GrokDeviceCode, error) {
	if f.device == nil {
		return nil, errors.New("unexpected device request")
	}
	return f.device, nil
}

func (f *fakeGrokOAuthAPI) PollDeviceToken(context.Context, *oauth.GrokDeviceCode) (*oauth.GrokTokenResponse, error) {
	if f.token == nil {
		return nil, errors.New("unexpected token poll")
	}
	return f.token, nil
}

func (f *fakeGrokOAuthAPI) RefreshToken(context.Context, string) (*oauth.GrokTokenResponse, error) {
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	if f.token == nil {
		return nil, errors.New("unexpected refresh")
	}
	return f.token, nil
}

func (f *fakeGrokOAuthAPI) UserInfo(context.Context, string) (string, error) {
	return f.accountID, f.userInfoErr
}

func (f *fakeGrokOAuthAPI) Revoke(_ context.Context, token string) error {
	f.revoked = token
	return f.revokeErr
}

func isolateGrokLLMTestEnv(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", root+"/config")
	t.Setenv("XDG_DATA_HOME", root+"/data")
	t.Setenv("XDG_CACHE_HOME", root+"/cache")
}

func TestConfiguredGrokAliasUsesNativeProvider(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	if err := credentials.SaveGrokCredentials(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}); err != nil {
		t.Fatal(err)
	}
	provider, err := createProviderFromConfig("personal-grok", &config.ProviderConfig{Type: config.ProviderTypeGrok, Model: "grok-4.6"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*GrokProvider); !ok {
		t.Fatalf("configured type grok created %T", provider)
	}
}

func TestGrokConfiguredAPIFieldsRequireExplicitProviderType(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	for _, cfg := range []config.ProviderConfig{
		{APIKey: "legacy-key", Model: "grok-4"},
		{BaseURL: "https://api.x.ai/v1", Model: "grok-4"},
	} {
		_, err := createProviderFromConfig("grok", &cfg)
		if err == nil || !strings.Contains(err.Error(), "type: xai") || !strings.Contains(err.Error(), "type: openai_compatible") {
			t.Fatalf("compatibility error = %v", err)
		}
	}
}

func TestPromptForGrokAuthUsesInjectedClientAndRejectsNonInteractive(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient, oldInteractive, oldOutput, oldContext := grokOAuthClient, grokInteractiveTerminal, grokAuthOutput, grokAuthContext
	t.Cleanup(func() {
		grokOAuthClient, grokInteractiveTerminal, grokAuthOutput, grokAuthContext = oldClient, oldInteractive, oldOutput, oldContext
	})
	grokInteractiveTerminal = func() bool { return false }
	if _, err := PromptForGrokAuth(); err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("noninteractive error = %v", err)
	}

	var output bytes.Buffer
	grokInteractiveTerminal = func() bool { return true }
	grokAuthOutput = &output
	grokAuthContext = func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }
	grokOAuthClient = &fakeGrokOAuthAPI{device: &oauth.GrokDeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://accounts.x.ai.evil.test/device", ExpiresIn: 600, Interval: 5}}
	if _, err := PromptForGrokAuth(); err == nil || !strings.Contains(err.Error(), "verification URL host") {
		t.Fatalf("unsafe verification URL error = %v", err)
	}
	if strings.Contains(output.String(), "evil.test") {
		t.Fatalf("unsafe URL was displayed: %q", output.String())
	}
	output.Reset()

	fake := &fakeGrokOAuthAPI{
		device:    &oauth.GrokDeviceCode{DeviceCode: "device", UserCode: "ABCD", VerificationURI: "https://accounts.x.ai/device", VerificationURIComplete: "https://accounts.x.ai/device?code=ABCD", ExpiresIn: 600, Interval: 5},
		token:     &oauth.GrokTokenResponse{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: 3600},
		accountID: "acct_1",
	}
	grokOAuthClient = fake
	creds, err := PromptForGrokAuth()
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccountID != "acct_1" || creds.AccessToken != "access" || !strings.Contains(output.String(), "https://accounts.x.ai/device") {
		t.Fatalf("credentials=%+v output=%q", creds, output.String())
	}
	stored, err := credentials.GetGrokCredentials()
	if err != nil || stored.RefreshToken != "refresh" {
		t.Fatalf("stored credentials=%+v err=%v", stored, err)
	}
}

func TestLogoutGrokRevocationFailureStillDeletesLocalCredentials(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	if err := credentials.SaveGrokCredentials(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}); err != nil {
		t.Fatal(err)
	}
	if err := saveGrokModelsCache(grokModelsCache{AccountID: "acct_1", FetchedAt: time.Now(), Models: []ModelInfo{{ID: "grok-4.6"}}}); err != nil {
		t.Fatal(err)
	}
	oldClient := grokOAuthClient
	fake := &fakeGrokOAuthAPI{revokeErr: errors.New("offline")}
	grokOAuthClient = fake
	t.Cleanup(func() { grokOAuthClient = oldClient })
	warning, err := LogoutGrok(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fake.revoked != "refresh" || !strings.Contains(warning, "revocation failed") || credentials.GrokCredentialsExist() {
		t.Fatalf("revoked=%q warning=%q credentialsExist=%v", fake.revoked, warning, credentials.GrokCredentialsExist())
	}
	cachePath, err := grokModelsCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) {
		t.Fatalf("Grok model cache still exists after logout: %v", err)
	}
}

func TestGrokProvider401InvalidRefreshConditionallyClearsCredentialsWithGuidance(t *testing.T) {
	for _, tc := range []struct {
		name          string
		storedRefresh string
		wantExists    bool
	}{
		{name: "matching stale generation", storedRefresh: "stale-refresh"},
		{name: "concurrent login preserved", storedRefresh: "new-login-refresh", wantExists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateGrokLLMTestEnv(t)
			stored := &credentials.GrokCredentials{AccessToken: "stale-access", RefreshToken: tc.storedRefresh, ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
			if err := credentials.SaveGrokCredentials(stored); err != nil {
				t.Fatal(err)
			}
			oldHTTP, oldOAuth := grokHTTPClient, grokOAuthClient
			grokHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`))}, nil
			})}
			grokOAuthClient = &fakeGrokOAuthAPI{refreshErr: oauth.ErrGrokRefreshTokenInvalid}
			t.Cleanup(func() { grokHTTPClient, grokOAuthClient = oldHTTP, oldOAuth })

			provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6")
			_, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
			if err == nil || !strings.Contains(err.Error(), "auth login grok") || !errors.Is(err, oauth.ErrGrokRefreshTokenInvalid) {
				t.Fatalf("401 recovery error = %v", err)
			}
			if got := credentials.GrokCredentialsExist(); got != tc.wantExists {
				t.Fatalf("credentials exist = %v, want %v", got, tc.wantExists)
			}
		})
	}
}

func TestNewGrokProviderPreflightInvalidRefreshClearsCredentials(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	if err := credentials.SaveGrokCredentials(&credentials.GrokCredentials{AccessToken: "expired", RefreshToken: "invalid-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "acct_1"}); err != nil {
		t.Fatal(err)
	}
	oldClient, oldInteractive := grokOAuthClient, grokInteractiveTerminal
	grokOAuthClient = &fakeGrokOAuthAPI{refreshErr: oauth.ErrGrokRefreshTokenInvalid}
	grokInteractiveTerminal = func() bool { return false }
	t.Cleanup(func() { grokOAuthClient, grokInteractiveTerminal = oldClient, oldInteractive })
	_, err := NewGrokProvider("grok-4.6")
	if err == nil || !errors.Is(err, oauth.ErrGrokRefreshTokenInvalid) || !strings.Contains(err.Error(), "auth login grok") {
		t.Fatalf("preflight error = %v", err)
	}
	if credentials.GrokCredentialsExist() {
		t.Fatal("invalid preflight refresh did not clear stale credentials")
	}
}

func TestGrokCachedOutputLimitClampsRequest(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	if err := credentials.SaveGrokCredentials(creds); err != nil {
		t.Fatal(err)
	}
	if err := saveGrokModelsCache(grokModelsCache{AccountID: "acct_1", FetchedAt: time.Now(), Models: []ModelInfo{{ID: "grok-4.6", InputLimit: 500_000, OutputLimit: 8_000}}}); err != nil {
		t.Fatal(err)
	}
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()
	var maxOutput float64
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		maxOutput, _ = payload["max_output_tokens"].(float64)
		return grokSSE(`event: response.completed`, `data: {"type":"response.completed","response":{}}`, `data: [DONE]`), nil
	})}
	provider := NewGrokProviderWithCreds(creds, "grok-4.6")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}, MaxOutputTokens: 20_000})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	if maxOutput != 8_000 {
		t.Fatalf("max_output_tokens = %v, want 8000", maxOutput)
	}
}

func TestGrokProviderOmitsToolControlsWithoutTools(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()

	var payload map[string]any
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return grokSSE(`event: response.completed`, `data: {"type":"response.completed","response":{}}`, `data: [DONE]`), nil
	})}

	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	provider := NewGrokProviderWithCreds(creds, "grok-4.6")
	stream, err := provider.Stream(context.Background(), Request{
		Messages:   []Message{UserText("hi")},
		ToolChoice: ToolChoice{Mode: ToolChoiceAuto},
	})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)

	for _, field := range []string{"tool_choice", "parallel_tool_calls"} {
		if value, ok := payload[field]; ok {
			t.Errorf("payload includes %s=%v without tools: %v", field, value, payload)
		}
	}
}

func TestGrokProviderExactEndpointHeadersAndResponsesFeatures(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()
	var payload map[string]any
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != grokResponsesURL {
			t.Fatalf("URL = %s, want %s", req.URL, grokResponsesURL)
		}
		wantHeaders := map[string]string{
			"Accept":                   "text/event-stream",
			"Authorization":            "Bearer access",
			"X-XAI-Token-Auth":         "xai-grok-cli",
			"x-authenticateresponse":   "authenticate-response",
			"x-grok-client-mode":       "headless",
			"x-grok-client-version":    "1.0.6",
			"x-grok-client-identifier": "term-llm",
			"x-grok-model-override":    "grok-4.6",
			"x-grok-user-id":           "acct_1",
			"x-grok-conv-id":           "session_1",
			"x-grok-session-id":        "session_1",
			"User-Agent":               "term-llm",
		}
		for name, want := range wantHeaders {
			if got := req.Header.Get(name); got != want {
				t.Fatalf("header %s = %q, want %q", name, got, want)
			}
		}
		if requestID := req.Header.Get("x-grok-req-id"); !validGrokUUIDv4(requestID) {
			t.Fatalf("x-grok-req-id = %q, want UUIDv4", requestID)
		}
		for _, name := range []string{"x-grok-agent-id", "x-grok-turn-idx"} {
			if got := req.Header.Get(name); got != "" {
				t.Fatalf("unexpected %s header %q", name, got)
			}
		}
		if req.Header.Get("session_id") != "" {
			t.Fatalf("unexpected generic session_id header %q", req.Header.Get("session_id"))
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return grokSSE(
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[]}}`,
			`event: response.reasoning_summary_text.delta`,
			`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"thinking"}`,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[{"type":"summary_text","text":"thinking"}]}}`,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			`event: response.output_item.added`,
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"shell"}}`,
			`event: response.function_call_arguments.delta`,
			`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"command\":\"pwd\"}"}`,
			`event: response.output_item.done`,
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":\"pwd\"}"}}`,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14}}}`,
			`data: [DONE]`,
		), nil
	})}
	provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6-high")
	provider.responsesClient = newGrokResponsesClient(provider.creds)
	provider.responsesClient.LastResponseID = "must-not-send"
	stream, err := provider.Stream(context.Background(), Request{
		SessionID: "session_1",
		Messages: []Message{{Role: RoleUser, Parts: []Part{
			{Type: PartText, Text: "look"},
			{Type: PartImage, ImageData: &ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
		}}},
		Tools:           []ToolSpec{{Name: "shell", Description: "run", Schema: map[string]any{"type": "object"}}},
		MaxOutputTokens: 123,
		Temperature:     0.7,
		TemperatureSet:  true,
		TopP:            0.8,
		TopPSet:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	seen := map[EventType]bool{}
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		seen[event.Type] = true
		if event.Type == EventError {
			t.Fatal(event.Err)
		}
		if event.Type == EventDone {
			break
		}
	}
	for _, typ := range []EventType{EventReasoningDelta, EventTextDelta, EventToolCall, EventUsage, EventProviderReplay, EventDone} {
		if !seen[typ] {
			t.Errorf("missing event type %s", typ)
		}
	}
	if payload["store"] != false || payload["max_output_tokens"].(float64) != 123 {
		t.Fatalf("payload controls = %v", payload)
	}
	if payload["parallel_tool_calls"] != true || payload["tool_choice"] != "auto" {
		t.Fatalf("tool protocol controls = %v", payload)
	}
	text, _ := payload["text"].(map[string]any)
	if text["verbosity"] != "low" {
		t.Fatalf("text controls = %v", payload)
	}
	if payload["instructions"] != grokDefaultInstructions {
		t.Fatalf("default instructions = %q, want %q", payload["instructions"], grokDefaultInstructions)
	}
	if payload["prompt_cache_key"] != "session_1" {
		t.Fatalf("prompt_cache_key = %v, want session_1", payload["prompt_cache_key"])
	}
	if got := payload["temperature"]; got != float64(float32(0.7)) {
		t.Fatalf("temperature = %v, want %v", got, float64(float32(0.7)))
	}
	if got := payload["top_p"]; got != float64(float32(0.8)) {
		t.Fatalf("top_p = %v, want %v", got, float64(float32(0.8)))
	}
	if _, ok := payload["previous_response_id"]; ok {
		t.Fatalf("previous_response_id sent: %v", payload)
	}
	encoded, _ := json.Marshal(payload)
	for _, want := range []string{"input_image", "data:image/png;base64,aGVsbG8=", "reasoning.encrypted_content", `"shell"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("request missing %q: %s", want, encoded)
		}
	}
}

func TestGrokProviderReplaysEncryptedReasoningThroughToolLoopWithoutServerState(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()

	var payloads []map[string]any
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
		switch len(payloads) {
		case 1:
			return grokSSE(
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[]}}`,
				`event: response.output_item.done`,
				// Completed provider items already carry reasoning_text. Opaque replay
				// must preserve that valid discriminator byte-for-byte.
				`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[],"content":[{"type":"reasoning_text","text":"private reasoning"}]}}`,
				`event: response.output_item.added`,
				`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"count_tool"}}`,
				`event: response.function_call_arguments.delta`,
				`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
				`event: response.output_item.done`,
				`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_1","name":"count_tool","arguments":"{}"}}`,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`,
				`data: [DONE]`,
			), nil
		case 2:
			return grokSSE(
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","delta":"done"}`,
				`event: response.completed`,
				`data: {"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":8,"output_tokens":1,"total_tokens":9}}}`,
				`data: [DONE]`,
			), nil
		default:
			t.Fatalf("unexpected Grok request %d", len(payloads))
			return nil, nil
		}
	})}

	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	provider := NewGrokProviderWithCreds(creds, "grok-4.6")
	provider.responsesClient = newGrokResponsesClient(creds)
	provider.responsesClient.LastResponseID = "must-not-send"
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)

	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("use the tool")},
		Tools:    []ToolSpec{tool.Spec()},
		MaxTurns: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := drainStreamErr(t, stream); err != nil {
		t.Fatalf("tool loop stream: %v", err)
	}
	if tool.calls.Load() != 1 {
		t.Fatalf("tool executions = %d, want 1", tool.calls.Load())
	}
	if len(payloads) != 2 {
		t.Fatalf("Grok requests = %d, want 2", len(payloads))
	}
	for i, payload := range payloads {
		if payload["store"] != false {
			t.Fatalf("request %d store = %v, want false", i+1, payload["store"])
		}
		if _, ok := payload["previous_response_id"]; ok {
			t.Fatalf("request %d sent previous_response_id: %v", i+1, payload)
		}
	}

	input, ok := payloads[1]["input"].([]any)
	if !ok {
		t.Fatalf("second request input = %#v", payloads[1]["input"])
	}
	var reasoning, functionCall, functionOutput map[string]any
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "reasoning":
			reasoning = item
		case "function_call":
			functionCall = item
		case "function_call_output":
			functionOutput = item
		}
	}
	if reasoning == nil || reasoning["id"] != "rs_1" || reasoning["encrypted_content"] != "enc_1" {
		t.Fatalf("reasoning replay item = %#v", reasoning)
	}
	content, ok := reasoning["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("reasoning replay content = %#v", reasoning["content"])
	}
	reasoningText, ok := content[0].(map[string]any)
	if !ok || reasoningText["type"] != "reasoning_text" || reasoningText["text"] != "private reasoning" {
		t.Fatalf("reasoning replay content item = %#v", content[0])
	}
	if functionCall == nil || functionCall["call_id"] != "call_1" || functionCall["name"] != "count_tool" || functionCall["arguments"] != "{}" {
		t.Fatalf("function call replay item = %#v", functionCall)
	}
	if functionOutput == nil || functionOutput["call_id"] != "call_1" || functionOutput["output"] != "ok" {
		t.Fatalf("function_call_output = %#v", functionOutput)
	}
}

func TestGrokProvider401ReplaysOnceWithConcurrentFreshCredentials(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	fresh := &credentials.GrokCredentials{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	if err := credentials.SaveGrokCredentials(fresh); err != nil {
		t.Fatal(err)
	}
	oldClient := grokHTTPClient
	defer func() { grokHTTPClient = oldClient }()
	var auth []string
	var requestIDs []string
	var bodies [][]byte
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		auth = append(auth, req.Header.Get("Authorization"))
		requestIDs = append(requestIDs, req.Header.Get("x-grok-req-id"))
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		if len(auth) == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"expired"}`))}, nil
		}
		return grokSSE(`event: response.completed`, `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`, `data: [DONE]`), nil
	})}
	provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6")
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	stream, err = provider.Stream(context.Background(), Request{Messages: []Message{UserText("second call")}})
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, stream)
	if len(auth) != 3 || auth[0] != "Bearer stale-access" || auth[1] != "Bearer fresh-access" || auth[2] != "Bearer fresh-access" {
		t.Fatalf("authorization attempts = %v", auth)
	}
	if !validGrokUUIDv4(requestIDs[0]) || requestIDs[0] != requestIDs[1] {
		t.Fatalf("auth retry request IDs = %v, want one preserved UUIDv4", requestIDs[:2])
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("auth retry body changed:\nfirst:  %s\nsecond: %s", bodies[0], bodies[1])
	}
	if !validGrokUUIDv4(requestIDs[2]) || requestIDs[2] == requestIDs[0] {
		t.Fatalf("logical call request IDs = %v, want unique UUIDv4 values", requestIDs)
	}
}

func TestGrokEncryptedContent400IsNotRetriedByOuterProvider(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	oldClient := grokHTTPClient
	t.Cleanup(func() { grokHTTPClient = oldClient })

	calls := 0
	grokHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"message":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}}`,
			)),
		}, nil
	})}
	creds := &credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}
	provider := WrapWithRetry(NewGrokProviderWithCreds(creds, "grok-4.6"), RetryConfig{
		MaxAttempts: 3,
		BaseBackoff: time.Nanosecond,
		MaxBackoff:  time.Nanosecond,
	})
	stream, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("continue")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	streamErr := drainStreamErr(t, stream)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "encrypted_content") {
		t.Fatalf("stream error = %v", streamErr)
	}
	if calls != 1 {
		t.Fatalf("requests = %d, want 1 for non-transient HTTP 400", calls)
	}
}

func TestGrokProvider403DoesNotRefreshAnd429IsTyped(t *testing.T) {
	isolateGrokLLMTestEnv(t)
	for _, tc := range []struct {
		status int
		check  func(error) bool
	}{{http.StatusForbidden, func(err error) bool {
		return strings.Contains(err.Error(), "subscription") && strings.Contains(err.Error(), "entitlement")
	}}, {http.StatusTooManyRequests, func(err error) bool {
		var rate *RateLimitError
		return errors.As(err, &rate) && rate.RetryAfter == 2*time.Second
	}}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			oldClient := grokHTTPClient
			defer func() { grokHTTPClient = oldClient }()
			calls := 0
			grokHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"2"}}, Body: io.NopCloser(strings.NewReader(`{"error":"denied"}`))}, nil
			})}
			provider := NewGrokProviderWithCreds(&credentials.GrokCredentials{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: "acct_1"}, "grok-4.6")
			_, err := provider.Stream(context.Background(), Request{Messages: []Message{UserText("hello")}})
			if err == nil || !tc.check(err) {
				t.Fatalf("error = %T %v", err, err)
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want 1", calls)
			}
		})
	}
}
