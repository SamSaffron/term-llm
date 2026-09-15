package live

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
)

func testAuth() Auth {
	return Auth{
		AccessToken: "token-123",
		AccountID:   "acct-9",
		Headers:     map[string]string{"originator": "term-llm", "User-Agent": "term-llm/test"},
	}
}

func TestCreateCallSendsCodexRealtimeRequest(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotHeader http.Header
		gotBody   []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Location", "/v1/live/rtc_abc123")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "v=0\r\na=answer\r\n")
	}))
	defer server.Close()

	sessionJSON, err := SessionJSON(config.LiveConfig{}, SessionOptions{
		SessionID:    "sess_1",
		InitialItems: []InitialItem{{Role: RoleUser, Text: "hi"}, {Role: RoleAssistant, Text: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const offer = "v=0\r\na=offer\r\n"
	result, err := CreateCall(context.Background(), server.Client(), server.URL+"/backend-api/codex", testAuth(), "sess_1",
		CallRequest{SDP: offer, Session: sessionJSON})
	if err != nil {
		t.Fatalf("CreateCall: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s", gotMethod)
	}
	if gotPath != "/backend-api/codex/realtime/calls" {
		t.Fatalf("path = %s", gotPath)
	}
	if !strings.Contains(gotQuery, "intent=quicksilver") || !strings.Contains(gotQuery, "architecture=avas") {
		t.Fatalf("query = %s", gotQuery)
	}
	for header, want := range map[string]string{
		// AVAS rejects a call without the frameless protocol selector.
		"OpenAI-Alpha":       "quicksilver=v2",
		"Authorization":      "Bearer token-123",
		"ChatGPT-Account-ID": "acct-9",
		"originator":         "term-llm",
		"User-Agent":         "term-llm/test",
		"X-Session-Id":       "sess_1",
		"Content-Type":       "application/json",
	} {
		if got := gotHeader.Get(header); got != want {
			t.Fatalf("header %s = %q, want %q", header, got, want)
		}
	}

	var body struct {
		SDP     string `json:"sdp"`
		Session struct {
			Model        string `json:"model"`
			Instructions string `json:"instructions"`
			Audio        struct {
				Output struct {
					Voice string `json:"voice"`
				} `json:"output"`
			} `json:"audio"`
			Delegation struct {
				Type      string `json:"type"`
				AckFiller bool   `json:"ack_filler"`
			} `json:"delegation"`
			InitialItems []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"initial_items"`
		} `json:"session"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// SDP is whitespace-significant: the provider rejects an offer whose final
	// CRLF was trimmed with "failed to unmarshal SDP: EOF".
	if body.SDP != offer {
		t.Fatalf("offer = %q, want %q", body.SDP, offer)
	}
	if body.Session.Model != config.DefaultLiveChatGPTModel {
		t.Fatalf("model = %q", body.Session.Model)
	}
	if body.Session.Audio.Output.Voice != config.DefaultLiveChatGPTVoice {
		t.Fatalf("voice = %q", body.Session.Audio.Output.Voice)
	}
	if body.Session.Delegation.Type != "client" || !body.Session.Delegation.AckFiller {
		t.Fatalf("delegation = %+v", body.Session.Delegation)
	}
	if body.Session.Instructions != DefaultInstructions {
		t.Fatal("default instructions were not sent")
	}
	if len(body.Session.InitialItems) != 2 {
		t.Fatalf("initial items = %d", len(body.Session.InitialItems))
	}
	if body.Session.InitialItems[0].Content[0].Type != "input_text" {
		t.Fatalf("user item content type = %q", body.Session.InitialItems[0].Content[0].Type)
	}
	if body.Session.InitialItems[1].Content[0].Type != "output_text" {
		t.Fatalf("assistant item content type = %q", body.Session.InitialItems[1].Content[0].Type)
	}

	if result.CallID != "rtc_abc123" {
		t.Fatalf("call id = %q", result.CallID)
	}
	if result.AnswerSDP != "v=0\r\na=answer\r\n" {
		t.Fatalf("answer = %q", result.AnswerSDP)
	}
}

func TestCreateCallSeparatesStaleTokensFromDeniedAccess(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		want     error
		wantText string
	}{
		// A stale token is recoverable, so the caller refreshes and retries.
		{
			name: "unauthorized", status: http.StatusUnauthorized,
			body: `{"detail":"token expired"}`, want: ErrUnauthorized, wantText: "token expired",
		},
		// Access denial can reflect session settings as well as account policy;
		// it must not be reported as an expired credential.
		{
			name: "forbidden", status: http.StatusForbidden,
			body:     `{"error":{"message":"Voice session access denied.","code":"forbidden"}}`,
			want:     ErrForbidden,
			wantText: "Voice session access denied.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			_, err := CreateCall(context.Background(), server.Client(), server.URL, testAuth(), "sess_1", CallRequest{SDP: "offer"})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if errors.Is(err, ErrForbidden) && errors.Is(err, ErrUnauthorized) {
				t.Fatal("access denial must not also report a credential failure")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %v lost the provider message %q", err, tc.wantText)
			}
		})
	}
}

func TestStartDoesNotRefreshCredentialsWhenAccessIsDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Voice session access denied."}}`)
	}))
	defer server.Close()

	var refreshes int
	cfg := config.LiveConfig{}
	cfg.ChatGPT.CallBaseURL = server.URL
	provider := NewChatGPTProvider(cfg, func(_ context.Context, refresh bool) (Auth, error) {
		if refresh {
			refreshes++
		}
		return testAuth(), nil
	}, server.Client())

	_, err := provider.Start(context.Background(), "v=0\r\n", SessionOptions{SessionID: "sess_1"})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
	// Rotating the OAuth token on every denied click is wasted work and churns
	// the stored refresh token.
	if refreshes != 0 {
		t.Fatalf("forced credential refreshes = %d, want 0", refreshes)
	}
	if !strings.Contains(err.Error(), "Voice session access denied.") {
		t.Fatalf("error text lost the provider message: %v", err)
	}
}

func TestCreateCallRequiresCallID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "v=0\r\n")
	}))
	defer server.Close()

	if _, err := CreateCall(context.Background(), server.Client(), server.URL, testAuth(), "", CallRequest{SDP: "offer"}); err == nil {
		t.Fatal("expected an error when Location is missing")
	}
}

func TestParseCallID(t *testing.T) {
	tests := []struct {
		location string
		want     string
	}{
		{"/v1/live/rtc_abc", "rtc_abc"},
		{"https://api.openai.com/v1/live/rtc_abc/", "rtc_abc"},
		{"/v1/live/7f4d3f0e-1a2b-4c3d-8e9f-0a1b2c3d4e5f", "7f4d3f0e-1a2b-4c3d-8e9f-0a1b2c3d4e5f"},
		{"/v1/live/", ""},
		{"", ""},
		{"/v1/live/not-a-call", ""},
	}
	for _, tc := range tests {
		if got := ParseCallID(tc.location); got != tc.want {
			t.Fatalf("ParseCallID(%q) = %q, want %q", tc.location, got, tc.want)
		}
	}
}

func TestSessionJSONHonoursConfiguredModelVoiceAndContext(t *testing.T) {
	cfg := config.LiveConfig{Instructions: "be brief"}
	cfg.ChatGPT.Model = "gpt-live-test"
	cfg.ChatGPT.Voice = "maple"
	payload, err := SessionJSON(cfg, SessionOptions{Context: "authoritative host facts"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["model"] != "gpt-live-test" {
		t.Fatalf("model = %v", decoded["model"])
	}
	if decoded["instructions"] != "be brief\n\nauthoritative host facts" {
		t.Fatalf("instructions = %v", decoded["instructions"])
	}
	audio, _ := decoded["audio"].(map[string]any)
	output, _ := audio["output"].(map[string]any)
	if output["voice"] != "maple" {
		t.Fatalf("voice = %v", output["voice"])
	}
}

func TestSidebandURL(t *testing.T) {
	tests := []struct {
		base, callID, want string
		wantErr            bool
	}{
		{base: "https://api.openai.com/v1", callID: "rtc_1", want: "wss://api.openai.com/v1/live/rtc_1"},
		{base: "https://api.openai.com/v1/live", callID: "rtc_1", want: "wss://api.openai.com/v1/live/rtc_1"},
		{base: "http://127.0.0.1:8080/v1", callID: "rtc_1", want: "ws://127.0.0.1:8080/v1/live/rtc_1"},
		{base: "", callID: "rtc_1", wantErr: true},
		{base: "https://api.openai.com/v1", callID: "", wantErr: true},
	}
	for _, tc := range tests {
		got, err := sidebandURL(tc.base, tc.callID)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("sidebandURL(%q, %q) expected an error", tc.base, tc.callID)
			}
			continue
		}
		if err != nil {
			t.Fatalf("sidebandURL(%q, %q): %v", tc.base, tc.callID, err)
		}
		if got != tc.want {
			t.Fatalf("sidebandURL(%q, %q) = %q, want %q", tc.base, tc.callID, got, tc.want)
		}
	}
}
