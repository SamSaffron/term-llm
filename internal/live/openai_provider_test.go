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
	"time"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestOpenAIProviderGPTLiveEndToEnd(t *testing.T) {
	const (
		offer  = "v=0\r\na=offer\r\n"
		answer = "v=0\r\na=answer\r\n"
	)
	type callCapture struct {
		authorization string
		contentType   string
		body          openAILiveCreateRequest
	}
	calls := make(chan callCapture, 1)
	handshakes := make(chan *http.Request, 1)
	clientMessages := make(chan []byte, 16)
	serverConn := make(chan *websocket.Conn, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/live/sessions":
			var body openAILiveCreateRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode Live session: %v", err)
			}
			calls <- callCapture{authorization: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), body: body}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(openAILiveCallResponse{
				Session: struct {
					ID string `json:"id"`
				}{ID: "live_e2e"},
				Transport: openAILiveTransport{Type: "webrtc", SDP: answer},
			})
		case "/v1/live/sessions/live_e2e/attach":
			handshakes <- r.Clone(r.Context())
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade sideband: %v", err)
				return
			}
			serverConn <- conn
			defer conn.Close()
			for {
				_, payload, err := conn.ReadMessage()
				if err != nil {
					return
				}
				clientMessages <- payload
				var event openAIClientEvent
				if json.Unmarshal(payload, &event) == nil && event.Type == "session.close" {
					_ = conn.WriteJSON(map[string]any{"type": "session.closed", "reason": "close_requested"})
					return
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := config.LiveConfig{Provider: config.LiveProviderOpenAI}
	cfg.OpenAI.APIKey = "sk-configured"
	cfg.OpenAI.BaseURL = server.URL + "/v1"
	provider, err := NewProviderWithClient(cfg, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*OpenAIProvider); !ok || provider.Name() != config.LiveProviderOpenAI {
		t.Fatalf("provider = %T/%q", provider, provider.Name())
	}

	session, err := provider.Start(context.Background(), offer, SessionOptions{
		Context: "authoritative context",
		InitialItems: []InitialItem{
			{Role: RoleUser, Text: " earlier question "},
			{Role: RoleAssistant, Text: "earlier answer"},
			{Role: "developer", Text: "repository fact"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	liveSession, ok := session.(*openAILiveSession)
	if !ok {
		t.Fatalf("session = %T, want *openAILiveSession", session)
	}
	if _, ok := session.(DelegationCompletionSession); ok {
		t.Fatal("GPT-Live session exposed the Realtime completion contract")
	}
	if session.AnswerSDP() != answer {
		t.Fatalf("answer = %q", session.AnswerSDP())
	}

	capturedCall := <-calls
	if capturedCall.authorization != "Bearer sk-configured" || capturedCall.contentType != "application/json" {
		t.Fatalf("call = %+v", capturedCall)
	}
	if capturedCall.body.Transport.Type != "webrtc" || capturedCall.body.Transport.SDP != offer {
		t.Fatalf("transport = %+v", capturedCall.body.Transport)
	}
	if capturedCall.body.Session.Model != config.DefaultLiveOpenAIModel || capturedCall.body.Session.Delegation.Type != "client" || capturedCall.body.Session.Audio.Output.Voice != cfg.OpenAI.ResolvedVoice() {
		t.Fatalf("session config = %+v", capturedCall.body.Session)
	}
	if !strings.Contains(capturedCall.body.Session.Instructions, "authoritative context") || len(capturedCall.body.Session.Input) != 3 {
		t.Fatalf("session config missing instructions/history: %+v", capturedCall.body.Session)
	}

	handshake := <-handshakes
	if handshake.URL.Path != "/v1/live/sessions/live_e2e/attach" || handshake.URL.RawQuery != "" || handshake.Header.Get("Authorization") != "Bearer sk-configured" {
		t.Fatalf("handshake = %s %v", handshake.URL.String(), handshake.Header)
	}
	if handshake.Header.Get("OpenAI-Alpha") != "" || handshake.Header.Get("OpenAI-Beta") != "" || handshake.Header.Get("ChatGPT-Account-ID") != "" {
		t.Fatalf("GPT-Live sideband carried proprietary headers: %v", handshake.Header)
	}

	conn := <-serverConn
	writeOpenAIEvent(t, conn, map[string]any{
		"type": "session.started",
		"session": map[string]any{
			"id": "live_e2e",
		},
	})
	started := receiveEvent(t, session.Events())
	if started.Kind != EventSessionStarted || started.RawType != "session.started" {
		t.Fatalf("first event = %+v", started)
	}
	writeOpenAIEvent(t, conn, map[string]any{"type": "session.input_transcript.delta", "delta": "run ", "start_ms": 10, "end_ms": 20})
	writeOpenAIEvent(t, conn, map[string]any{"type": "session.output_transcript.delta", "delta": "I'll help. ", "start_ms": 20, "end_ms": 30})
	writeOpenAIEvent(t, conn, map[string]any{
		"type":      "session.delegation.created",
		"offset_ms": 30,
		"delegation": map[string]any{
			"id": "item_delegate", "type": "delegation", "target": "client",
		},
	})
	for _, want := range []Event{
		{Kind: EventUserTranscript, Role: RoleUser, Text: "run "},
		{Kind: EventAssistantTranscript, Role: RoleAssistant, Text: "I'll help. "},
		{Kind: EventDelegationCreated, DelegationID: "item_delegate"},
	} {
		got := receiveEvent(t, session.Events())
		if got.Kind != want.Kind || got.Role != want.Role || got.Text != want.Text || got.DelegationID != want.DelegationID {
			t.Fatalf("event = %+v, want %+v", got, want)
		}
	}

	if err := session.AppendDelegation(context.Background(), "item_delegate", DelegationChunk{Channel: ChannelCommentary, Text: "Checking now."}); err != nil {
		t.Fatal(err)
	}
	thinking := receiveOpenAILiveClientMessage(t, clientMessages)
	if thinking.Type != "session.thinking.append" || thinking.DelegationID == nil || *thinking.DelegationID != "item_delegate" || thinking.Content != "Checking now." {
		t.Fatalf("thinking append = %+v", thinking)
	}
	if err := session.AppendDelegation(context.Background(), "item_delegate", DelegationChunk{Channel: ChannelSpeakable, Text: "Tests passed."}); err != nil {
		t.Fatal(err)
	}
	commentary := receiveOpenAILiveClientMessage(t, clientMessages)
	if commentary.Type != "session.commentary.append" || commentary.DelegationID == nil || *commentary.DelegationID != "item_delegate" || commentary.Content != "Tests passed." {
		t.Fatalf("commentary append = %+v", commentary)
	}
	if err := session.AppendText(context.Background(), "[USER] use order 123"); err != nil {
		t.Fatal(err)
	}
	typed := receiveOpenAILiveClientMessage(t, clientMessages)
	if typed.Type != openAILiveThinkingAppend || typed.DelegationID != nil || typed.Content != "[USER] use order 123" {
		t.Fatalf("typed context = %+v", typed)
	}
	typedRequest := receiveEvent(t, session.Events())
	if typedRequest.Kind != EventDelegationCreated || typedRequest.Text != "use order 123" || typedRequest.DelegationID == "" {
		t.Fatalf("typed delegation = %+v", typedRequest)
	}
	if err := session.AppendDelegation(context.Background(), typedRequest.DelegationID, DelegationChunk{Channel: ChannelSpeakable, Text: "Order found."}); err != nil {
		t.Fatal(err)
	}
	typedResult := receiveOpenAILiveClientMessage(t, clientMessages)
	if typedResult.Type != openAILiveCommentaryAppend || typedResult.DelegationID != nil || typedResult.Content != "Order found." {
		t.Fatalf("typed result = %+v", typedResult)
	}
	if err := liveSession.appendContext(context.Background(), openAILiveInstructionsAppend, nil, "Wait for the user."); err != nil {
		t.Fatal(err)
	}
	instruction := receiveOpenAILiveClientMessage(t, clientMessages)
	if instruction.Type != openAILiveInstructionsAppend || instruction.DelegationID != nil || instruction.Content != "Wait for the user." {
		t.Fatalf("instruction append = %+v", instruction)
	}

	longResult := strings.Repeat("x", MaxAppendBytes+1)
	if err := session.AppendDelegation(context.Background(), "item_delegate", DelegationChunk{Channel: ChannelSpeakable, Text: longResult}); err != nil {
		t.Fatal(err)
	}
	firstPart := receiveOpenAILiveClientMessage(t, clientMessages)
	secondPart := receiveOpenAILiveClientMessage(t, clientMessages)
	if firstPart.Type != openAILiveCommentaryAppend || len(firstPart.Content) != MaxAppendBytes || secondPart.Type != openAILiveCommentaryAppend || secondPart.Content != "x" {
		t.Fatalf("bounded appends = %+v / %+v", firstPart, secondPart)
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := liveSession.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	closeEvent := receiveOpenAIClientType(t, clientMessages)
	if closeEvent != "session.close" {
		t.Fatalf("close command = %q", closeEvent)
	}
	ended := receiveEvent(t, session.Events())
	if ended.Kind != EventEnded || ended.RawType != "session.closed" {
		t.Fatalf("ended = %+v", ended)
	}
}

func TestOpenAIProviderRoutesRealtimeModelsToLegacyTransport(t *testing.T) {
	paths := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		switch r.URL.Path {
		case "/v1/realtime/calls":
			if err := r.ParseMultipartForm(64 << 10); err != nil {
				t.Errorf("parse multipart: %v", err)
			}
			var payload openAIRealtimeSessionPayload
			if err := json.Unmarshal([]byte(r.FormValue("session")), &payload); err != nil {
				t.Errorf("decode Realtime session: %v", err)
			} else if payload.Model != "my-realtime-model" || payload.Tools[0].Name != openAIDelegationTool {
				t.Errorf("payload = %+v", payload)
			}
			w.Header().Set("Location", "/v1/realtime/calls/rtc_legacy")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "v=0\r\na=answer\r\n")
		case "/v1/realtime":
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err == nil {
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cfg := config.LiveConfig{Provider: config.LiveProviderOpenAI}
	cfg.OpenAI.APIKey = "key"
	cfg.OpenAI.Model = "my-realtime-model"
	cfg.OpenAI.BaseURL = server.URL + "/v1"
	session, err := NewOpenAIProvider(cfg, server.Client()).Start(context.Background(), "v=0\r\n", SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := session.(*openAISession); !ok {
		t.Fatalf("session = %T, want legacy Realtime", session)
	}
	if first, second := <-paths, <-paths; first != "/v1/realtime/calls" || second != "/v1/realtime" {
		t.Fatalf("paths = %q, %q", first, second)
	}
	_ = session.Close(context.Background())
}

func TestOpenAIProviderUsesEnvironmentKeyAndReportsMissingKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", " sk-env ")
	provider := NewOpenAIProvider(config.LiveConfig{}, nil)
	if key, err := provider.apiKey(); err != nil || key != "sk-env" {
		t.Fatalf("environment key = %q, %v", key, err)
	}

	t.Setenv("OPENAI_API_KEY", "")
	if err := provider.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Fatalf("missing key error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provider.Ready(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled readiness = %v", err)
	}
}

func TestOpenAIProviderRejectsEmptyOfferBeforeNetwork(t *testing.T) {
	cfg := config.LiveConfig{}
	cfg.OpenAI.APIKey = "key"
	provider := NewOpenAIProvider(cfg, nil)
	if _, err := provider.Start(context.Background(), "  ", SessionOptions{}); err == nil || !strings.Contains(err.Error(), "empty sdp") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAIProviderRejectsInvalidVoiceBeforeNetwork(t *testing.T) {
	cfg := config.LiveConfig{}
	cfg.OpenAI.APIKey = "key"
	cfg.OpenAI.Voice = "not-a-voice"
	provider := NewOpenAIProvider(cfg, &http.Client{Transport: openAIRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid voice reached the network")
		return nil, nil
	})})
	if _, err := provider.Start(context.Background(), "v=0\r\n", SessionOptions{}); err == nil || !strings.Contains(err.Error(), "live.openai.voice") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAIProviderReturnsLiveSidebandAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/live/sessions" {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"session":{"id":"live_1"},"transport":{"type":"webrtc","sdp":"v=0\\r\\n"}}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	cfg := config.LiveConfig{Provider: config.LiveProviderOpenAI}
	cfg.OpenAI.APIKey = "bad"
	cfg.OpenAI.BaseURL = server.URL
	_, err := NewOpenAIProvider(cfg, server.Client()).Start(context.Background(), "v=0\r\n", SessionOptions{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAISessionRejectsLateOrUnidentifiedDelegationOutput(t *testing.T) {
	session := &openAISession{
		delegations:    make(map[string]*openAIPendingDelegation),
		completedCalls: map[string]struct{}{"call_done": {}},
	}
	if err := session.AppendDelegation(context.Background(), "", DelegationChunk{Text: "x"}); err == nil {
		t.Fatal("missing call ID was accepted")
	}
	if err := session.AppendDelegation(context.Background(), "call_done", DelegationChunk{Text: "x"}); err == nil || !strings.Contains(err.Error(), "already completed") {
		t.Fatalf("late output error = %v", err)
	}
}

type openAIRoundTripFunc func(*http.Request) (*http.Response, error)

func (f openAIRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func receiveOpenAILiveClientMessage(t *testing.T, messages <-chan []byte) openAILiveAppendEvent {
	t.Helper()
	select {
	case payload := <-messages:
		var event openAILiveAppendEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode GPT-Live client event %s: %v", payload, err)
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for GPT-Live client event")
		return openAILiveAppendEvent{}
	}
}

func receiveOpenAIClientType(t *testing.T, messages <-chan []byte) string {
	t.Helper()
	select {
	case payload := <-messages:
		var event openAIClientEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			t.Fatalf("decode OpenAI client event %s: %v", payload, err)
		}
		return event.Type
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for OpenAI client event")
		return ""
	}
}

func writeOpenAIEvent(t *testing.T, conn *websocket.Conn, event any) {
	t.Helper()
	if err := conn.WriteJSON(event); err != nil {
		t.Fatalf("write OpenAI server event: %v", err)
	}
}

func TestOpenAILiveCloseReportsMissingFinalEvent(t *testing.T) {
	channel := &openAISideband{ready: make(chan struct{}), done: make(chan struct{}), finished: make(chan struct{})}
	channel.markStopped()
	close(channel.finished)
	session := newOpenAILiveSession("answer", channel)
	if err := session.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "before session.closed") {
		t.Fatalf("close error = %v", err)
	}
}
