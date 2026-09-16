package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	liveSDPLimitBytes        = 64 << 10
	liveTextLimitBytes       = 8 << 10
	liveEventHistoryLimit    = 512
	liveSubscriberBuffer     = 64
	liveIdleCheckInterval    = 15 * time.Second
	liveCommentaryMinSpacing = 5 * time.Second
	liveStartTimeout         = 45 * time.Second
)

// Live event types streamed to the browser for one live call.
const (
	liveEventStarted    = "live.started"
	liveEventTranscript = "live.transcript"
	liveEventDelegation = "live.delegation"
	liveEventError      = "live.error"
	liveEventEnded      = "live.ended"
)

type liveSessionEvent struct {
	Sequence int            `json:"sequence"`
	Type     string         `json:"type"`
	Data     map[string]any `json:"data,omitempty"`
	At       time.Time      `json:"at"`
}

// liveSession binds one provider voice call to one chat session and fans its
// events out to the browser tabs watching it.
type liveSession struct {
	id        string
	sessionID string

	mu                sync.Mutex
	controller        *live.Controller
	voiceSession      live.VoiceSession
	capabilities      live.Capabilities
	cancel            context.CancelFunc
	lastActivity      time.Time
	ended             bool
	events            []liveSessionEvent
	eventHead         int
	nextSequence      int
	subscribers       map[int]chan liveSessionEvent
	subscriberDropped map[int]bool
	nextSubscriber    int
}

func newLiveSession(id, sessionID string) *liveSession {
	return &liveSession{
		id:                id,
		sessionID:         sessionID,
		lastActivity:      time.Now(),
		subscribers:       make(map[int]chan liveSessionEvent),
		subscriberDropped: make(map[int]bool),
	}
}

func (l *liveSession) appendEvent(eventType string, data map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended {
		// The stream is terminal: a browser that reconnects must not see events
		// published after the call ended.
		return
	}
	l.lastActivity = time.Now()
	l.nextSequence++
	event := liveSessionEvent{Sequence: l.nextSequence, Type: eventType, Data: data, At: time.Now().UTC()}
	if len(l.events) < liveEventHistoryLimit {
		l.events = append(l.events, event)
	} else {
		l.events[l.eventHead] = event
		l.eventHead = (l.eventHead + 1) % len(l.events)
	}
	if eventType == liveEventEnded {
		l.ended = true
	}
	for id, subscriber := range l.subscribers {
		select {
		case subscriber <- event:
		default:
			// Make the SSE handler reconnect and replay from its last
			// acknowledged sequence rather than silently losing events.
			l.subscriberDropped[id] = true
			close(subscriber)
			delete(l.subscribers, id)
		}
	}
}

func (l *liveSession) eventRangesLocked() ([]liveSessionEvent, []liveSessionEvent) {
	if l.eventHead == 0 {
		return l.events, nil
	}
	return l.events[l.eventHead:], l.events[:l.eventHead]
}

func (l *liveSession) subscribe(after int) ([]liveSessionEvent, int, <-chan liveSessionEvent, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var replay []liveSessionEvent
	first, second := l.eventRangesLocked()
	for _, events := range [][]liveSessionEvent{first, second} {
		for _, event := range events {
			if event.Sequence > after {
				replay = append(replay, event)
			}
		}
	}
	if l.ended {
		return replay, 0, nil, true
	}
	id := l.nextSubscriber
	l.nextSubscriber++
	ch := make(chan liveSessionEvent, liveSubscriberBuffer)
	l.subscribers[id] = ch
	return replay, id, ch, false
}

func (l *liveSession) unsubscribe(id int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch, ok := l.subscribers[id]; ok {
		delete(l.subscribers, id)
		close(ch)
	}
	delete(l.subscriberDropped, id)
}

// running returns the controller, or nil while the call is still starting.
func (l *liveSession) running() (*live.Controller, context.CancelFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.controller, l.cancel
}

// subscriberOverflowed reports whether a browser fell behind and had its
// stream closed, so the reconnect with ?after= is explained in the log.
func (l *liveSession) subscriberOverflowed(id int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.subscriberDropped[id] {
		return false
	}
	delete(l.subscriberDropped, id)
	return true
}

func (l *liveSession) touch() {
	l.mu.Lock()
	l.lastActivity = time.Now()
	l.mu.Unlock()
}

func (l *liveSession) idleFor(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return now.Sub(l.lastActivity)
}

func (l *liveSession) closeSubscribers() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended = true
	for id, ch := range l.subscribers {
		delete(l.subscribers, id)
		close(ch)
	}
}

// liveConfig returns the effective live settings.
func (s *serveServer) liveConfig() config.LiveConfig {
	if s == nil || s.cfgRef == nil {
		return config.LiveConfig{}
	}
	return s.cfgRef.Live
}

// liveFeatureEnabled reports whether live sessions may be started at all.
// Provider readiness is checked separately because it can change at runtime.
func (s *serveServer) liveFeatureEnabled() bool {
	if s == nil || !s.cfg.ui || s.sessionMgr == nil {
		return false
	}
	return s.liveConfig().Enabled
}

// liveProvider returns the shared provider, building it on first use. One
// instance per server keeps provider-side session accounting (and the Codex
// child processes it owns) in a single place that shutdown can close.
func (s *serveServer) liveProvider() (live.Provider, error) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.liveProviderInstance != nil {
		return s.liveProviderInstance, nil
	}
	cfg := s.liveConfig()
	build := live.NewProvider
	if s.liveProviderFactory != nil {
		build = s.liveProviderFactory
	}
	provider, err := build(cfg)
	if err != nil {
		return nil, err
	}
	s.liveProviderInstance = provider
	return provider, nil
}

// liveCapability describes live support for /v1/capabilities.
func (s *serveServer) liveCapability(ctx context.Context) map[string]any {
	cfg := s.liveConfig()
	provider := strings.TrimSpace(cfg.Provider)
	if provider == "" {
		provider = config.DefaultLiveProvider
	}
	model := config.DefaultLiveChatGPTModel
	if provider == config.LiveProviderChatGPT {
		if configured := strings.TrimSpace(cfg.ChatGPT.Model); configured != "" {
			model = configured
		}
	}
	enabled := false
	if s.liveFeatureEnabled() {
		if p, err := s.liveProvider(); err == nil && p.Ready(ctx) == nil {
			enabled = true
		}
	}
	return map[string]any{
		"enabled": enabled, "provider": provider, "model": model,
		"transport": "webrtc", "version": 1,
	}
}

type liveStartRequest struct {
	SDP       string `json:"sdp"`
	SessionID string `json:"session_id"`
}

type liveTextRequest struct {
	Text string `json:"text"`
}

// handleLiveSessions starts a live voice call bound to a chat session.
func (s *serveServer) handleLiveSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if !s.liveFeatureEnabled() {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live voice is not enabled")
		return
	}
	var request liveStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, liveSDPLimitBytes+liveTextLimitBytes)).Decode(&request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	// The offer is forwarded byte for byte: SDP requires every line, including
	// the last, to end with CRLF, so trimming it makes the provider's parser
	// fail with EOF.
	offer := request.SDP
	if strings.TrimSpace(offer) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "sdp is required")
		return
	}
	if len(offer) > liveSDPLimitBytes {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "sdp offer is too large")
		return
	}
	sessionID := strings.TrimSpace(request.SessionID)
	if sessionID == "" {
		sessionID = resolveRequestSessionID(r)
	}
	if sessionID == "" {
		sessionID = session.NewID()
	}
	if err := s.validateRequestSessionID(r.Context(), sessionID); err != nil {
		status, errorType, message := sessionIDErrorResponse(err)
		writeOpenAIError(w, status, errorType, message)
		return
	}

	provider, err := s.liveProvider()
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	if err := provider.Ready(r.Context()); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live voice is unavailable: "+err.Error())
		return
	}

	liveID := "live_" + session.NewID()
	record := newLiveSession(liveID, sessionID)
	record.capabilities = live.ConfigCapabilities(s.liveConfig())
	if err := s.registerLiveSession(record); err != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		return
	}

	startCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), liveStartTimeout)
	defer cancel()
	opts := s.liveSessionOptions(startCtx, sessionID, record.capabilities)
	providerSession, err := provider.Start(startCtx, offer, opts)
	if err != nil {
		s.removeLiveSession(liveID)
		writeOpenAIError(w, http.StatusBadGateway, "server_error", "could not start the live session: "+err.Error())
		return
	}

	if !s.startLiveController(record, providerSession) {
		// Negotiation may succeed after Stop or shutdown removed the reservation.
		// Teardown must not depend on the original HTTP request still being alive.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := providerSession.Close(closeCtx); err != nil {
			log.Printf("[live] close cancelled startup %s: %v", liveID, err)
		}
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "the live session was cancelled while starting")
		return
	}

	w.Header().Set("x-session-id", sessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"live_id": liveID, "session_id": sessionID, "sdp": providerSession.AnswerSDP(),
	})
}

// handleLiveSessionByID serves DELETE, /text, and /events for one live call.
func (s *serveServer) handleLiveSessionByID(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/live/sessions/"), "/")
	if path == "" {
		s.handleLiveSessions(w, r)
		return
	}
	liveID, action, _ := strings.Cut(path, "/")
	liveID = strings.TrimSpace(liveID)
	if liveID == "" {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	switch action {
	case "":
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", http.MethodDelete)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		s.stopLiveSession(r.Context(), liveID, "client")
		writeJSON(w, http.StatusOK, map[string]any{"live_id": liveID, "status": "ended"})
	case "text":
		s.handleLiveSessionText(w, r, liveID)
	case "events":
		s.handleLiveSessionEvents(w, r, liveID)
	default:
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
	}
}

func (s *serveServer) handleLiveSessionText(w http.ResponseWriter, r *http.Request, liveID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	record, ok := s.lookupLiveSession(liveID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	var request liveTextRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, liveTextLimitBytes)).Decode(&request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(request.Text) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "text is required")
		return
	}
	controller, _ := record.running()
	if controller == nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "the live session is still starting")
		return
	}
	if err := controller.AppendUserText(r.Context(), request.Text); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	record.touch()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *serveServer) handleLiveSessionEvents(w http.ResponseWriter, r *http.Request, liveID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	record, ok := s.lookupLiveSession(liveID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	restart.Passive(r.Context())
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is unsupported")
		return
	}
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	replay, subscriberID, events, ended := record.subscribe(after)
	if events != nil {
		defer record.unsubscribe(subscriberID)
	}
	setSSEHeaders(w)
	flusher.Flush()
	// A quiet call can go minutes without an event; heartbeats keep proxies and
	// mobile radios from dropping the stream.
	pingMu, stopPing := sseKeepalive(r.Context(), w, flusher, serveEventHeartbeat)
	defer stopPing()
	writeEvent := func(event liveSessionEvent) bool {
		data, err := json.Marshal(event.Data)
		if err != nil {
			return false
		}
		pingMu.Lock()
		defer pingMu.Unlock()
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, event := range replay {
		if !writeEvent(event) {
			return
		}
	}
	if ended {
		return
	}
	for {
		select {
		case event, open := <-events:
			if !open {
				if record.subscriberOverflowed(subscriberID) {
					log.Printf("[live] session %s subscriber %d fell behind; reconnecting from replay", liveID, subscriberID)
				}
				return
			}
			if !writeEvent(event) {
				return
			}
			if event.Type == liveEventEnded {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			return
		}
	}
}

// observe turns controller updates into browser events.
func (l *liveSession) observe(update live.Update) {
	switch update.Kind {
	case live.UpdateStarted:
		l.appendEvent(liveEventStarted, map[string]any{})
	case live.UpdateTranscript:
		l.appendEvent(liveEventTranscript, map[string]any{
			"role": update.Role, "text": update.Text, "final": update.Final,
		})
	case live.UpdateDelegation:
		data := map[string]any{"delegation_id": update.DelegationID, "state": string(update.State)}
		if strings.TrimSpace(update.Text) != "" {
			data["text"] = update.Text
		}
		l.appendEvent(liveEventDelegation, data)
	case live.UpdateError:
		l.appendEvent(liveEventError, map[string]any{"message": update.Text})
	case live.UpdateEnded:
		l.appendEvent(liveEventEnded, map[string]any{})
	}
}

// startLiveController publishes and starts the controller under the same lock
// that removes its reservation. Stop can therefore see either no controller
// (startup must close the provider) or a fully started, safe-to-close controller.
// In particular, Controller.Start must finish its WaitGroup setup before Close.
func (s *serveServer) startLiveController(record *liveSession, providerSession live.Session) bool {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.liveClosed || s.liveSessions[record.id] != record {
		return false
	}
	controllerCtx, controllerCancel := context.WithCancel(context.Background())
	controller := live.NewController(live.ControllerOptions{
		Session:   providerSession,
		Delegator: &serveLiveDelegator{server: s, sessionID: record.sessionID, live: record},
		Observer:  record.observe,
	})
	record.mu.Lock()
	record.voiceSession, _ = providerSession.(live.VoiceSession)
	record.capabilities.CanSetVoice = record.voiceSession != nil
	record.controller, record.cancel = controller, controllerCancel
	record.mu.Unlock()
	controller.Start(controllerCtx)
	go s.watchLiveIdle(record)
	return true
}

func (s *serveServer) registerLiveSession(record *liveSession) error {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.liveClosed {
		return errors.New("server is shutting down")
	}
	if s.liveSessions == nil {
		s.liveSessions = make(map[string]*liveSession)
		s.liveByChat = make(map[string]string)
	}
	if existing, ok := s.liveByChat[record.sessionID]; ok {
		if _, live := s.liveSessions[existing]; live {
			return errors.New("a live session is already active for this chat session")
		}
	}
	s.liveSessions[record.id] = record
	s.liveByChat[record.sessionID] = record.id
	return nil
}

func (s *serveServer) lookupLiveSession(liveID string) (*liveSession, bool) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	record, ok := s.liveSessions[liveID]
	return record, ok
}

func (s *serveServer) removeLiveSession(liveID string) *liveSession {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	record, ok := s.liveSessions[liveID]
	if !ok {
		return nil
	}
	delete(s.liveSessions, liveID)
	if s.liveByChat[record.sessionID] == liveID {
		delete(s.liveByChat, record.sessionID)
	}
	return record
}

// stopLiveSession ends a live call. It is safe to call for unknown ids so
// DELETE stays idempotent.
func (s *serveServer) stopLiveSession(ctx context.Context, liveID, reason string) {
	record := s.removeLiveSession(liveID)
	if record == nil {
		return
	}
	if reason == "idle" {
		record.appendEvent(liveEventError, map[string]any{"message": "The live session timed out."})
	}
	controller, cancel := record.running()
	if controller != nil {
		if err := controller.Close(ctx); err != nil {
			log.Printf("[live] close session %s: %v", liveID, err)
		}
	}
	if cancel != nil {
		cancel()
	}
	record.appendEvent(liveEventEnded, map[string]any{})
	record.closeSubscribers()
}

// watchLiveIdle closes a live call that has seen no traffic for the configured
// idle timeout, so an abandoned browser tab cannot hold the provider session
// (and the chat session's voice binding) open forever.
func (s *serveServer) watchLiveIdle(record *liveSession) {
	timeout := s.liveConfig().ResolvedIdleTimeout()
	ticker := time.NewTicker(liveIdleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
			if _, ok := s.lookupLiveSession(record.id); !ok {
				return
			}
			if record.idleFor(time.Now()) < timeout {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.stopLiveSession(ctx, record.id, "idle")
			cancel()
			return
		}
	}
}

// closeLiveSessions ends every live call. It runs before the session manager
// and store shut down so delegated turns cannot outlive their storage.
func (s *serveServer) closeLiveSessions(ctx context.Context) {
	s.liveMu.Lock()
	if s.liveClosed {
		s.liveMu.Unlock()
		return
	}
	s.liveClosed = true
	ids := make([]string, 0, len(s.liveSessions))
	for id := range s.liveSessions {
		ids = append(ids, id)
	}
	s.liveProviderInstance = nil
	s.liveMu.Unlock()
	for _, id := range ids {
		s.stopLiveSession(ctx, id, "shutdown")
	}
}

// serveLiveDelegator runs delegated voice requests as ordinary chat turns.
type serveLiveDelegator struct {
	server    *serveServer
	sessionID string
	live      *liveSession
}

// Steer admits a voice follow-up through the same durable, run-fenced path as
// typed steering. It never cancels a task or subscribes to its output again.
func (d *serveLiveDelegator) Steer(ctx context.Context, request live.DelegationRequest) (bool, error) {
	if strings.TrimSpace(request.Input) == "" {
		return false, errors.New("the voice delegation contained no request")
	}
	s := d.server
	if s == nil || s.sessionMgr == nil {
		return false, errors.New("the session runtime is unavailable")
	}
	manager := s.ensureResponseRuns()
	run := manager.activeRun(d.sessionID)
	if run == nil {
		return false, nil
	}
	runtime, ok := s.sessionMgr.Get(d.sessionID)
	if !ok {
		return false, nil
	}
	// A run is registered before its engine is ready for steering. Let the
	// controller retry rather than discard guidance in that startup window.
	runtime.interruptMu.Lock()
	ready := runtime.activeInterrupt != nil && runtime.activeInterrupt.responseRun == run
	runtime.interruptMu.Unlock()
	if !ready {
		return false, nil
	}
	id := strings.TrimSpace(request.ID)
	if id == "" {
		id = session.NewID()
	}
	if d.live != nil {
		id = d.live.id + ":" + id
	}
	id = "live-steering:" + id
	message := llm.UserText(live.DelegationPrompt(request.Input, request.TranscriptDelta))
	message.DisplayText = strings.TrimSpace(request.Input)
	var admissionErr error
	owned := manager.withExpectedActiveRun(d.sessionID, run.id, run.runEpoch, func() {
		_, _, admissionErr = runtime.InterruptMessage(ctx, message, message.DisplayText, id, nil, interruptDeliverySteer)
	})
	if !owned {
		return false, nil
	}
	if errors.Is(admissionErr, errSteeringRunFinished) {
		return false, nil
	}
	if admissionErr != nil {
		return false, admissionErr
	}
	if d.live != nil {
		d.live.touch()
	}
	return true, nil
}

// Run executes one delegation in the bound session and streams its speakable
// output back to the voice model.
func (d *serveLiveDelegator) Run(ctx context.Context, request live.DelegationRequest, emit func(live.DelegationChunk)) error {
	if strings.TrimSpace(request.Input) == "" {
		return errors.New("the voice delegation contained no request")
	}
	s := d.server
	if s == nil || s.sessionMgr == nil {
		return errors.New("the session runtime is unavailable")
	}
	if activeID := s.ensureResponseRuns().activeRunID(d.sessionID); activeID != "" {
		admitted, err := d.Steer(ctx, request)
		if err != nil {
			return err
		}
		if admitted {
			emit(live.DelegationChunk{Channel: live.ChannelCommentary, Text: "Guidance queued for the running task."})
			return nil
		}
		return live.ErrDelegationBusy
	}
	runtime, _, err := s.runtimeForRequest(ctx, d.sessionID)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			return live.ErrDelegationBusy
		}
		return err
	}
	previousResponseID := strings.TrimSpace(runtime.getLastResponseID())
	if previousResponseID == "" {
		previousResponseID = s.latestDurableResponseIDForSession(ctx, d.sessionID)
	}
	message := llm.UserText(live.DelegationPrompt(request.Input, request.TranscriptDelta))
	message.DisplayText = strings.TrimSpace(request.Input)
	req := s.buildResponsesLLMRequest(responsesCreateRequest{Model: runtime.defaultModel}, runtime, d.sessionID, true)
	options := startResponseRunOptions{previousResponseID: previousResponseID, uiSession: true, live: d.live}
	options.runtimeSetup = func(req *llm.Request) error {
		if d.live != nil {
			runtime.liveContext = d.live.executionContext
			tool := &tools.LiveSettingsTool{}
			// The tool is executable only with the call-bound context supplied above.
			// Keep its schema out of ordinary turns and preserve engine allowlists.
			runtime.engine.Tools().RegisterDeferred(tool)
			req.Tools = appendResponsePassthroughTools(req.Tools, []llm.ToolSpec{tool.Spec()}, nil)
		}
		return nil
	}
	run, err := s.startResponseRun(runtime, true, false, []llm.Message{message}, req, d.sessionID, options)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			return live.ErrDelegationBusy
		}
		return err
	}
	if d.live != nil {
		d.live.touch()
	}
	return d.streamRun(ctx, run, emit)
}

// streamRun consumes one run's event stream, reconnecting from the last
// sequence when a slow subscriber is dropped.
func (d *serveLiveDelegator) streamRun(ctx context.Context, run *responseRun, emit func(live.DelegationChunk)) error {
	reporter := &liveToolCommentary{emit: emit}
	after := int64(0)
	for {
		subscription := run.subscribe(after)
		if subscription.snapshotRequired {
			return errors.New("the response stream fell too far behind")
		}
		events := subscription.ch
		for _, event := range subscription.replay {
			after = event.Sequence
			done, err := d.applyRunEvent(event, emit, reporter)
			if done {
				if events != nil {
					run.unsubscribe(events)
				}
				return err
			}
		}
		if events == nil {
			return liveRunStatusError(subscription.status)
		}
		closed := false
		for !closed {
			select {
			case <-ctx.Done():
				run.unsubscribe(events)
				return ctx.Err()
			case event, open := <-events:
				if !open {
					closed = true
					continue
				}
				after = event.Sequence
				done, err := d.applyRunEvent(event, emit, reporter)
				if done {
					run.unsubscribe(events)
					return err
				}
				if d.live != nil {
					d.live.touch()
				}
			}
		}
		run.unsubscribe(events)
	}
}

// applyRunEvent maps one run event to voice output and reports whether the run
// reached a terminal state.
func (d *serveLiveDelegator) applyRunEvent(event responseRunEvent, emit func(live.DelegationChunk), reporter *liveToolCommentary) (bool, error) {
	switch event.Event {
	case "response.output_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &payload); err == nil && payload.Delta != "" {
			emit(live.DelegationChunk{Channel: live.ChannelSpeakable, Text: payload.Delta})
		}
	case "response.tool_exec.start":
		var payload struct {
			ToolName string `json:"tool_name"`
		}
		if err := json.Unmarshal(event.Data, &payload); err == nil {
			reporter.toolStarted(payload.ToolName)
		}
	case "response.approval.prompt":
		reporter.approvalRequested()
	case "response.completed":
		return true, nil
	case "response.cancelled":
		return true, errors.New("the request was cancelled")
	case "response.failed":
		return true, liveRunFailure(event.Data)
	}
	return false, nil
}

func liveRunFailure(data []byte) error {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &payload); err == nil {
		if message := strings.TrimSpace(payload.Error.Message); message != "" {
			return errors.New(message)
		}
		if message := strings.TrimSpace(payload.Message); message != "" {
			return errors.New(message)
		}
	}
	return errors.New("the request failed")
}

func liveRunStatusError(status string) error {
	switch status {
	case "completed", "":
		return nil
	case "cancelled":
		return errors.New("the request was cancelled")
	default:
		return errors.New("the request failed")
	}
}

// liveToolCommentary keeps spoken progress notes sparse: the voice model only
// needs enough to fill a silence, not a running tool log.
type liveToolCommentary struct {
	emit       func(live.DelegationChunk)
	lastTool   string
	lastSpoken time.Time
	approval   bool
}

func (c *liveToolCommentary) toolStarted(name string) {
	name = strings.TrimSpace(name)
	if name == "" || name == c.lastTool {
		return
	}
	if time.Since(c.lastSpoken) < liveCommentaryMinSpacing && c.lastTool != "" {
		return
	}
	c.lastTool = name
	c.lastSpoken = time.Now()
	c.emit(live.DelegationChunk{Channel: live.ChannelCommentary, Text: "Running " + name + "…"})
}

func (c *liveToolCommentary) approvalRequested() {
	if c.approval {
		return
	}
	c.approval = true
	c.emit(live.DelegationChunk{Channel: live.ChannelCommentary, Text: "Waiting for your approval in the app."})
}
