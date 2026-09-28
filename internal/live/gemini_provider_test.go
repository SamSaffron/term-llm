package live

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
)

func TestGeminiProviderProtocolMediaTranscriptsAndDelegation(t *testing.T) {
	requests := make(chan *http.Request, 1)
	clientMessages := make(chan []byte, 32)
	serverConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
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
			var message map[string]json.RawMessage
			if json.Unmarshal(payload, &message) == nil && message["setup"] != nil {
				_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})
			}
		}
	}))
	defer server.Close()

	cfg := config.LiveConfig{Provider: config.LiveProviderGemini}
	cfg.Gemini.APIKey = "gemini-secret"
	cfg.Gemini.BaseURL = server.URL + "/live"
	provider := NewGeminiProvider(cfg)
	session, err := provider.Start(context.Background(), "", SessionOptions{
		Context: "authoritative host facts",
		InitialItems: []InitialItem{
			{Role: RoleUser, Text: "earlier question"},
			{Role: RoleAssistant, Text: "earlier answer"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pcm, ok := session.(PCMSession)
	if !ok {
		t.Fatalf("session = %T, want PCMSession", session)
	}
	if session.AnswerSDP() != "" {
		t.Fatalf("Gemini answer SDP = %q", session.AnswerSDP())
	}
	if _, ok := session.(DelegationCompletionSession); !ok {
		t.Fatal("Gemini session does not expose delegation completion")
	}

	request := <-requests
	if request.URL.Path != "/live" || request.URL.Query().Get("key") != "gemini-secret" {
		t.Fatalf("handshake URL = %s", request.URL.String())
	}
	setupPayload := <-clientMessages
	var setup geminiSetupMessage
	if err := json.Unmarshal(setupPayload, &setup); err != nil {
		t.Fatal(err)
	}
	if setup.Setup.Model != "models/"+config.DefaultLiveGeminiModel || setup.Setup.GenerationConfig.ResponseModalities[0] != "AUDIO" {
		t.Fatalf("setup = %+v", setup.Setup)
	}
	if setup.Setup.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName != config.DefaultLiveGeminiVoice {
		t.Fatalf("voice = %+v", setup.Setup.GenerationConfig.SpeechConfig)
	}
	if !strings.Contains(setup.Setup.SystemInstruction.Parts[0].Text, "authoritative host facts") || setup.Setup.HistoryConfig == nil {
		t.Fatalf("instructions/history config = %+v", setup.Setup)
	}
	if setup.Setup.InputAudioTranscription == nil || setup.Setup.OutputAudioTranscription == nil {
		t.Fatal("Gemini input/output transcription must be enabled in setup")
	}
	if got := setup.Setup.Tools[0].FunctionDeclarations[0].Name; got != geminiDelegationTool {
		t.Fatalf("tool name = %q", got)
	}
	var history geminiClientMessage
	if err := json.Unmarshal(<-clientMessages, &history); err != nil {
		t.Fatal(err)
	}
	if history.ClientContent == nil || !history.ClientContent.TurnComplete || len(history.ClientContent.Turns) != 2 || history.ClientContent.Turns[1].Role != "model" {
		t.Fatalf("history = %+v", history.ClientContent)
	}
	if started := receiveEvent(t, session.Events()); started.Kind != EventSessionStarted {
		t.Fatalf("started = %+v", started)
	}

	conn := <-serverConn
	if err := pcm.SendPCM(context.Background(), []byte{1, 0, 2, 0}); err != nil {
		t.Fatal(err)
	}
	var input geminiClientMessage
	if err := json.Unmarshal(<-clientMessages, &input); err != nil {
		t.Fatal(err)
	}
	if input.RealtimeInput == nil || input.RealtimeInput.Audio.MIMEType != "audio/pcm;rate=16000" || string(input.RealtimeInput.Audio.Data) != string([]byte{1, 0, 2, 0}) {
		t.Fatalf("PCM input = %+v", input.RealtimeInput)
	}

	_ = conn.WriteJSON(map[string]any{
		"serverContent": map[string]any{
			"inputTranscription":  map[string]any{"text": "check files"},
			"outputTranscription": map[string]any{"text": "I'll check."},
			"modelTurn": map[string]any{
				"parts": []any{map[string]any{
					"inlineData": map[string]any{"mimeType": "audio/pcm;rate=24000", "data": []byte{3, 0, 4, 0}},
				}},
			},
		},
	})
	user := receiveEvent(t, session.Events())
	assistant := receiveEvent(t, session.Events())
	if user.Kind != EventUserTranscript || user.Text != "check files" || assistant.Kind != EventAssistantTranscript || assistant.Text != "I'll check." {
		t.Fatalf("transcripts = %+v / %+v", user, assistant)
	}
	select {
	case frame := <-pcm.PCMFrames():
		if frame.Flush || string(frame.Audio) != string([]byte{3, 0, 4, 0}) {
			t.Fatalf("PCM output = %+v", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no PCM output")
	}

	_ = conn.WriteJSON(map[string]any{"toolCall": map[string]any{"functionCalls": []any{map[string]any{
		"id": "call_1", "name": geminiDelegationTool, "args": map[string]any{"input": "inspect the repository"},
	}}}})
	delegation := receiveEvent(t, session.Events())
	if delegation.Kind != EventDelegationCreated || delegation.DelegationID != "call_1" || delegation.Text != "inspect the repository" {
		t.Fatalf("delegation = %+v", delegation)
	}
	if err := session.AppendDelegation(context.Background(), "call_1", DelegationChunk{Channel: ChannelQuiet, Text: "Running tests"}); err != nil {
		t.Fatal(err)
	}
	// Progress is stale by the time the single tool response is sent.
	for _, chunk := range []DelegationChunk{
		{Channel: ChannelQuiet, Text: "[STATUS] Running shell (4.0s).", Progress: true},
		{Channel: ChannelSpeakable, Text: "[PROGRESS] Still working.", Progress: true},
	} {
		if err := session.AppendDelegation(context.Background(), "call_1", chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.AppendDelegation(context.Background(), "call_1", DelegationChunk{Channel: ChannelSpeakable, Text: "All tests passed"}); err != nil {
		t.Fatal(err)
	}
	if err := session.(DelegationCompletionSession).CompleteDelegation(context.Background(), "call_1"); err != nil {
		t.Fatal(err)
	}
	var result geminiClientMessage
	if err := json.Unmarshal(<-clientMessages, &result); err != nil {
		t.Fatal(err)
	}
	responses := result.ToolResponse.FunctionResponses
	if len(responses) != 1 || responses[0].ID != "call_1" || !strings.Contains(responses[0].Response["result"].(string), "All tests passed") {
		t.Fatalf("tool response = %+v", result.ToolResponse)
	}
	if text := responses[0].Response["result"].(string); strings.Contains(text, "[STATUS]") || strings.Contains(text, "[PROGRESS]") {
		t.Fatalf("progress leaked into the final tool response: %q", text)
	}

	_ = conn.WriteJSON(map[string]any{"serverContent": map[string]any{"interrupted": true}})
	if interrupted := receiveEvent(t, session.Events()); interrupted.Kind != EventInterrupted {
		t.Fatalf("interrupted event = %+v", interrupted)
	}
	select {
	case frame := <-pcm.PCMFrames():
		if !frame.Flush {
			t.Fatalf("interrupt frame = %+v", frame)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no interrupt frame")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	var ending geminiClientMessage
	if err := json.Unmarshal(<-clientMessages, &ending); err != nil {
		t.Fatal(err)
	}
	if ending.RealtimeInput == nil || !ending.RealtimeInput.AudioStreamEnd {
		t.Fatalf("close message = %+v", ending)
	}
}

func TestGeminiSessionCloseUnblocksSaturatedEvents(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		serverConn <- conn
		defer close(serverDone)
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	session := newGeminiSession(conn)
	for range cap(session.events) {
		session.events <- Event{Kind: EventUserTranscript, Text: "queued"}
	}
	go session.run()

	remote := <-serverConn
	if err := remote.WriteJSON(map[string]any{"toolCall": map[string]any{"functionCalls": []any{map[string]any{
		"id": "blocked_call", "name": geminiDelegationTool, "args": map[string]any{"input": "blocked"},
	}}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.mu.Lock()
		_, pending := session.pending["blocked_call"]
		session.mu.Unlock()
		if pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Gemini reader did not reach the saturated event queue")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- session.Close(ctx) }()
	go func() { results <- session.Close(ctx) }()
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("Gemini websocket was not closed")
	}

	count := 0
	for range session.Events() {
		count++
	}
	if count != geminiLiveEventBuffer {
		t.Fatalf("events after shutdown = %d, want %d queued events", count, geminiLiveEventBuffer)
	}

	// A late tool call after shutdown must not write session state or emit on the
	// closed event channel.
	session.handleToolCalls([]geminiFunctionCall{{
		ID: "late_call", Name: geminiDelegationTool, Args: map[string]any{"input": "late"},
	}})
	session.mu.Lock()
	_, late := session.pending["late_call"]
	session.mu.Unlock()
	if late {
		t.Fatal("tool call was recorded after session shutdown")
	}
}

func TestGeminiSessionInterruptionReplacesQueuedPCMBeforeNewAudio(t *testing.T) {
	session := newGeminiSession(nil)
	for i := range cap(session.pcmFrames) {
		session.pcmFrames <- PCMFrame{Audio: []byte{byte(i), 0}}
	}

	newAudio := []byte{255, 0, 254, 0}
	session.handleServerContent(&geminiServerContent{
		Interrupted: true,
		ModelTurn: &geminiContent{Parts: []geminiPart{{InlineData: &geminiInlineData{
			MIMEType: "audio/pcm;rate=24000",
			Data:     newAudio,
		}}}},
	})

	first := <-session.pcmFrames
	if !first.Flush || len(first.Audio) != 0 {
		t.Fatalf("first PCM frame after interruption = %+v, want flush", first)
	}
	second := <-session.pcmFrames
	if second.Flush || string(second.Audio) != string(newAudio) {
		t.Fatalf("second PCM frame after interruption = %+v, want new audio %v", second, newAudio)
	}
	select {
	case frame := <-session.pcmFrames:
		t.Fatalf("unexpected stale PCM after interruption: %+v", frame)
	default:
	}

	interrupted := <-session.events
	if interrupted.Kind != EventInterrupted {
		t.Fatalf("event after interruption = %+v", interrupted)
	}
	select {
	case event := <-session.events:
		t.Fatalf("unexpected event after interruption: %+v", event)
	default:
	}
}

func TestGeminiProviderSetupCancellationWithoutDeadline(t *testing.T) {
	setupReceived := make(chan struct{})
	peerClosed := make(chan struct{})
	serverConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		close(setupReceived)
		_, _, _ = conn.ReadMessage()
		close(peerClosed)
	}))
	defer server.Close()

	cfg := config.LiveConfig{Provider: config.LiveProviderGemini}
	cfg.Gemini.APIKey = "gemini-secret"
	cfg.Gemini.BaseURL = server.URL + "/live"
	provider := NewGeminiProvider(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("test context unexpectedly has a deadline")
	}
	result := make(chan error, 1)
	go func() {
		_, err := provider.Start(ctx, "", SessionOptions{})
		result <- err
	}()

	conn := <-serverConn
	<-setupReceived
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start after setup cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		_ = conn.Close()
		t.Fatal("setup read was not interrupted by context cancellation")
	}
	select {
	case <-peerClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("setup cancellation did not close the websocket")
	}
}

type geminiDeadlineConn struct {
	net.Conn
	readDeadlines chan time.Time
}

func (c *geminiDeadlineConn) SetReadDeadline(deadline time.Time) error {
	c.readDeadlines <- deadline
	return c.Conn.SetReadDeadline(deadline)
}

func TestGeminiProviderSetupUsesDefaultDeadlineAndStopsCancellationOnHandoff(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	setupSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}}); err != nil {
			return
		}
		close(setupSent)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	readDeadlines := make(chan time.Time, 16)
	dialer := *websocket.DefaultDialer
	netDialer := &net.Dialer{}
	dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := netDialer.DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &geminiDeadlineConn{Conn: conn, readDeadlines: readDeadlines}, nil
	}

	cfg := config.LiveConfig{Provider: config.LiveProviderGemini}
	cfg.Gemini.APIKey = "gemini-secret"
	cfg.Gemini.BaseURL = server.URL + "/live"
	provider := NewGeminiProvider(cfg)
	provider.dialer = &dialer
	ctx, cancel := context.WithCancel(context.Background())
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("test context unexpectedly has a deadline")
	}
	beforeStart := time.Now()
	session, err := provider.Start(ctx, "", SessionOptions{})
	afterStart := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		if err := session.Close(closeCtx); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	setupDeadline := <-readDeadlines
	if setupDeadline.Before(beforeStart.Add(geminiLiveWriteWait)) || setupDeadline.After(afterStart.Add(geminiLiveWriteWait)) {
		t.Fatalf("default setup deadline = %v, want start time + %v", setupDeadline, geminiLiveWriteWait)
	}
	if resetDeadline := <-readDeadlines; !resetDeadline.IsZero() {
		t.Fatalf("setup read deadline reset = %v, want zero", resetDeadline)
	}

	if started := receiveEvent(t, session.Events()); started.Kind != EventSessionStarted {
		t.Fatalf("started event = %+v", started)
	}
	cancel()
	conn := <-serverConn
	<-setupSent
	if err := conn.WriteJSON(map[string]any{
		"serverContent": map[string]any{"inputTranscription": map[string]any{"text": "still connected"}},
	}); err != nil {
		t.Fatalf("write after Start context cancellation: %v", err)
	}
	if event := receiveEvent(t, session.Events()); event.Kind != EventUserTranscript || event.Text != "still connected" {
		t.Fatalf("event after Start context cancellation = %+v", event)
	}
}

func TestGeminiProviderReadinessURLAndFrameValidation(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_API_KEY", "")
	provider := NewGeminiProvider(config.LiveConfig{})
	if err := provider.Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "live.gemini.api_key") {
		t.Fatalf("missing key error = %v", err)
	}

	url, err := geminiLiveURL(config.DefaultLiveGeminiBaseURL, "secret + value")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "wss://generativelanguage.googleapis.com/") || !strings.Contains(url, "key=secret+%2B+value") {
		t.Fatalf("URL = %q", url)
	}
	if _, err := geminiLiveURL("file:///tmp/socket", "key"); err == nil {
		t.Fatal("invalid scheme accepted")
	}

	session := newGeminiSession(nil)
	if err := session.SendPCM(context.Background(), []byte{1}); err == nil {
		t.Fatal("odd PCM frame accepted")
	}
	if err := session.SendPCM(context.Background(), make([]byte, geminiLiveMaxPCMBytes+2)); err == nil {
		t.Fatal("oversize PCM frame accepted")
	}
}
