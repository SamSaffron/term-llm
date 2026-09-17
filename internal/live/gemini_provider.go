package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
)

const (
	geminiLiveWriteWait       = 10 * time.Second
	geminiLivePongWait        = 60 * time.Second
	geminiLivePingInterval    = 20 * time.Second
	geminiLiveMaxMessageBytes = 2 << 20
	geminiLiveMaxPCMBytes     = 64 << 10
	geminiLiveEventBuffer     = 64
	geminiLivePCMBuffer       = 64
	geminiLiveResultBytes     = 32 << 10
)

var errGeminiLiveEnded = errors.New("live: Gemini session ended")

// GeminiProvider runs Gemini Live over a server-owned BidiGenerateContent
// WebSocket. Browser audio is proxied as PCM and the configured API key never
// leaves the server.
type GeminiProvider struct {
	cfg    config.LiveConfig
	dialer *websocket.Dialer
}

// NewGeminiProvider builds the Gemini Live provider.
func NewGeminiProvider(cfg config.LiveConfig) *GeminiProvider {
	return &GeminiProvider{cfg: cfg}
}

func (p *GeminiProvider) Name() string { return config.LiveProviderGemini }

func (p *GeminiProvider) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.cfg.GeminiKey().Configured() {
		return errors.New("live Gemini API key is required (set live.gemini.api_key, GEMINI_API_KEY, or GOOGLE_API_KEY)")
	}
	return nil
}

func (p *GeminiProvider) Start(ctx context.Context, _ string, opts SessionOptions) (Session, error) {
	diagnostics := newGeminiDiagnostics(opts)
	apiKey, err := p.cfg.GeminiKey().Resolve()
	if err != nil {
		diagnostics.connectionError("credentials", err)
		return nil, fmt.Errorf("resolve live Gemini API key: %w", err)
	}
	if strings.TrimSpace(apiKey) == "" {
		err := errors.New("live Gemini API key is required (set live.gemini.api_key, GEMINI_API_KEY, or GOOGLE_API_KEY)")
		diagnostics.connectionError("credentials", err)
		return nil, err
	}
	if diagnostics != nil {
		diagnostics.apiKey = apiKey
	}
	endpoint, err := geminiLiveURL(p.baseURL(), apiKey)
	if err != nil {
		diagnostics.connectionError("endpoint", err)
		return nil, err
	}
	if diagnostics != nil {
		if parsed, parseErr := url.Parse(endpoint); parseErr == nil {
			diagnostics.metadata("dial begin scheme=%s host=%s path=%s", parsed.Scheme, parsed.Host, parsed.Path)
		}
	}
	dialer := p.dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		classified := classifyGeminiDialError(resp, err)
		diagnostics.connectionError("dial failed", classified)
		return nil, classified
	}
	if diagnostics != nil {
		diagnostics.metadata("dial connected elapsed_ms=%d", time.Since(diagnostics.started).Milliseconds())
	}
	conn.SetReadLimit(geminiLiveMaxMessageBytes)

	model := strings.TrimSpace(p.cfg.Gemini.Model)
	if model == "" {
		model = config.DefaultLiveGeminiModel
	}
	setup := geminiSetupFor(model, p.cfg.Gemini.ResolvedVoice(), resolvedInstructions(p.cfg.Instructions, opts), len(opts.InitialItems) > 0)
	if err := writeGeminiJSON(ctx, conn, setup); err != nil {
		diagnostics.connectionError("setup send failed", err)
		_ = conn.Close()
		return nil, err
	}
	diagnostics.setup(model, p.cfg.Gemini.ResolvedVoice(), len(opts.InitialItems))
	if err := waitGeminiSetup(ctx, conn, diagnostics); err != nil {
		diagnostics.connectionError("setup failed", err)
		_ = conn.Close()
		return nil, err
	}
	diagnostics.metadata("setup complete")

	session := newGeminiSessionWithDiagnostics(conn, diagnostics)
	if history := geminiHistory(opts.InitialItems); history != nil {
		if err := session.send(ctx, history); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("seed Gemini Live history: %w", err)
		}
	}
	session.events <- Event{Kind: EventSessionStarted, RawType: "setupComplete"}
	go session.run()
	return session, nil
}

func (p *GeminiProvider) baseURL() string {
	if value := strings.TrimSpace(p.cfg.Gemini.BaseURL); value != "" {
		return value
	}
	return config.DefaultLiveGeminiBaseURL
}

func geminiLiveURL(baseURL, apiKey string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("live: invalid Gemini Live URL: %w", err)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("live: invalid Gemini Live URL scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("live: Gemini Live URL is missing a host")
	}
	query := parsed.Query()
	query.Set("key", apiKey)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func classifyGeminiDialError(resp *http.Response, err error) error {
	if resp == nil {
		return fmt.Errorf("dial Gemini Live: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: Gemini Live", ErrUnauthorized)
	case http.StatusForbidden:
		return fmt.Errorf("%w: Gemini Live", ErrForbidden)
	default:
		return fmt.Errorf("dial Gemini Live: http %d: %w", resp.StatusCode, err)
	}
}

func waitGeminiSetup(ctx context.Context, conn *websocket.Conn, diagnostics *geminiDiagnostics) (resultErr error) {
	deadline := time.Now().Add(geminiLiveWriteWait)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetReadDeadline(deadline)

	cancelFinished := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(cancelFinished)
	})
	defer func() {
		if !stopCancellation() {
			<-cancelFinished
			if resultErr == nil {
				contextErr := ctx.Err()
				if contextErr == nil {
					contextErr = context.Canceled
				}
				resultErr = fmt.Errorf("wait for Gemini Live setup: %w", contextErr)
			}
		}
		_ = conn.SetReadDeadline(time.Time{})
	}()

	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return fmt.Errorf("wait for Gemini Live setup: %w", contextErr)
			}
			return fmt.Errorf("wait for Gemini Live setup: %w", err)
		}
		var message geminiServerMessage
		if err := jsonUnmarshalGemini(payload, &message); err != nil {
			diagnostics.malformed(payload, err)
			return err
		}
		diagnostics.incomingMessage(payload, &message)
		if message.Error != nil {
			return errors.New(geminiErrorText(message.Error))
		}
		if message.SetupComplete != nil {
			return nil
		}
	}
}

func jsonUnmarshalGemini(payload []byte, target any) error {
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode Gemini Live message: %w", err)
	}
	return nil
}

func writeGeminiJSON(ctx context.Context, conn *websocket.Conn, message any) error {
	payload, err := marshalGemini(message)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(geminiLiveWriteWait)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = conn.SetWriteDeadline(deadline)
	err = conn.WriteMessage(websocket.TextMessage, payload)
	_ = conn.SetWriteDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("send Gemini Live message: %w", err)
	}
	return nil
}

type geminiPendingDelegation struct {
	name string
	text strings.Builder
}

type geminiSession struct {
	conn      *websocket.Conn
	events    chan Event
	pcmFrames chan PCMFrame
	stopping  chan struct{}
	done      chan struct{}

	writeMu  sync.Mutex
	mu       sync.Mutex
	closed   bool
	pending  map[string]*geminiPendingDelegation
	finished map[string]struct{}

	userTranscript        strings.Builder
	interimUserTranscript bool
	assistantTranscript   strings.Builder
	diagnostics           *geminiDiagnostics
	closeOnce             sync.Once
}

func newGeminiSession(conn *websocket.Conn) *geminiSession {
	return newGeminiSessionWithDiagnostics(conn, nil)
}

func newGeminiSessionWithDiagnostics(conn *websocket.Conn, diagnostics *geminiDiagnostics) *geminiSession {
	return &geminiSession{
		conn: conn, events: make(chan Event, geminiLiveEventBuffer),
		pcmFrames: make(chan PCMFrame, geminiLivePCMBuffer), diagnostics: diagnostics,
		stopping: make(chan struct{}), done: make(chan struct{}),
		pending: make(map[string]*geminiPendingDelegation), finished: make(map[string]struct{}),
	}
}

func (s *geminiSession) AnswerSDP() string          { return "" }
func (s *geminiSession) Events() <-chan Event       { return s.events }
func (s *geminiSession) PCMFrames() <-chan PCMFrame { return s.pcmFrames }

func (s *geminiSession) run() {
	defer close(s.done)
	defer close(s.events)
	defer close(s.pcmFrames)
	defer s.markClosed()
	defer s.conn.Close()

	_ = s.conn.SetReadDeadline(time.Now().Add(geminiLivePongWait))
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(geminiLivePongWait))
	})
	pingDone := make(chan struct{})
	defer close(pingDone)
	go s.pingLoop(pingDone)

	for {
		_, payload, err := s.conn.ReadMessage()
		if err != nil {
			select {
			case <-s.stopping:
				s.diagnostics.metadata("upstream closed normally")
			default:
				s.diagnostics.connectionError("upstream read closed", err)
			}
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(geminiLivePongWait))
		var message geminiServerMessage
		if err := jsonUnmarshalGemini(payload, &message); err != nil {
			s.diagnostics.malformed(payload, err)
			s.emit(Event{Kind: EventError, RawType: "malformed", Text: err.Error()})
			continue
		}
		s.diagnostics.incomingMessage(payload, &message)
		if !s.handleMessage(message) {
			return
		}
	}
}

func (s *geminiSession) pingLoop(done <-chan struct{}) {
	ticker := time.NewTicker(geminiLivePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s.writeMu.Lock()
			err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(geminiLiveWriteWait))
			s.writeMu.Unlock()
			if err != nil {
				s.diagnostics.connectionError("ping failed", err)
				_ = s.conn.Close()
				return
			}
		}
	}
}

func (s *geminiSession) handleMessage(message geminiServerMessage) bool {
	if message.Error != nil {
		s.diagnostics.metadata("provider error code=%d status=%q", message.Error.Code, redactGeminiDiagnosticString(message.Error.Status))
		s.emit(Event{Kind: EventError, RawType: "error", Text: geminiErrorText(message.Error)})
		return false
	}
	if message.GoAway != nil {
		s.emit(Event{Kind: EventError, RawType: "goAway", Text: "Gemini Live session is ending soon"})
	}
	if message.ToolCall != nil {
		s.handleToolCalls(message.ToolCall.FunctionCalls)
	}
	if message.ToolCallCancellation != nil {
		s.handleToolCancellations(message.ToolCallCancellation.IDs)
	}
	if content := message.ServerContent; content != nil {
		s.handleServerContent(content)
	}
	return true
}

func (s *geminiSession) handleServerContent(content *geminiServerContent) {
	// Interim results are revised snapshots, not append-only transcript deltas.
	// Prefer the authoritative result if both fields arrive in the same frame.
	if content.InputTranscription != nil && content.InputTranscription.Text != "" {
		s.interimUserTranscript = false
		text := content.InputTranscription.Text
		s.userTranscript.WriteString(text)
		s.emit(Event{Kind: EventUserTranscript, RawType: "serverContent.inputTranscription", Role: RoleUser, Text: text})
	} else if content.InterimInputTranscription != nil {
		s.interimUserTranscript = true
		s.emit(Event{Kind: EventUserTranscriptInterim, RawType: "serverContent.interimInputTranscription", Role: RoleUser, Text: content.InterimInputTranscription.Text})
	}
	if content.OutputTranscription != nil && content.OutputTranscription.Text != "" {
		text := content.OutputTranscription.Text
		s.assistantTranscript.WriteString(text)
		s.emit(Event{Kind: EventAssistantTranscript, RawType: "serverContent.outputTranscription", Role: RoleAssistant, Text: text})
	}
	if content.Interrupted {
		s.assistantTranscript.Reset()
		if !s.replaceQueuedPCMWithFlush() {
			return
		}
		s.emit(Event{Kind: EventInterrupted, RawType: "serverContent.interrupted"})
	}
	if content.ModelTurn != nil {
		for _, part := range content.ModelTurn.Parts {
			if part.InlineData == nil || len(part.InlineData.Data) == 0 {
				continue
			}
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(part.InlineData.MIMEType)), "audio/pcm") {
				continue
			}
			audio := append([]byte(nil), part.InlineData.Data...)
			if len(audio) > geminiLiveMaxMessageBytes || len(audio)%2 != 0 {
				s.emit(Event{Kind: EventError, RawType: "serverContent.modelTurn", Text: "Gemini Live returned an invalid PCM audio frame"})
				continue
			}
			if !s.emitPCM(PCMFrame{Audio: audio}) {
				s.emit(Event{Kind: EventError, RawType: "audio", Text: "Gemini Live audio playback queue overflowed"})
				_ = s.conn.Close()
				return
			}
		}
	}
	if content.TurnComplete {
		if s.interimUserTranscript {
			s.emit(Event{Kind: EventUserTranscriptInterim, Role: RoleUser})
			s.interimUserTranscript = false
		}
		if text := strings.TrimSpace(s.userTranscript.String()); text != "" {
			s.emit(Event{Kind: EventTurnDone, RawType: "serverContent.turnComplete", Role: RoleUser, Text: text})
		}
		if text := strings.TrimSpace(s.assistantTranscript.String()); text != "" {
			s.emit(Event{Kind: EventTurnDone, RawType: "serverContent.turnComplete", Role: RoleAssistant, Text: text})
		}
		s.userTranscript.Reset()
		s.assistantTranscript.Reset()
	}
}

func (s *geminiSession) handleToolCalls(calls []geminiFunctionCall) {
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		input, err := geminiDelegationInput(call)
		if id == "" {
			err = errors.New("Gemini Live delegation is missing an id")
		}
		if err != nil {
			s.emit(Event{Kind: EventError, RawType: "toolCall", Text: err.Error()})
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		if _, duplicate := s.pending[id]; duplicate {
			s.mu.Unlock()
			continue
		}
		s.pending[id] = &geminiPendingDelegation{name: call.Name}
		s.mu.Unlock()
		s.emit(Event{Kind: EventDelegationCreated, RawType: "toolCall", DelegationID: id, Text: input})
	}
}

func (s *geminiSession) handleToolCancellations(ids []string) {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		_, existed := s.pending[id]
		delete(s.pending, id)
		s.finished[id] = struct{}{}
		s.mu.Unlock()
		if existed {
			s.emit(Event{Kind: EventError, RawType: "toolCallCancellation", Text: fmt.Sprintf("Gemini Live cancelled delegation %s", id), ErrorHandled: true})
		}
	}
}

func (s *geminiSession) emit(event Event) bool {
	select {
	case s.events <- event:
		return true
	case <-s.stopping:
		return false
	}
}

func (s *geminiSession) replaceQueuedPCMWithFlush() bool {
	for {
		select {
		case _, ok := <-s.pcmFrames:
			if !ok {
				return false
			}
			// Audio queued before the interruption is obsolete.
		default:
			select {
			case s.pcmFrames <- PCMFrame{Flush: true}:
				return true
			case <-s.stopping:
				return false
			}
		}
	}
}

func (s *geminiSession) emitPCM(frame PCMFrame) bool {
	select {
	case s.pcmFrames <- frame:
		return true
	case <-s.stopping:
		return false
	default:
		return false
	}
}

func (s *geminiSession) send(ctx context.Context, message any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return errGeminiLiveEnded
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return writeGeminiJSON(ctx, s.conn, message)
}

func (s *geminiSession) SendPCM(ctx context.Context, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	if len(pcm) > geminiLiveMaxPCMBytes {
		return fmt.Errorf("live: PCM frame is too large (%d bytes)", len(pcm))
	}
	if len(pcm)%2 != 0 {
		return errors.New("live: PCM frame must contain complete 16-bit samples")
	}
	err := s.send(ctx, geminiClientMessage{RealtimeInput: &geminiRealtimeInput{Audio: &geminiInlineData{
		MIMEType: "audio/pcm;rate=16000", Data: pcm,
	}}})
	if err == nil {
		s.diagnostics.inputPCM(len(pcm))
	}
	return err
}

func (s *geminiSession) AppendText(ctx context.Context, text string) error {
	text = prefixUserText(text)
	if text == "" {
		return nil
	}
	return s.send(ctx, geminiClientMessage{RealtimeInput: &geminiRealtimeInput{Text: text}})
}

func (s *geminiSession) AppendDelegation(ctx context.Context, delegationID string, chunk DelegationChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delegationID = strings.TrimSpace(delegationID)
	if delegationID == "" {
		return errors.New("live: Gemini delegation is missing an id")
	}
	text := strings.TrimSpace(chunk.Text)
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errGeminiLiveEnded
	}
	if _, done := s.finished[delegationID]; done {
		return nil
	}
	pending := s.pending[delegationID]
	if pending == nil {
		return fmt.Errorf("live: Gemini delegation %q is unknown", delegationID)
	}
	if pending.text.Len() >= geminiLiveResultBytes {
		return nil
	}
	remaining := geminiLiveResultBytes - pending.text.Len()
	if len(text) > remaining {
		text = text[:remaining]
	}
	if pending.text.Len() > 0 {
		pending.text.WriteByte('\n')
	}
	if chunk.Channel == ChannelCommentary {
		pending.text.WriteString("[Progress update] ")
	}
	pending.text.WriteString(text)
	return nil
}

func (s *geminiSession) CompleteDelegation(ctx context.Context, delegationID string) error {
	delegationID = strings.TrimSpace(delegationID)
	if delegationID == "" {
		return errors.New("live: Gemini delegation is missing an id")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errGeminiLiveEnded
	}
	if _, done := s.finished[delegationID]; done {
		s.mu.Unlock()
		return nil
	}
	pending := s.pending[delegationID]
	if pending == nil {
		s.mu.Unlock()
		return fmt.Errorf("live: Gemini delegation %q is unknown", delegationID)
	}
	delete(s.pending, delegationID)
	s.finished[delegationID] = struct{}{}
	output := strings.TrimSpace(pending.text.String())
	name := pending.name
	s.mu.Unlock()
	if output == "" {
		output = "The execution turn completed without text output."
	}
	return s.send(ctx, geminiClientMessage{ToolResponse: &geminiToolResponse{FunctionResponses: []geminiFunctionResponse{{
		ID: delegationID, Name: name, Response: map[string]any{"result": output},
	}}}})
}

func (s *geminiSession) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.diagnostics.metadata("close requested")
		close(s.stopping)
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = s.send(closeCtx, geminiClientMessage{RealtimeInput: &geminiRealtimeInput{AudioStreamEnd: true}})
		s.markClosed()
		_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = s.conn.Close()
	})
	select {
	case <-s.done:
		s.diagnostics.metadata("close complete")
		return nil
	case <-ctx.Done():
		s.diagnostics.connectionError("close deadline", ctx.Err())
		return ctx.Err()
	}
}

func (s *geminiSession) markClosed() {
	s.mu.Lock()
	s.closed = true
	s.pending = nil
	s.mu.Unlock()
}

func geminiLiveVoices() []string {
	return []string{"Kore", "Puck", "Charon", "Fenrir", "Aoede", "Leda", "Orus", "Zephyr"}
}
