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

func TestOpenAIRealtimeSessionJSONUsesGARealtimeShapeAndDelegationTool(t *testing.T) {
	cfg := config.LiveConfig{Instructions: "configured"}
	cfg.OpenAI.Model = "gpt-realtime-2.1"
	payload, err := openAIRealtimeSessionJSON(cfg, SessionOptions{Context: "host facts"})
	if err != nil {
		t.Fatal(err)
	}
	var got openAIRealtimeSessionPayload
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "realtime" || got.Model != "gpt-realtime-2.1" {
		t.Fatalf("session identity = %q/%q", got.Type, got.Model)
	}
	if got.Audio.Output.Voice != cfg.OpenAI.ResolvedVoice() {
		t.Fatalf("voice = %q", got.Audio.Output.Voice)
	}
	if got.Audio.Input.Transcription.Model != "gpt-4o-mini-transcribe" {
		t.Fatalf("transcription = %q", got.Audio.Input.Transcription.Model)
	}
	if len(got.OutputModalities) != 1 || got.OutputModalities[0] != "audio" {
		t.Fatalf("output modalities = %v", got.OutputModalities)
	}
	if got.Instructions != "configured\n\nhost facts" {
		t.Fatalf("instructions = %q", got.Instructions)
	}
	if len(got.Tools) != 1 || got.Tools[0].Type != "function" || got.Tools[0].Name != openAIDelegationTool || got.ToolChoice != "auto" {
		t.Fatalf("delegation tool = %+v, choice = %q", got.Tools, got.ToolChoice)
	}
}

func TestOpenAILiveSessionPayloadUsesClientDelegationAndStartupHistory(t *testing.T) {
	cfg := config.LiveConfig{Instructions: "configured"}
	payload, err := openAILiveSessionPayloadFor(cfg, SessionOptions{
		Context: "host facts",
		InitialItems: []InitialItem{
			{Role: RoleUser, Text: " question "},
			{Role: RoleAssistant, Text: "answer"},
			{Role: "developer", Text: "trusted fact"},
			{Role: "system", Text: "older trusted fact"},
			{Role: "unknown", Text: "fallback user"},
			{Role: RoleUser, Text: "   "},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.Model != config.DefaultLiveOpenAIModel || payload.Audio.Output.Voice != cfg.OpenAI.ResolvedVoice() {
		t.Fatalf("identity = model %q, voice %q", payload.Model, payload.Audio.Output.Voice)
	}
	if payload.Instructions != "configured\n\nhost facts" || payload.Delegation.Type != "client" {
		t.Fatalf("session = %+v", payload)
	}
	want := []struct {
		role, contentType, text string
	}{
		{RoleUser, "input_text", "question"},
		{RoleAssistant, "output_text", "answer"},
		{"developer", "input_text", "trusted fact"},
		{"developer", "input_text", "older trusted fact"},
		{RoleUser, "input_text", "fallback user"},
	}
	if len(payload.Input) != len(want) {
		t.Fatalf("input = %+v", payload.Input)
	}
	for i, item := range payload.Input {
		if item.Type != "message" || item.Role != want[i].role || len(item.Content) != 1 || item.Content[0].Type != want[i].contentType || item.Content[0].Text != want[i].text {
			t.Fatalf("input[%d] = %+v, want %+v", i, item, want[i])
		}
	}

	tooMany := make([]InitialItem, 129)
	for i := range tooMany {
		tooMany[i] = InitialItem{Role: RoleUser, Text: "x"}
	}
	if _, err := openAILiveSessionPayloadFor(cfg, SessionOptions{InitialItems: tooMany}); err == nil || !strings.Contains(err.Error(), "maximum is 128") {
		t.Fatalf("history limit error = %v", err)
	}
}

func TestCreateOpenAILiveCallSendsDocumentedJSONAndReadsResponse(t *testing.T) {
	const (
		offer  = "v=0\r\na=offer\r\n"
		answer = "v=0\r\na=answer\r\n"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/live/sessions" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		if r.Header.Get("OpenAI-Beta") != "" || r.Header.Get("OpenAI-Alpha") != "" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Errorf("public request carried proprietary headers: %v", r.Header)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if len(body) != 2 || body["session"] == nil || body["transport"] == nil {
			t.Errorf("top-level body = %v", body)
		}
		var transport openAILiveTransport
		if err := json.Unmarshal(body["transport"], &transport); err != nil {
			t.Errorf("decode transport: %v", err)
		} else if transport.Type != "webrtc" || transport.SDP != offer {
			t.Errorf("transport = %+v", transport)
		}
		var session map[string]json.RawMessage
		if err := json.Unmarshal(body["session"], &session); err != nil {
			t.Errorf("decode session: %v", err)
		} else if session["tools"] != nil || session["tool_choice"] != nil || session["output_modalities"] != nil || session["type"] != nil {
			t.Errorf("GPT-Live session carried Realtime fields: %s", body["session"])
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"session":{"id":"live_public123"},"transport":{"type":"webrtc","sdp":"v=0\r\na=answer\r\n"}}`)
	}))
	defer server.Close()

	payload, err := openAILiveSessionPayloadFor(config.LiveConfig{}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := createOpenAILiveCall(context.Background(), server.Client(), server.URL+"/v1", "sk-test", offer, payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.CallID != "live_public123" || got.AnswerSDP != answer {
		t.Fatalf("call response = %+v", got)
	}
}

func TestCreateOpenAIRealtimeCallSendsMultipartAndPreservesSDP(t *testing.T) {
	const offer = "v=0\r\na=offer\r\n"
	const answer = "v=0\r\na=answer\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/realtime/calls" {
			t.Errorf("request = %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q", got)
		}
		if r.Header.Get("OpenAI-Beta") != "" || r.Header.Get("OpenAI-Alpha") != "" {
			t.Errorf("GA request carried beta/proprietary headers: %v", r.Header)
		}
		if err := r.ParseMultipartForm(64 << 10); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		if got := r.FormValue("sdp"); got != offer {
			t.Errorf("offer = %q, want %q", got, offer)
		}
		var session openAIRealtimeSessionPayload
		if err := json.Unmarshal([]byte(r.FormValue("session")), &session); err != nil {
			t.Errorf("decode session: %v", err)
		} else if session.Type != "realtime" || session.Tools[0].Name != openAIDelegationTool {
			t.Errorf("session = %+v", session)
		}
		w.Header().Set("Location", "/v1/realtime/calls/rtc_public123")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()

	cfg := config.LiveConfig{}
	cfg.OpenAI.Model = "gpt-realtime-2.1"
	session, err := openAIRealtimeSessionJSON(cfg, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := createOpenAICall(context.Background(), server.Client(), server.URL+"/v1", "sk-test", offer, session)
	if err != nil {
		t.Fatal(err)
	}
	if got.CallID != "rtc_public123" || got.AnswerSDP != answer {
		t.Fatalf("call response = %+v", got)
	}
}

func TestCreateOpenAILiveCallFailurePaths(t *testing.T) {
	tests := []struct {
		name, body, wantText string
		status               int
		wantIs               error
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"message":"bad API key"}}`, wantIs: ErrUnauthorized, wantText: "bad API key"},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"code":"access_denied"}}`, wantIs: ErrForbidden, wantText: "access_denied"},
		{name: "rate limited", status: http.StatusTooManyRequests, body: "slow down", wantText: "http 429: slow down"},
		{name: "bad json", status: http.StatusCreated, body: `{`, wantText: "decode OpenAI GPT-Live"},
		{name: "missing session", status: http.StatusCreated, body: `{"transport":{"type":"webrtc","sdp":"answer"}}`, wantText: "missing session.id"},
		{name: "wrong transport", status: http.StatusCreated, body: `{"session":{"id":"live_1"},"transport":{"type":"sip","sdp":"answer"}}`, wantText: "unexpected transport type"},
		{name: "empty answer", status: http.StatusCreated, body: `{"session":{"id":"live_1"},"transport":{"type":"webrtc","sdp":""}}`, wantText: "empty transport.sdp"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			_, err := createOpenAILiveCall(context.Background(), server.Client(), server.URL, "key", "v=0\r\n", openAILiveSessionPayload{})
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error = %v, want %q", err, test.wantText)
			}
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Fatalf("error = %v, want errors.Is %v", err, test.wantIs)
			}
		})
	}
}

func TestOpenAIURLRejectsInvalidBases(t *testing.T) {
	for _, base := range []string{"", "api.openai.com/v1", "ftp://api.openai.com/v1", "://bad"} {
		if _, err := openAIURL(base, "/live/sessions"); err == nil {
			t.Fatalf("base %q was accepted", base)
		}
	}
}
