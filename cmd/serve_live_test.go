package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/credentials"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
)

// liveTestHarness wires a serve server to in-process fakes for the provider's
// call-creation endpoint and control channel.
type liveTestHarness struct {
	server      *serveServer
	callServer  *httptest.Server
	wsServer    *httptest.Server
	callHeaders chan http.Header
	callBodies  chan []byte
	conns       chan *websocket.Conn
	inbound     chan string
}

func writeTestChatGPTCredentials(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "term-llm"), 0o755); err != nil {
		t.Fatal(err)
	}
	creds := credentials.ChatGPTCredentials{
		AccessToken:  "token-live",
		RefreshToken: "refresh-live",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		AccountID:    "acct-live",
	}
	payload, err := json.Marshal(creds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "term-llm", "chatgpt_oauth.json"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newLiveTestHarness(t *testing.T, responses ...string) *liveTestHarness {
	t.Helper()
	writeTestChatGPTCredentials(t)

	harness := &liveTestHarness{
		callHeaders: make(chan http.Header, 4),
		callBodies:  make(chan []byte, 4),
		conns:       make(chan *websocket.Conn, 4),
		inbound:     make(chan string, 64),
	}
	harness.callServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		harness.callHeaders <- r.Header.Clone()
		harness.callBodies <- body
		w.Header().Set("Location", "/v1/live/rtc_test")
		_, _ = io.WriteString(w, "v=0\r\na=answer\r\n")
	}))
	t.Cleanup(harness.callServer.Close)

	upgrader := websocket.Upgrader{}
	harness.wsServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		harness.conns <- conn
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			harness.inbound <- string(payload)
		}
	}))
	t.Cleanup(harness.wsServer.Close)

	srv := newTestServeServer(responses...)
	srv.cfg.ui = true
	srv.shutdownCh = make(chan struct{})
	// These cases cover the direct realtime protocol end to end against fakes.
	srv.cfgRef = &config.Config{Live: config.LiveConfig{
		Enabled:  true,
		Provider: config.LiveProviderChatGPT,
		ChatGPT: config.LiveChatGPTConfig{
			Model:           "gpt-live-test",
			Voice:           "cove",
			CallBaseURL:     harness.callServer.URL,
			SidebandBaseURL: harness.wsServer.URL,
		},
	}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.closeLiveSessions(ctx)
	})
	harness.server = srv
	return harness
}

// browserOffer mirrors the shape a browser produces: every line, including the
// last, ends with CRLF.
const browserOffer = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n"

func (h *liveTestHarness) startLive(t *testing.T, sessionID string) (int, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"sdp":"v=0\r\na=offer\r\n","session_id":%q}`, sessionID)
	request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.server.handleLiveSessions(recorder, request)
	var decoded map[string]any
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	}
	return recorder.Code, decoded
}

func (h *liveTestHarness) waitForInbound(t *testing.T, what string, match func(string) bool) string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case message := <-h.inbound:
			if match(message) {
				return message
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
			return ""
		}
	}
}

func TestLiveStartCreatesProviderCallAndReturnsAnswer(t *testing.T) {
	harness := newLiveTestHarness(t)

	status, body := harness.startLive(t, "sess_live")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	if body["sdp"] != "v=0\r\na=answer\r\n" {
		t.Fatalf("answer sdp = %q", body["sdp"])
	}
	if !strings.HasPrefix(fmt.Sprint(body["live_id"]), "live_") {
		t.Fatalf("live_id = %v", body["live_id"])
	}
	if body["session_id"] != "sess_live" {
		t.Fatalf("session_id = %v", body["session_id"])
	}

	headers := <-harness.callHeaders
	if got := headers.Get("Authorization"); got != "Bearer token-live" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("ChatGPT-Account-ID"); got != "acct-live" {
		t.Fatalf("ChatGPT-Account-ID = %q", got)
	}
	if got := headers.Get("originator"); got != "term-llm" {
		t.Fatalf("originator = %q", got)
	}
	if got := headers.Get("X-Session-Id"); got != "sess_live" {
		t.Fatalf("x-session-id = %q", got)
	}

	var payload struct {
		Session struct {
			Model string `json:"model"`
		} `json:"session"`
	}
	if err := json.Unmarshal(<-harness.callBodies, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Session.Model != "gpt-live-test" {
		t.Fatalf("model = %q", payload.Session.Model)
	}

	// The provider control channel is joined immediately after the answer.
	select {
	case <-harness.conns:
	case <-time.After(5 * time.Second):
		t.Fatal("the control channel was never joined")
	}
}

func TestLiveForwardsTheBrowserOfferAndAnswerVerbatim(t *testing.T) {
	harness := newLiveTestHarness(t)

	payload, err := json.Marshal(map[string]string{"sdp": browserOffer, "session_id": "sess_sdp"})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions", strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	harness.server.handleLiveSessions(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var sent struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(<-harness.callBodies, &sent); err != nil {
		t.Fatal(err)
	}
	// SDP is whitespace-significant. Trimming the trailing CRLF makes the
	// provider reject the offer with "failed to unmarshal SDP: EOF".
	if sent.SDP != browserOffer {
		t.Fatalf("forwarded offer = %q, want %q", sent.SDP, browserOffer)
	}

	var answered map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if answered["sdp"] != "v=0\r\na=answer\r\n" {
		t.Fatalf("answer returned to the browser = %q", answered["sdp"])
	}
}

func TestLiveDelegationRunsAgentTurnAndSpeaksTheAnswer(t *testing.T) {
	harness := newLiveTestHarness(t, "There are three Go files.")

	status, body := harness.startLive(t, "sess_delegate")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	conn := <-harness.conns
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn.done","turn":{"role":"user","transcript":"list the go files"}}`)); err != nil {
		t.Fatal(err)
	}
	delegation := `{"type":"delegation.created","item":{"id":"item_1","type":"delegation","target":"client","content":[{"type":"input_text","text":"list the go files"}]}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(delegation)); err != nil {
		t.Fatal(err)
	}

	message := harness.waitForInbound(t, "the spoken answer", func(message string) bool {
		return strings.Contains(message, "delegation.context.append") && strings.Contains(message, "three Go files")
	})
	if !strings.Contains(message, `"delegation_item_id":"item_1"`) {
		t.Fatalf("append lost the delegation id: %s", message)
	}
	if !strings.Contains(message, `"channel":"speakable"`) {
		t.Fatalf("append used the wrong channel: %s", message)
	}

	runtime, ok := harness.server.sessionMgr.Get("sess_delegate")
	if !ok {
		t.Fatal("the delegated turn did not create a session runtime")
	}
	provider, ok := runtime.provider.(*llm.MockProvider)
	if !ok {
		t.Fatalf("unexpected provider %T", runtime.provider)
	}
	requests := provider.RecordedRequests()
	if len(requests) == 0 {
		t.Fatal("the delegated turn never reached the model")
	}
	prompt := lastUserMessageText(t, requests[len(requests)-1])
	if !strings.Contains(prompt, "<input>list the go files</input>") {
		t.Fatalf("delegated prompt = %s", prompt)
	}
	if !strings.Contains(prompt, "<transcript_delta>") {
		t.Fatalf("delegated prompt lost the transcript delta: %s", prompt)
	}
}

func TestLiveInterimTranscriptIsForwardedAsPreview(t *testing.T) {
	record := newLiveSession("live_interim", "chat")
	record.observe(live.Update{Kind: live.UpdateTranscript, Role: live.RoleUser, Text: "a revised guess", Interim: true})
	events, subscriberID, _, _ := record.subscribe(0)
	defer record.unsubscribe(subscriberID)
	if len(events) != 1 || events[0].Type != liveEventTranscript {
		t.Fatalf("events = %+v", events)
	}
	data := events[0].Data
	if data["interim"] != true || data["final"] != false || data["text"] != "a revised guess" || data["role"] != live.RoleUser {
		t.Fatalf("preview payload = %+v", data)
	}
}

func TestLiveStreamsTranscriptEventsToTheBrowser(t *testing.T) {
	harness := newLiveTestHarness(t)
	status, body := harness.startLive(t, "sess_events")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	liveID := fmt.Sprint(body["live_id"])
	conn := <-harness.conns
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn.done","turn":{"role":"assistant","transcript":"hello there"}}`)); err != nil {
		t.Fatal(err)
	}

	record, ok := harness.server.lookupLiveSession(liveID)
	if !ok {
		t.Fatal("live session was not registered")
	}
	waitForLiveEvent(t, record, liveEventTranscript)

	stream := httptest.NewServer(http.HandlerFunc(harness.server.handleLiveSessionByID))
	defer stream.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, stream.URL+"/v1/live/sessions/"+liveID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}

	reader := bufio.NewReader(response.Body)
	var frame []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event stream: %v (read %v)", err, frame)
		}
		line = strings.TrimRight(line, "\r\n")
		frame = append(frame, line)
		if strings.HasPrefix(line, "data: ") && slices.Contains(frame, "event: "+liveEventTranscript) {
			if !strings.Contains(line, `"role":"assistant"`) || !strings.Contains(line, `"final":true`) {
				t.Fatalf("transcript frame = %v", frame)
			}
			return
		}
		if line == "" {
			frame = nil
		}
	}
}

func TestLiveRejectsSecondSessionForTheSameChatSession(t *testing.T) {
	harness := newLiveTestHarness(t)
	if status, body := harness.startLive(t, "sess_single"); status != http.StatusOK {
		t.Fatalf("status = %d, body = %v", status, body)
	}
	status, body := harness.startLive(t, "sess_single")
	if status != http.StatusConflict {
		t.Fatalf("second live session status = %d, body = %v", status, body)
	}
}

func TestLiveStopSendsSessionCloseAndIsIdempotent(t *testing.T) {
	harness := newLiveTestHarness(t)
	_, body := harness.startLive(t, "sess_stop")
	liveID := fmt.Sprint(body["live_id"])
	<-harness.conns

	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodDelete, "/v1/live/sessions/"+liveID, nil)
		recorder := httptest.NewRecorder()
		harness.server.handleLiveSessionByID(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("delete attempt %d status = %d", attempt, recorder.Code)
		}
	}
	harness.waitForInbound(t, "session.close", func(message string) bool {
		return strings.Contains(message, `"type":"session.close"`)
	})
	if _, ok := harness.server.lookupLiveSession(liveID); ok {
		t.Fatal("the live session was not removed")
	}
}

func TestLiveIsUnavailableWhenDisabled(t *testing.T) {
	harness := newLiveTestHarness(t)
	harness.server.cfgRef.Live.Enabled = false

	status, _ := harness.startLive(t, "sess_disabled")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	capability := harness.server.liveCapability(context.Background())
	if capability["enabled"] != false {
		t.Fatalf("capability = %v", capability)
	}
	if capability["provider"] != config.LiveProviderChatGPT {
		t.Fatalf("capability provider = %v", capability["provider"])
	}
}

func TestLiveCapabilityRequiresCredentials(t *testing.T) {
	harness := newLiveTestHarness(t)
	if enabled := harness.server.liveCapability(context.Background())["enabled"]; enabled != true {
		t.Fatalf("capability with credentials = %v", enabled)
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if enabled := harness.server.liveCapability(context.Background())["enabled"]; enabled != false {
		t.Fatalf("capability without credentials = %v", enabled)
	}
}

// stubLiveSession isolates provider lifecycle tests from protocol details.
type stubLiveSession struct {
	events chan live.Event
	closed chan struct{}
	once   sync.Once
}

func (s *stubLiveSession) AnswerSDP() string         { return "v=0\r\na=answer\r\n" }
func (s *stubLiveSession) Events() <-chan live.Event { return s.events }
func (s *stubLiveSession) AppendDelegation(context.Context, string, live.DelegationChunk) error {
	return nil
}
func (s *stubLiveSession) AppendText(context.Context, string) error { return nil }
func (s *stubLiveSession) Close(context.Context) error {
	s.once.Do(func() { close(s.closed); close(s.events) })
	return nil
}

type stubLiveProvider struct{ session *stubLiveSession }

func (p *stubLiveProvider) Name() string                { return "stub" }
func (p *stubLiveProvider) Ready(context.Context) error { return nil }
func (p *stubLiveProvider) Start(context.Context, string, live.SessionOptions) (live.Session, error) {
	return p.session, nil
}
func newStubLiveHarness(t *testing.T) (*serveServer, *stubLiveSession) {
	t.Helper()
	session := &stubLiveSession{events: make(chan live.Event, 16), closed: make(chan struct{})}
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.shutdownCh = make(chan struct{})
	srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderChatGPT}}
	srv.liveProviderFactory = func(config.LiveConfig) (live.Provider, error) { return &stubLiveProvider{session: session}, nil }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.closeLiveSessions(ctx)
	})
	return srv, session
}

func TestLiveSignalRouteRemoved(t *testing.T) {
	srv, _ := newStubLiveHarness(t)
	record := newLiveSession("test-call", "test-session")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions/test-call/signal", strings.NewReader(`{"type":"session.started"}`))
	response := httptest.NewRecorder()
	srv.handleLiveSessionByID(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("removed signal route status = %d", response.Code)
	}
}

func waitForLiveEvent(t *testing.T, record *liveSession, eventType string) {
	t.Helper()
	waitForLiveCondition(t, "live event "+eventType, func() bool {
		record.mu.Lock()
		defer record.mu.Unlock()
		for _, event := range record.events {
			if event.Type == eventType {
				return true
			}
		}
		return false
	})
}

func waitForLiveCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func lastUserMessageText(t *testing.T, request llm.Request) string {
	t.Helper()
	var text strings.Builder
	for _, message := range request.Messages {
		if message.Role != llm.RoleUser {
			continue
		}
		text.Reset()
		for _, part := range message.Parts {
			if part.Type == llm.PartText {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

type stubPCMLiveSession struct {
	events   chan live.Event
	frames   chan live.PCMFrame
	received chan []byte
	closed   chan struct{}
	once     sync.Once
}

func newStubPCMLiveSession() *stubPCMLiveSession {
	return &stubPCMLiveSession{
		events: make(chan live.Event, 16), frames: make(chan live.PCMFrame, 16),
		received: make(chan []byte, 16), closed: make(chan struct{}),
	}
}

func (s *stubPCMLiveSession) AnswerSDP() string         { return "" }
func (s *stubPCMLiveSession) Events() <-chan live.Event { return s.events }
func (s *stubPCMLiveSession) PCMFrames() <-chan live.PCMFrame {
	return s.frames
}
func (s *stubPCMLiveSession) SendPCM(_ context.Context, pcm []byte) error {
	s.received <- append([]byte(nil), pcm...)
	return nil
}
func (s *stubPCMLiveSession) AppendDelegation(context.Context, string, live.DelegationChunk) error {
	return nil
}
func (s *stubPCMLiveSession) AppendText(context.Context, string) error { return nil }
func (s *stubPCMLiveSession) Close(context.Context) error {
	s.once.Do(func() {
		close(s.closed)
		close(s.events)
		close(s.frames)
	})
	return nil
}

func TestLiveDiagnosticsEndpointIsDebugGatedAndStructured(t *testing.T) {
	t.Setenv("TERM_LLM_LIVE_DEBUG", "")
	report := `{"sequence":1,"elapsed_ms":5000,"audio_context_state":"running","stream_open":true,"packets_received":4,"bytes_received":6400,"packets_scheduled":4,"packets_ended":3,"playback_queued_seconds":0.2,"underruns":1,"flush_interrupts":1,"flush_buffer_resets":0,"input_packets":20,"input_bytes":12800,"input_queue_drops":2,"post_count":4,"post_errors":0,"post_latency_total_ms":80,"post_latency_max_ms":25,"post_inflight":0,"input_silence_ms":10,"output_silence_ms":50}`

	t.Run("disabled", func(t *testing.T) {
		srv := newTestServeServer()
		record := newLiveSession("live_diag_off", "session")
		if err := srv.registerLiveSession(record); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions/live_diag_off/diagnostics", strings.NewReader(report))
		response := httptest.NewRecorder()
		srv.handleLiveSessionByID(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("disabled status=%d body=%s", response.Code, response.Body.String())
		}
		if record.diagnosticReports != 0 {
			t.Fatalf("disabled endpoint processed %d reports", record.diagnosticReports)
		}
	})

	t.Run("enabled", func(t *testing.T) {
		srv := newTestServeServer()
		srv.cfg.debug = true
		record := newLiveSession("live_diag_on", "session")
		if err := srv.registerLiveSession(record); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions/live_diag_on/diagnostics", strings.NewReader(report))
		response := httptest.NewRecorder()
		srv.handleLiveSessionByID(response, request)
		if response.Code != http.StatusNoContent || record.diagnosticReports != 1 {
			t.Fatalf("enabled status=%d reports=%d body=%s", response.Code, record.diagnosticReports, response.Body.String())
		}
		request = httptest.NewRequest(http.MethodPost, "/v1/live/sessions/live_diag_on/diagnostics", strings.NewReader(report))
		response = httptest.NewRecorder()
		srv.handleLiveSessionByID(response, request)
		if response.Code != http.StatusNoContent || record.diagnosticReports != 1 {
			t.Fatalf("rate limit status=%d reports=%d", response.Code, record.diagnosticReports)
		}

		bad := strings.TrimSuffix(report, "}") + `,"message":"arbitrary browser log https://secret.example/token"}`
		request = httptest.NewRequest(http.MethodPost, "/v1/live/sessions/live_diag_on/diagnostics", strings.NewReader(bad))
		response = httptest.NewRecorder()
		srv.handleLiveSessionByID(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("arbitrary string status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestLiveGeminiStartReturnsPCMTransportWithoutSDP(t *testing.T) {
	pcm := newStubPCMLiveSession()
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.cfg.debug = true
	srv.cfg.debugRaw = true
	srv.shutdownCh = make(chan struct{})
	srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderGemini}}
	provider := &stubPCMProvider{session: pcm, options: make(chan live.SessionOptions, 1)}
	srv.liveProviderFactory = func(config.LiveConfig) (live.Provider, error) {
		return provider, nil
	}
	t.Cleanup(func() { srv.closeLiveSessions(context.Background()) })

	request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions", strings.NewReader(`{"session_id":"gemini-chat","audio_transport":"http_pcm"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.handleLiveSessions(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["transport"] != "http_pcm" || body["sdp"] != "" || strings.TrimSpace(fmt.Sprint(body["audio_capability"])) == "" || body["audio_url"] != nil || body["diagnostics"] != true {
		t.Fatalf("start response = %v", body)
	}
	select {
	case opts := <-provider.options:
		if !opts.Debug || !opts.DebugRaw {
			t.Fatalf("diagnostic options = %+v", opts)
		}
	default:
		t.Fatal("provider did not receive session options")
	}
	capability := srv.liveCapability(context.Background())
	if capability["transport"] != "http_pcm" || capability["model"] != config.DefaultLiveGeminiModel {
		t.Fatalf("capability = %v", capability)
	}
}

type stubPCMProvider struct {
	session *stubPCMLiveSession
	options chan live.SessionOptions
}

func (p *stubPCMProvider) Name() string                { return config.LiveProviderGemini }
func (p *stubPCMProvider) Ready(context.Context) error { return nil }
func (p *stubPCMProvider) Start(_ context.Context, _ string, opts live.SessionOptions) (live.Session, error) {
	if p.options != nil {
		p.options <- opts
	}
	return p.session, nil
}

type deadlineFailingLiveResponseWriter struct {
	header           http.Header
	deadlines        []time.Time
	currentDeadline  time.Time
	writeSawDeadline bool
	flushSawDeadline bool
	flushErr         error
	writes           int
}

func (w *deadlineFailingLiveResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *deadlineFailingLiveResponseWriter) Write([]byte) (int, error) {
	w.writes++
	w.writeSawDeadline = !w.currentDeadline.IsZero()
	return 0, context.DeadlineExceeded
}

func (*deadlineFailingLiveResponseWriter) WriteHeader(int) {}
func (*deadlineFailingLiveResponseWriter) Flush()          {}

func (w *deadlineFailingLiveResponseWriter) FlushError() error {
	w.flushSawDeadline = !w.currentDeadline.IsZero()
	return w.flushErr
}

func (w *deadlineFailingLiveResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.currentDeadline = deadline
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestLiveHTTPAudioOutputRequiresHeaderCapabilityAndDeadlinesFailedWrites(t *testing.T) {
	pcm := newStubPCMLiveSession()
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.shutdownCh = make(chan struct{})
	record := newLiveSession("live_http_capability", "chat_http_capability")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	if !srv.startLiveController(record, pcm) {
		t.Fatal("controller did not start")
	}
	t.Cleanup(func() { srv.closeLiveSessions(context.Background()) })

	token, err := record.issueAudioToken(time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	queryRequest := httptest.NewRequest(http.MethodGet, "/v1/live/sessions/"+record.id+"/audio/output?token="+token, nil)
	queryResponse := httptest.NewRecorder()
	srv.handleLiveSessionAudioOutput(queryResponse, queryRequest, record.id)
	if queryResponse.Code != http.StatusUnauthorized {
		t.Fatalf("query capability status=%d body=%s", queryResponse.Code, queryResponse.Body.String())
	}
	record.mu.Lock()
	attachedAfterQuery := record.audioAttached
	record.mu.Unlock()
	if attachedAfterQuery {
		t.Fatal("rejected query capability consumed the one-use claim")
	}

	headerRequest := httptest.NewRequest(http.MethodGet, "/v1/live/sessions/"+record.id+"/audio/output", nil)
	headerRequest.Header.Set("X-Term-LLM-Live-Audio-Capability", token)
	writer := &deadlineFailingLiveResponseWriter{}
	srv.handleLiveSessionAudioOutput(writer, headerRequest, record.id)
	if !writer.flushSawDeadline {
		t.Fatal("initial SSE flush did not have a write deadline")
	}
	if !writer.writeSawDeadline {
		t.Fatal("failed SSE write did not have a write deadline")
	}
	if len(writer.deadlines) < 4 {
		t.Fatalf("deadline operations=%v, want set+clear for initial flush and failed ready write", writer.deadlines)
	}
	for i, deadline := range writer.deadlines {
		if deadline.IsZero() != (i%2 == 1) {
			t.Fatalf("deadline %d zero=%t, want alternating set/clear: %v", i, deadline.IsZero(), writer.deadlines)
		}
	}
	select {
	case <-pcm.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("failed deadline-bound SSE write did not disconnect the live session")
	}
}

func TestLiveHTTPAudioOutputInitialFlushDeadlineFailureDisconnects(t *testing.T) {
	pcm := newStubPCMLiveSession()
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.shutdownCh = make(chan struct{})
	record := newLiveSession("live_http_flush", "chat_http_flush")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	if !srv.startLiveController(record, pcm) {
		t.Fatal("controller did not start")
	}
	t.Cleanup(func() { srv.closeLiveSessions(context.Background()) })

	token, err := record.issueAudioToken(time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/live/sessions/"+record.id+"/audio/output", nil)
	request.Header.Set("X-Term-LLM-Live-Audio-Capability", token)
	writer := &deadlineFailingLiveResponseWriter{flushErr: context.DeadlineExceeded}
	srv.handleLiveSessionAudioOutput(writer, request, record.id)
	if !writer.flushSawDeadline {
		t.Fatal("failed initial SSE flush did not have a write deadline")
	}
	if writer.writes != 0 {
		t.Fatalf("writes after failed initial flush=%d, want 0", writer.writes)
	}
	select {
	case <-pcm.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("failed initial SSE flush did not disconnect the live session")
	}
}

func TestLiveAudioWebSocketRequiresAuthOriginOneUseTokenAndBridgesPCM(t *testing.T) {
	pcm := newStubPCMLiveSession()
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.cfg.requireAuth = true
	srv.cfg.token = "serve-secret"
	srv.cfg.basePath = "/chat"
	srv.shutdownCh = make(chan struct{})
	srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderGemini}}
	record := newLiveSession("live_pcm", "chat_pcm")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	if !srv.startLiveController(record, pcm) {
		t.Fatal("controller did not start")
	}
	token, err := record.issueAudioToken(time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.httpHandler())
	defer ts.Close()
	t.Cleanup(func() { srv.closeLiveSessions(context.Background()) })
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/chat/v1/live/sessions/live_pcm/audio?token=" + token

	originOnly := http.Header{"Origin": []string{ts.URL}}
	if conn, response, err := websocket.DefaultDialer.Dial(wsURL, originOnly); err == nil {
		conn.Close()
		t.Fatal("unauthenticated audio socket connected")
	} else if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated response=%v err=%v", response, err)
	}

	headers := http.Header{"Origin": []string{ts.URL}, "Cookie": []string{"term_llm_token=serve-secret"}}
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("dial status=%d: %v", status, err)
	}
	defer conn.Close()
	// Valid microphone frames must reach the provider on the same socket.
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pcm.received:
		if string(got) != string([]byte{1, 0}) {
			t.Fatalf("provider PCM from odd frame = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("complete browser PCM sample was not forwarded")
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte{2, 0, 3, 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-pcm.received:
		if string(got) != string([]byte{2, 0, 3, 0}) {
			t.Fatalf("provider PCM after odd frame = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subsequent browser PCM was not forwarded")
	}

	pcm.frames <- live.PCMFrame{Audio: []byte{4, 0, 0xff}}
	messageType, payload, err := conn.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || string(payload) != string([]byte{4, 0}) {
		t.Fatalf("browser PCM from odd frame type=%d payload=%v err=%v", messageType, payload, err)
	}
	pcm.frames <- live.PCMFrame{Audio: []byte{5, 0, 6, 0}}
	messageType, payload, err = conn.ReadMessage()
	if err != nil || messageType != websocket.BinaryMessage || string(payload) != string([]byte{5, 0, 6, 0}) {
		t.Fatalf("browser PCM after odd frame type=%d payload=%v err=%v", messageType, payload, err)
	}
	pcm.frames <- live.PCMFrame{Flush: true}
	messageType, payload, err = conn.ReadMessage()
	if err != nil || messageType != websocket.TextMessage || !strings.Contains(string(payload), `"interrupt"`) {
		t.Fatalf("interrupt type=%d payload=%s err=%v", messageType, payload, err)
	}

	if second, secondResponse, secondErr := websocket.DefaultDialer.Dial(wsURL, headers); secondErr == nil {
		second.Close()
		t.Fatal("one-use audio capability was reused")
	} else if secondResponse == nil || secondResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("reuse response=%v err=%v", secondResponse, secondErr)
	}
	_ = conn.Close()
	select {
	case <-pcm.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("audio disconnect did not close provider session")
	}
	waitForLiveCondition(t, "audio disconnect cleanup", func() bool {
		_, ok := srv.lookupLiveSession(record.id)
		return !ok
	})
}

func TestLiveAudioWebSocketUpgradeFailureStopsClaimedSession(t *testing.T) {
	pcm := newStubPCMLiveSession()
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.shutdownCh = make(chan struct{})
	srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderGemini}}
	record := newLiveSession("live_upgrade_failure", "chat_upgrade_failure")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	if !srv.startLiveController(record, pcm) {
		t.Fatal("controller did not start")
	}
	token, err := record.issueAudioToken(time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.closeLiveSessions(context.Background()) })

	request := httptest.NewRequest(http.MethodGet, "/v1/live/sessions/live_upgrade_failure/audio?token="+token, nil)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	// ResponseRecorder deliberately cannot hijack the connection, so Upgrade
	// fails after the valid one-use capability has been claimed.
	response := httptest.NewRecorder()
	srv.handleLiveSessionAudio(response, request, record.id)

	select {
	case <-pcm.closed:
	default:
		t.Fatal("failed websocket upgrade left the provider session open")
	}
	if _, ok := srv.lookupLiveSession(record.id); ok {
		t.Fatal("failed websocket upgrade retained the live session")
	}
}

func TestLiveAudioWebSocketRejectsCrossOriginAndExpiredCapability(t *testing.T) {
	pcm := newStubPCMLiveSession()
	srv := newTestServeServer()
	srv.cfg.ui = true
	srv.cfg.basePath = "/chat"
	srv.shutdownCh = make(chan struct{})
	srv.cfgRef = &config.Config{Live: config.LiveConfig{Enabled: true, Provider: config.LiveProviderGemini}}
	record := newLiveSession("live_origin", "chat_origin")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	if !srv.startLiveController(record, pcm) {
		t.Fatal("controller did not start")
	}
	token, err := record.issueAudioToken(time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.httpHandler())
	defer ts.Close()
	t.Cleanup(func() { srv.closeLiveSessions(context.Background()) })
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/chat/v1/live/sessions/live_origin/audio?token=" + token

	badOrigin := http.Header{"Origin": []string{"https://evil.example"}}
	if conn, response, err := websocket.DefaultDialer.Dial(wsURL, badOrigin); err == nil {
		conn.Close()
		t.Fatal("cross-origin audio socket connected")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin response=%v err=%v", response, err)
	}
	if _, ok := record.claimAudio(token, time.Now()); !ok {
		t.Fatal("origin rejection consumed the audio capability")
	}

	expired := newLiveSession("expired", "chat")
	expired.pcmSession = pcm
	expiredToken, err := expired.issueAudioToken(time.Now().Add(-time.Minute), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := expired.claimAudio(expiredToken, time.Now()); ok {
		t.Fatal("expired audio capability was accepted")
	}
}

// delayedLiveProvider models negotiation completing successfully even after
// teardown has removed the local reservation.
type delayedLiveProvider struct {
	stubLiveProvider
	entered chan struct{}
	release chan struct{}
}

func (p *delayedLiveProvider) Start(context.Context, string, live.SessionOptions) (live.Session, error) {
	close(p.entered)
	<-p.release
	return p.session, nil
}

func TestLiveStartupAfterTeardownClosesProviderWithoutStartingController(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			srv, providerSession := newStubLiveHarness(t)
			provider := &delayedLiveProvider{stubLiveProvider: stubLiveProvider{session: providerSession}, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(provider.release) })
			srv.liveProviderFactory = func(config.LiveConfig) (live.Provider, error) { return provider, nil }
			payload, _ := json.Marshal(map[string]string{"session_id": "starting-call", "sdp": browserOffer})
			req := httptest.NewRequest(http.MethodPost, "/v1/live/sessions", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); srv.handleLiveSessions(rr, req) }()
			select {
			case <-provider.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("provider did not start")
			}
			srv.liveMu.Lock()
			record := srv.liveSessions[srv.liveByChat["starting-call"]]
			srv.liveMu.Unlock()
			if record == nil {
				t.Fatal("no startup reservation")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var replacement *liveSession
			if shutdown {
				srv.closeLiveSessions(ctx)
			} else {
				srv.stopLiveSession(ctx, record.id, "user")
				replacement = newLiveSession("replacement", record.sessionID)
				if err := srv.registerLiveSession(replacement); err != nil {
					t.Fatal(err)
				}
			}
			release.Do(func() { close(provider.release) })
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("startup did not finish")
			}
			if rr.Code != http.StatusConflict {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			select {
			case <-providerSession.closed:
			default:
				t.Fatal("orphan provider session was not closed")
			}
			if controller, cancel := record.running(); controller != nil || cancel != nil {
				t.Fatal("controller attached after teardown")
			}
			if _, ok := srv.lookupLiveSession(record.id); ok {
				t.Fatal("cancelled reservation reappeared")
			}
			if replacement != nil {
				if got, ok := srv.lookupLiveSession(replacement.id); !ok || got != replacement {
					t.Fatal("late startup disturbed replacement call")
				}
			}
		})
	}
}

func TestLiveControllerStartAndStopAreSerialized(t *testing.T) {
	for range 30 {
		srv, providerSession := newStubLiveHarness(t)
		record := newLiveSession("race", "start-stop")
		if err := srv.registerLiveSession(record); err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		started := make(chan bool, 1)
		stopped := make(chan struct{})
		go func() { <-gate; started <- srv.startLiveController(record, providerSession) }()
		go func() { <-gate; srv.stopLiveSession(context.Background(), record.id, "user"); close(stopped) }()
		close(gate)
		select {
		case ok := <-started:
			if !ok {
				if err := providerSession.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("startup deadlocked")
		}
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("teardown deadlocked")
		}
		select {
		case <-providerSession.closed:
		default:
			t.Fatal("provider left open")
		}
		if _, ok := srv.lookupLiveSession(record.id); ok {
			t.Fatal("call retained after stop")
		}
	}
}
