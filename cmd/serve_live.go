package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	liveSDPLimitBytes          = 64 << 10
	liveTextLimitBytes         = 8 << 10
	liveEventHistoryLimit      = 512
	liveSubscriberBuffer       = 64
	liveIdleCheckInterval      = 15 * time.Second
	liveStartTimeout           = 45 * time.Second
	liveAudioAttachTimeout     = 20 * time.Second
	liveAudioWriteTimeout      = 10 * time.Second
	liveAudioPongTimeout       = 45 * time.Second
	liveAudioPingInterval      = 15 * time.Second
	liveAudioFrameLimitBytes   = 64 << 10
	liveAudioSSEChunkBytes     = 24 << 10
	liveDiagnosticsMaxBytes    = 8 << 10
	liveDiagnosticsMaxCount    = 256
	liveDiagnosticsMinGap      = time.Second
	liveBindingAnnounceTimeout = 10 * time.Second
	liveSwitchBodyLimitBytes   = 4 << 10
	// liveDelegationContextLimitBytes bounds the client's device-capability hint.
	// It is client-authored prompt text that stays in the voice model's
	// instructions for the whole call, so it is kept to a short paragraph.
	liveDelegationContextLimitBytes = 2 << 10
	// liveDelegationResultLimitBytes bounds one client delegation result. Larger
	// output would be wasted: delegationWriter truncates to its head/tail policy
	// before any of it is spoken.
	liveDelegationResultLimitBytes = 64 << 10
	// liveClientDelegationResendInterval re-publishes a pending request. The event
	// ring holds 512 events and carries every interim transcript, so one publish is
	// not a guaranteed delivery to a client that reconnects late.
	liveClientDelegationResendInterval = 15 * time.Second
	liveClientDelegationTimeout        = 5 * time.Minute
	// liveClientDelegationHistoryLimit bounds the answered delegations kept per
	// call for idempotent replay. Delegations run one at a time, so this is a long
	// history in practice.
	liveClientDelegationHistoryLimit = 64
	liveDelegationFailureLimitBytes  = 1 << 10
)

// Delegation execution modes for POST /v1/live/sessions. An unknown mode is
// rejected rather than defaulted: a client that asks for a mode this host does
// not implement must learn that, not silently get the other one.
const (
	liveDelegationModeServer = "server"
	liveDelegationModeClient = "client"
)

// Live event types streamed to the browser for one live call.
const (
	liveEventStarted        = "live.started"
	liveEventTranscript     = "live.transcript"
	liveEventDelegation     = "live.delegation"
	liveEventInterrupted    = "live.interrupted"
	liveEventError          = "live.error"
	liveEventEnded          = "live.ended"
	liveEventSessionChanged = "live.session_changed"
	// liveEventDelegationRequested asks the authenticated client that owns this
	// call to execute one delegation. It is published only in client delegation
	// mode and never replaces the controller-owned live.delegation lifecycle.
	liveEventDelegationRequested = "live.delegation_requested"
)

type liveSessionEvent struct {
	Sequence int            `json:"sequence"`
	Type     string         `json:"type"`
	Data     map[string]any `json:"data,omitempty"`
	At       time.Time      `json:"at"`
}

// liveSession binds one provider voice call to one chat session and fans its
// events out to the browser tabs watching it. The binding is mutable: the call
// can switch which chat session it drives, so sessionID is guarded by mu.
type liveSession struct {
	id string

	mu                sync.Mutex
	sessionID         string
	controller        *live.Controller
	providerSession   live.Session
	voiceSession      live.VoiceSession
	pcmSession        live.PCMSession
	capabilities      live.Capabilities
	cancel            context.CancelFunc
	lastActivity      time.Time
	ended             bool
	audioTokenHash    [sha256.Size]byte
	audioTokenExpires time.Time
	audioTokenSet     bool
	audioAttached     bool
	audioActive       bool
	// delegationMode and delegationContext are fixed for the call: set from the
	// start request before registerLiveSession publishes the record, read-only
	// afterwards, so they need no lock.
	delegationMode    string
	delegationContext string
	// clientDelegations holds the newest liveClientDelegationHistoryLimit
	// delegations this call published to its client, in publish order, so a result
	// can be matched, a replay compared, and a shutdown can unblock waiters.
	clientDelegations []*liveClientDelegation
	events            []liveSessionEvent
	eventHead         int
	nextSequence      int
	subscribers       map[int]chan liveSessionEvent
	subscriberDropped map[int]bool
	nextSubscriber    int
	diagnosticLast    time.Time
	diagnosticReports int
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

// boundSession reports the chat session the call is bound to right now.
func (l *liveSession) boundSession() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessionID
}

// providerSessionRef returns the provider session for host-initiated writes.
// Callers must not hold l.mu while using it.
func (l *liveSession) providerSessionRef() live.Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.providerSession
}

func (l *liveSession) appendEvent(eventType string, data map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.appendEventLocked(eventType, data)
}

// appendBindingEvent publishes an event that names the call's bound chat session.
// The binding is read under the same lock that publishes the event: a rebind
// completes and publishes live.session_changed inside that critical section, so
// reading the binding with a separate acquisition would let a delegation event
// carry the session the call has just left and drag a watching browser back to it.
func (l *liveSession) appendBindingEvent(eventType string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	data["session_id"] = l.sessionID
	l.appendEventLocked(eventType, data)
}

// appendEventLocked publishes eventType while l.mu is already held. Callers that
// must publish inside a wider critical section (the rebind path, so two racing
// rebinds cannot publish out of order) use it to avoid re-locking.
func (l *liveSession) appendEventLocked(eventType string, data map[string]any) {
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
		// A terminal stream is terminal for the device too. The provider can end a
		// call with no host teardown running (a hangup, a provider error), and a
		// waiter left behind then holds the controller's delegation loop until its
		// timeout while the check above silently swallows its resends.
		for _, entry := range l.clientDelegations {
			entry.abandonLocked()
		}
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

func (l *liveSession) acceptDiagnostic(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended || l.diagnosticReports >= liveDiagnosticsMaxCount {
		return false
	}
	if !l.diagnosticLast.IsZero() && now.Sub(l.diagnosticLast) < liveDiagnosticsMinGap {
		return false
	}
	l.diagnosticLast = now
	l.diagnosticReports++
	return true
}

func (l *liveSession) idleFor(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return now.Sub(l.lastActivity)
}

func (l *liveSession) issueAudioToken(now time.Time, lifetime time.Duration) (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("generate audio capability: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	l.mu.Lock()
	l.audioTokenHash = sha256.Sum256([]byte(token))
	l.audioTokenExpires = now.Add(lifetime)
	l.audioTokenSet = true
	l.mu.Unlock()
	return token, nil
}

func (l *liveSession) claimAudio(token string, now time.Time) (live.PCMSession, bool) {
	hash := sha256.Sum256([]byte(token))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended || l.pcmSession == nil || !l.audioTokenSet || l.audioAttached || now.After(l.audioTokenExpires) {
		return nil, false
	}
	if subtle.ConstantTimeCompare(hash[:], l.audioTokenHash[:]) != 1 {
		return nil, false
	}
	l.audioTokenSet = false
	l.audioAttached = true
	l.audioActive = true
	l.lastActivity = now
	return l.pcmSession, true
}

func (l *liveSession) attachedAudio() (live.PCMSession, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ended || !l.audioActive || l.pcmSession == nil {
		return nil, false
	}
	return l.pcmSession, true
}

func (l *liveSession) releaseAudio() {
	l.mu.Lock()
	l.audioActive = false
	l.mu.Unlock()
}

func (l *liveSession) audioWasAttached() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.audioAttached
}

func (l *liveSession) closeSubscribers() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended = true
	l.audioActive = false
	for id, ch := range l.subscribers {
		delete(l.subscribers, id)
		close(ch)
	}
	// Teardown can reach here with no live.ended ever published — a call removed
	// before its controller started — so this is not redundant with the abandon in
	// appendEventLocked. Entries stay behind so a late POST is refused, not absorbed.
	for _, entry := range l.clientDelegations {
		entry.abandonLocked()
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
	capabilities := live.ConfigCapabilities(cfg)
	transport := "webrtc"
	if capabilities.Provider == config.LiveProviderGemini {
		// Browser WebSockets cannot traverse an existing reverse Hub connector.
		// Ordinary streamed HTTP can, and remains usable for direct nodes too.
		transport = "http_pcm"
	}
	enabled := false
	if s.liveFeatureEnabled() {
		if p, err := s.liveProvider(); err == nil && p.Ready(ctx) == nil {
			enabled = true
		}
	}
	// delegation_modes is additive and "version" deliberately stays at 3: the web
	// UI reads that number and gains nothing from client-owned delegation.
	return map[string]any{
		"enabled": enabled, "provider": capabilities.Provider, "model": capabilities.Model,
		"transport": transport, "version": 3,
		"delegation_modes": []string{liveDelegationModeServer, liveDelegationModeClient},
	}
}

type liveStartRequest struct {
	SDP            string `json:"sdp"`
	SessionID      string `json:"session_id"`
	AudioTransport string `json:"audio_transport"`
	// DelegationMode selects who executes this call's delegations: the host
	// ("server", the default) or the authenticated client that started the call
	// ("client").
	DelegationMode string `json:"delegation_mode"`
	// DelegationContext describes what the device executing delegations can do. It
	// is prompt text, not a tool schema: tool authority stays on the device, which
	// declares its real tools per request.
	DelegationContext string `json:"delegation_context"`
}

// liveDelegationStart validates the delegation half of a start request. Mode and
// context are validated together because the pairing is the security-relevant
// part: a capability hint accepted in server mode would make the host promise
// device tools no delegation can ever reach.
func liveDelegationStart(request liveStartRequest) (string, string, error) {
	mode := strings.TrimSpace(request.DelegationMode)
	switch mode {
	case "":
		mode = liveDelegationModeServer
	case liveDelegationModeServer, liveDelegationModeClient:
	default:
		return "", "", fmt.Errorf("delegation_mode must be %q or %q", liveDelegationModeServer, liveDelegationModeClient)
	}
	hint := strings.TrimSpace(request.DelegationContext)
	if len(hint) > liveDelegationContextLimitBytes {
		return "", "", fmt.Errorf("delegation_context must be at most %d bytes", liveDelegationContextLimitBytes)
	}
	if !liveDelegationContextIsPlainText(hint) {
		return "", "", errors.New("delegation_context must be plain text")
	}
	if hint != "" && mode != liveDelegationModeClient {
		return "", "", errors.New(`delegation_context is only accepted with "delegation_mode":"client"`)
	}
	return mode, hint, nil
}

// liveDelegationContextIsPlainText rejects control characters other than the
// newline a client may reasonably use to separate sentences. The prompt layer
// sanitises again; this is the transport saying no to bytes that only make sense
// as an attempt to forge structure in a prompt or a log line.
func liveDelegationContextIsPlainText(hint string) bool {
	if !utf8.ValidString(hint) {
		return false
	}
	for _, r := range hint {
		if r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

type liveTextRequest struct {
	Text string `json:"text"`
}

type liveSessionSwitchRequest struct {
	SessionID string `json:"session_id"`
}

type liveBrowserDiagnostics struct {
	Sequence           int64   `json:"sequence"`
	ElapsedMS          float64 `json:"elapsed_ms"`
	AudioContextState  string  `json:"audio_context_state"`
	StreamOpen         bool    `json:"stream_open"`
	PacketsReceived    int64   `json:"packets_received"`
	BytesReceived      int64   `json:"bytes_received"`
	PacketsScheduled   int64   `json:"packets_scheduled"`
	PacketsEnded       int64   `json:"packets_ended"`
	PlaybackQueuedSecs float64 `json:"playback_queued_seconds"`
	Underruns          int64   `json:"underruns"`
	FlushInterrupts    int64   `json:"flush_interrupts"`
	FlushBufferResets  int64   `json:"flush_buffer_resets"`
	InputPackets       int64   `json:"input_packets"`
	InputBytes         int64   `json:"input_bytes"`
	InputQueueDrops    int64   `json:"input_queue_drops"`
	PostCount          int64   `json:"post_count"`
	PostErrors         int64   `json:"post_errors"`
	PostLatencyTotalMS float64 `json:"post_latency_total_ms"`
	PostLatencyMaxMS   float64 `json:"post_latency_max_ms"`
	PostInflight       int64   `json:"post_inflight"`
	InputSilenceMS     float64 `json:"input_silence_ms"`
	OutputSilenceMS    float64 `json:"output_silence_ms"`
}

func (d liveBrowserDiagnostics) valid() bool {
	const dayMS = float64((24 * time.Hour) / time.Millisecond)
	const tenMinutesMS = float64((10 * time.Minute) / time.Millisecond)
	if d.Sequence < 0 || d.Sequence > 1_000_000 || !liveDiagnosticNumber(d.ElapsedMS, dayMS) ||
		!liveDiagnosticNumber(d.PlaybackQueuedSecs, 60) || !liveDiagnosticNumber(d.PostLatencyTotalMS, dayMS) ||
		!liveDiagnosticNumber(d.PostLatencyMaxMS, tenMinutesMS) ||
		!liveDiagnosticNumber(d.InputSilenceMS, dayMS) || !liveDiagnosticNumber(d.OutputSilenceMS, dayMS) {
		return false
	}
	for _, count := range []int64{d.PacketsReceived, d.BytesReceived, d.PacketsScheduled, d.PacketsEnded, d.Underruns,
		d.FlushInterrupts, d.FlushBufferResets, d.InputPackets, d.InputBytes, d.InputQueueDrops, d.PostCount, d.PostErrors, d.PostInflight} {
		if count < 0 || count > 1_000_000_000 {
			return false
		}
	}
	switch d.AudioContextState {
	case "running", "suspended", "interrupted", "closed", "unknown":
		return true
	default:
		return false
	}
}

func liveDiagnosticNumber(value, maximum float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= maximum
}

// handleLiveSessions starts a live voice call bound to a chat session.
// liveOfferProblem validates the SDP offer for the selected provider and
// returns the client-facing rejection, or "" when the offer is acceptable.
// Gemini uses server-proxied PCM and therefore has no SDP to check.
func liveOfferProblem(provider, offer string) string {
	if strings.TrimSpace(provider) == config.LiveProviderGemini {
		return ""
	}
	if strings.TrimSpace(offer) == "" {
		return "sdp is required"
	}
	if len(offer) > liveSDPLimitBytes {
		return "sdp offer is too large"
	}
	return ""
}

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
	// fail with EOF. Gemini uses server-proxied PCM and therefore has no SDP.
	offer := request.SDP
	if problem := liveOfferProblem(s.liveConfig().Provider, offer); problem != "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", problem)
		return
	}
	delegationMode, delegationContext, err := liveDelegationStart(request)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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
	record.delegationMode, record.delegationContext = delegationMode, delegationContext
	if err := s.registerLiveSession(record); err != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		return
	}

	startCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), liveStartTimeout)
	defer cancel()
	// Read from the record, which is where a future re-snapshot path would have to
	// find it: a start request is long gone by then.
	opts := s.liveSessionOptions(startCtx, sessionID, record.capabilities, record.delegationContext)
	opts.LiveID = liveID
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
	response := map[string]any{
		"live_id": liveID, "session_id": sessionID, "transport": "webrtc", "sdp": providerSession.AnswerSDP(),
		// Echoed so a client can tell it got the mode it asked for, not assume it.
		"delegation_mode": delegationMode,
	}
	if _, ok := providerSession.(live.PCMSession); ok {
		if enabled, _ := s.liveDebugOptions(); enabled {
			response["diagnostics"] = true
		}
		token, err := record.issueAudioToken(time.Now(), liveAudioAttachTimeout)
		if err != nil {
			s.stopLiveSession(context.Background(), liveID, "audio-token")
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "could not authorize the live audio transport")
			return
		}
		if request.AudioTransport == "http_pcm" {
			response["transport"] = "http_pcm"
			response["audio_capability"] = token
		} else {
			// Keep the direct WebSocket media path for older/direct clients. New
			// clients request HTTP PCM because reverse Hub tunnels do not carry
			// WebSocket upgrades or bidirectional streaming request bodies.
			response["transport"] = "websocket_pcm"
			response["audio_url"] = fmt.Sprintf("%s/v1/live/sessions/%s/audio?token=%s", s.cfg.basePath, liveID, token)
		}
		go s.watchLiveAudioAttachment(record)
	}
	writeJSON(w, http.StatusOK, response)
}

// handleLiveSessionByID serves every per-call route: DELETE, /text, /session,
// /events, the audio transports, /diagnostics, and one client delegation result.
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
	case "session":
		s.handleLiveSessionSwitch(w, r, liveID)
	case "events":
		s.handleLiveSessionEvents(w, r, liveID)
	case "audio":
		s.handleLiveSessionAudio(w, r, liveID)
	case "audio/output":
		s.handleLiveSessionAudioOutput(w, r, liveID)
	case "audio/input":
		s.handleLiveSessionAudioInput(w, r, liveID)
	case "diagnostics":
		s.handleLiveSessionDiagnostics(w, r, liveID)
	default:
		// The split above keeps the whole remainder in one string, so the delegation
		// result path arrives as "delegations/{delegation_id}/result" and is parsed
		// here rather than by adding a second Cut to every other action.
		if rest, ok := strings.CutPrefix(action, "delegations/"); ok {
			if delegationID, ok := strings.CutSuffix(rest, "/result"); ok {
				s.handleLiveDelegationResult(w, r, liveID, delegationID)
				return
			}
			// The call exists; it is the delegation sub-path that does not. Naming the
			// right entity keeps a client from retrying the call id.
			writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live delegation not found")
			return
		}
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

// handleLiveSessionSwitch moves a live call to another chat session. It shares
// switchLiveSession with the voice tool, so both consumers apply the same
// validation.
func (s *serveServer) handleLiveSessionSwitch(w http.ResponseWriter, r *http.Request, liveID string) {
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
	var request liveSessionSwitchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, liveSwitchBodyLimitBytes)).Decode(&request); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid request body: "+err.Error())
		return
	}
	target, err := s.switchLiveSession(r.Context(), record, request.SessionID)
	if err != nil {
		status, errorType, message := liveSwitchErrorResponse(err)
		writeOpenAIError(w, status, errorType, message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"live_id": liveID, "session_id": target.SessionID, "session_number": target.Number,
		"title": target.Title, "no_op": target.NoOp,
	})
}

// liveSwitchErrorResponse maps shared switch failures to HTTP. Internal
// failures keep their details out of the response body.
func liveSwitchErrorResponse(err error) (int, string, string) {
	switch {
	case errors.Is(err, errLiveTargetBusy), errors.Is(err, errLiveCallEnded):
		return http.StatusConflict, "conflict_error", err.Error()
	case errors.Is(err, errLiveSwitchInvalidTarget):
		return http.StatusBadRequest, "invalid_request_error", err.Error()
	default:
		log.Printf("[live] session switch failed: %v", err)
		return http.StatusInternalServerError, "server_error", "could not switch the live session"
	}
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
	if _, ok := w.(http.Flusher); !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is unsupported")
		return
	}
	flushController := http.NewResponseController(w)
	w = newStreamingResponseWriter(w, serveStreamWriteTimeout)
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	replay, subscriberID, events, ended := record.subscribe(after)
	if events != nil {
		defer record.unsubscribe(subscriberID)
	}
	setSSEHeaders(w)
	if err := flushLiveSSEResponse(flushController); err != nil {
		return
	}
	writeEvent := func(event liveSessionEvent) bool {
		return writeLiveSessionEvent(w, flushController, event)
	}
	for _, event := range replay {
		if !writeEvent(event) {
			return
		}
	}
	if ended {
		return
	}
	heartbeat := time.NewTicker(serveEventHeartbeat)
	defer heartbeat.Stop()
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
		case <-heartbeat.C:
			// Keepalive writes use the same per-write deadline as event writes. A
			// write timeout or disconnect ends this handler instead of leaving a
			// heartbeat goroutine retrying a dead client forever.
			if !writeLiveSSEKeepalive(w, flushController) {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			return
		}
	}
}

// writeLiveSessionEvent emits one buffered session event in SSE framing. It
// reports false once the client can no longer receive events, which ends the
// stream instead of retrying a dead connection.
func writeLiveSessionEvent(w io.Writer, controller *http.ResponseController, event liveSessionEvent) bool {
	data, err := json.Marshal(event.Data)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, data); err != nil {
		return false
	}
	return flushLiveSSEResponse(controller) == nil
}

// writeLiveSSEKeepalive emits an SSE comment so idle streams keep proxies and
// browsers from closing the response. It reports false when the client is gone.
func writeLiveSSEKeepalive(w io.Writer, controller *http.ResponseController) bool {
	if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
		return false
	}
	return flushLiveSSEResponse(controller) == nil
}

// flushLiveSSEResponse preserves flush errors, which http.Flusher cannot expose,
// while applying the same bounded write window as ordinary SSE writes. Some
// synthetic or proxied writers do not support deadlines; they still flush, but
// callers must not assume a timeout exists in that case.
func flushLiveSSEResponse(controller *http.ResponseController) error {
	deadlineSet := false
	if err := controller.SetWriteDeadline(time.Now().Add(serveStreamWriteTimeout)); err == nil {
		deadlineSet = true
	} else if !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if deadlineSet {
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	}
	return controller.Flush()
}

func (s *serveServer) handleLiveSessionAudioRoute(w http.ResponseWriter, r *http.Request) {
	liveID := strings.TrimSpace(r.PathValue("liveID"))
	if liveID == "" {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	s.handleLiveSessionAudio(w, r, liveID)
}

func (s *serveServer) handleLiveSessionAudio(w http.ResponseWriter, r *http.Request, liveID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if (origin == "" && fetchSite != "same-origin") || !sameOriginShellRequest(r) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_origin", "live audio must come from the first-party UI origin")
		return
	}
	record, ok := s.lookupLiveSession(liveID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
	pcmSession, ok := record.claimAudio(r.URL.Query().Get("token"), time.Now())
	if !ok {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "invalid or expired live audio capability")
		return
	}
	defer func() {
		record.releaseAudio()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.stopLiveSession(ctx, liveID, "audio-disconnect")
	}()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	conn.SetReadLimit(liveAudioFrameLimitBytes)
	_ = conn.SetReadDeadline(time.Now().Add(liveAudioPongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(liveAudioPongTimeout))
	})
	readErr := readLiveAudioInput(r.Context(), conn, pcmSession, record)

	frames := pcmSession.PCMFrames()
	ping := time.NewTicker(liveAudioPingInterval)
	defer ping.Stop()
	for {
		select {
		case frame, open := <-frames:
			if !open {
				return
			}
			if !writeLiveAudioFrame(conn, record, frame) {
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(liveAudioWriteTimeout)); err != nil {
				return
			}
		case <-readErr:
			return
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			return
		}
	}
}

// readLiveAudioInput forwards browser microphone frames to the provider until
// the socket fails or a frame breaks the PCM framing contract. The returned
// channel carries the terminating error so the writer loop can unwind.
func readLiveAudioInput(ctx context.Context, conn *websocket.Conn, pcmSession live.PCMSession, record *liveSession) <-chan error {
	readErr := make(chan error, 1)
	go func() {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(liveAudioPongTimeout))
			if messageType != websocket.BinaryMessage || len(payload) == 0 {
				continue
			}
			if len(payload) > liveAudioFrameLimitBytes || len(payload)%2 != 0 {
				readErr <- errors.New("invalid live PCM input frame")
				return
			}
			if err := pcmSession.SendPCM(ctx, payload); err != nil {
				readErr <- err
				return
			}
			record.touch()
		}
	}()
	return readErr
}

// writeLiveAudioFrame sends one provider frame to the browser, split to the
// socket frame limit and kept on 16-bit sample boundaries. It reports false
// once the connection fails, which ends the stream.
func writeLiveAudioFrame(conn *websocket.Conn, record *liveSession, frame live.PCMFrame) bool {
	if frame.Flush {
		if err := writeLiveAudioControl(conn, map[string]string{"type": "interrupt"}); err != nil {
			return false
		}
	}
	audio := completeLivePCMSamples(frame.Audio)
	for len(audio) > 0 {
		size := min(len(audio), liveAudioFrameLimitBytes)
		if size%2 != 0 {
			size--
		}
		if size == 0 {
			break
		}
		_ = conn.SetWriteDeadline(time.Now().Add(liveAudioWriteTimeout))
		err := conn.WriteMessage(websocket.BinaryMessage, audio[:size])
		_ = conn.SetWriteDeadline(time.Time{})
		if err != nil {
			return false
		}
		audio = audio[size:]
		record.touch()
	}
	return true
}

func completeLivePCMSamples(pcm []byte) []byte {
	if len(pcm)%2 != 0 {
		return pcm[:len(pcm)-1]
	}
	return pcm
}

func writeLiveAudioControl(conn *websocket.Conn, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(time.Now().Add(liveAudioWriteTimeout))
	err = conn.WriteMessage(websocket.TextMessage, payload)
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func liveHTTPAudioCapability(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Term-LLM-Live-Audio-Capability"))
}

// writeLiveAudioSSEEvent emits one named audio event and flushes it. It reports
// false once the client can no longer receive audio.
func writeLiveAudioSSEEvent(w io.Writer, controller *http.ResponseController, event string, data any) bool {
	payload, err := json.Marshal(data)
	if err != nil {
		return false
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return false
	}
	return flushLiveSSEResponse(controller) == nil
}

// writeLiveAudioSSEFrame emits one provider frame as base64 audio events, split
// to the SSE chunk limit and kept on 16-bit sample boundaries. An interrupt
// marker precedes the audio so the browser can drop stale playback first.
func writeLiveAudioSSEFrame(w io.Writer, controller *http.ResponseController, record *liveSession, frame live.PCMFrame) bool {
	if frame.Flush && !writeLiveAudioSSEEvent(w, controller, "interrupt", map[string]any{}) {
		return false
	}
	audio := completeLivePCMSamples(frame.Audio)
	for len(audio) > 0 {
		size := min(len(audio), liveAudioSSEChunkBytes)
		if size%2 != 0 {
			size--
		}
		if size == 0 {
			break
		}
		encoded := base64.StdEncoding.EncodeToString(audio[:size])
		if !writeLiveAudioSSEEvent(w, controller, "audio", map[string]string{"audio": encoded}) {
			return false
		}
		audio = audio[size:]
		record.touch()
	}
	return true
}

// handleLiveSessionAudioOutput streams base64-encoded 24 kHz PCM and interrupt
// markers over an ordinary HTTP response. Unlike WebSocket upgrades, this path
// traverses deployed reverse Hub connectors and WebRTC HTTP tunnels unchanged.
func (s *serveServer) handleLiveSessionAudioOutput(w http.ResponseWriter, r *http.Request, liveID string) {
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
	pcmSession, ok := record.claimAudio(liveHTTPAudioCapability(r), time.Now())
	if !ok {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_session", "invalid or expired live audio capability")
		return
	}
	defer func() {
		record.releaseAudio()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.stopLiveSession(ctx, liveID, "audio-disconnect")
	}()
	if _, ok := w.(http.Flusher); !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is unsupported")
		return
	}
	flushController := http.NewResponseController(w)
	w = newStreamingResponseWriter(w, serveStreamWriteTimeout)
	restart.Passive(r.Context())
	setSSEHeaders(w)
	w.Header().Set("X-Accel-Buffering", "no")
	if err := flushLiveSSEResponse(flushController); err != nil {
		return
	}
	// A body-bearing first event forces response headers through reverse Hub and
	// other buffering proxies so browser fetch resolves before microphone input.
	if !writeLiveAudioSSEEvent(w, flushController, "ready", map[string]any{}) {
		return
	}

	frames := pcmSession.PCMFrames()
	heartbeat := time.NewTicker(serveEventHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case frame, open := <-frames:
			if !open {
				return
			}
			if !writeLiveAudioSSEFrame(w, flushController, record, frame) {
				return
			}
		case <-heartbeat.C:
			if !writeLiveSSEKeepalive(w, flushController) {
				return
			}
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			return
		}
	}
}

// handleLiveSessionAudioInput accepts one finite microphone batch. Browsers
// serialize these small requests; reverse Hub requests therefore never require
// an unsupported infinite or bidirectional upload body.
func (s *serveServer) handleLiveSessionAudioInput(w http.ResponseWriter, r *http.Request, liveID string) {
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
	pcmSession, attached := record.attachedAudio()
	if !attached {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "live audio output is not attached")
		return
	}
	if r.ContentLength > liveAudioFrameLimitBytes {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "live PCM input batch is too large")
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, liveAudioFrameLimitBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "live PCM input batch is too large")
		return
	}
	if len(payload) == 0 || len(payload)%2 != 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "live PCM input must be non-empty mono int16 data")
		return
	}
	if err := pcmSession.SendPCM(r.Context(), payload); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "server_error", "could not forward live PCM input: "+err.Error())
		return
	}
	record.touch()
	w.WriteHeader(http.StatusNoContent)
}

func (s *serveServer) handleLiveSessionDiagnostics(w http.ResponseWriter, r *http.Request, liveID string) {
	// Check the opt-in before reading, decoding, or looking up anything:
	// disabled diagnostics have no ingestion or log-processing path.
	if enabled, _ := s.liveDebugOptions(); !enabled {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "live session not found")
		return
	}
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
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, liveDiagnosticsMaxBytes))
	decoder.DisallowUnknownFields()
	var report liveBrowserDiagnostics
	if err := decoder.Decode(&report); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid live diagnostics report")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid live diagnostics report")
		return
	}
	if !report.valid() {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid live diagnostics report")
		return
	}
	if !record.acceptDiagnostic(time.Now()) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// All values are fixed-schema booleans, enum values, or validated numbers;
	// browser-provided strings, URLs, transcripts, and tokens are never logged.
	log.Printf("[live browser] live_id=%s seq=%d elapsed_ms=%.0f audio_context=%s stream_open=%t packets_received=%d bytes_received=%d packets_scheduled=%d packets_ended=%d queued_seconds=%.3f underruns=%d flush_interrupt=%d flush_buffer_reset=%d input_packets=%d input_bytes=%d input_queue_drops=%d posts=%d post_errors=%d post_latency_total_ms=%.0f post_latency_max_ms=%.0f post_inflight=%d input_silence_ms=%.0f output_silence_ms=%.0f",
		record.id, report.Sequence, report.ElapsedMS, report.AudioContextState, report.StreamOpen,
		report.PacketsReceived, report.BytesReceived, report.PacketsScheduled, report.PacketsEnded, report.PlaybackQueuedSecs,
		report.Underruns, report.FlushInterrupts, report.FlushBufferResets, report.InputPackets, report.InputBytes, report.InputQueueDrops,
		report.PostCount, report.PostErrors, report.PostLatencyTotalMS, report.PostLatencyMaxMS, report.PostInflight,
		report.InputSilenceMS, report.OutputSilenceMS)
	w.WriteHeader(http.StatusNoContent)
}

// observe turns controller updates into browser events. Events that describe the
// call's binding carry session_id: the event ring buffer is bounded, so a
// reconnecting tab must be able to recover the current binding from any later
// event instead of relying on live.session_changed still being buffered. Those
// events are published through appendBindingEvent so the id they carry is the
// binding as of their sequence number, never an older snapshot.
func (l *liveSession) observe(update live.Update) {
	switch update.Kind {
	case live.UpdateStarted:
		l.appendBindingEvent(liveEventStarted, nil)
	case live.UpdateTranscript:
		l.appendEvent(liveEventTranscript, map[string]any{
			"role": update.Role, "text": update.Text, "final": update.Final, "interim": update.Interim,
		})
	case live.UpdateDelegation:
		data := map[string]any{"delegation_id": update.DelegationID, "state": string(update.State)}
		if strings.TrimSpace(update.Text) != "" {
			data["text"] = update.Text
		}
		l.appendBindingEvent(liveEventDelegation, data)
	case live.UpdateInterrupted:
		l.appendEvent(liveEventInterrupted, map[string]any{})
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
	// The delegator is chosen by the call's mode and never changes afterwards.
	var delegator live.Delegator = &serveLiveDelegator{server: s, live: record}
	if record.delegationMode == liveDelegationModeClient {
		delegator = &serveLiveClientDelegator{
			server: s, live: record,
			resendInterval: liveClientDelegationResendInterval,
			timeout:        liveClientDelegationTimeout,
		}
	}
	options := live.ControllerOptions{
		Session:   providerSession,
		Delegator: delegator,
		Observer:  record.observe,
	}
	// Without a router every delegation is ordinary work for the bound session,
	// which is exactly the behaviour before the control plane existed. Leaving the
	// field nil is the whole of "off": no fast-model turn is made, nothing is
	// triaged, and no code path downstream behaves differently.
	liveCfg := s.liveConfig()
	if liveCfg.RouterEnabled() {
		switch liveCfg.ControlPlane {
		case config.LiveControlPlaneAgent:
			options.Router = &serveLiveControlExecutor{server: s, live: record}
		case config.LiveControlPlaneClassify:
			options.Router = s.newLiveClassifyRouter(record)
		}
	}
	controller := live.NewController(options)
	record.mu.Lock()
	record.providerSession = providerSession
	record.voiceSession, _ = providerSession.(live.VoiceSession)
	record.pcmSession, _ = providerSession.(live.PCMSession)
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
	bound := record.boundSession()
	if existing, ok := s.liveByChat[bound]; ok {
		if _, live := s.liveSessions[existing]; live {
			return errors.New("a live session is already active for this chat session")
		}
	}
	s.liveSessions[record.id] = record
	s.liveByChat[bound] = record.id
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
	// The binding is read while liveMu is held, so a concurrent rebind either
	// happened before this teardown (and is removed here) or refuses because the
	// call is already gone from liveSessions.
	if bound := record.boundSession(); s.liveByChat[bound] == liveID {
		delete(s.liveByChat, bound)
	}
	return record
}

// Live switch failures. Both the voice tool and the HTTP endpoint map these to
// their own transport errors, so validation never diverges between them.
var (
	// errLiveSwitchInvalidTarget rejects a selector that is missing, unknown, or
	// not a switchable chat session.
	errLiveSwitchInvalidTarget = errors.New("invalid live switch target")
	// errLiveTargetBusy rejects a target another live call is already driving.
	errLiveTargetBusy = errors.New("another live call is already bound to that chat session")
	// errLiveCallEnded rejects a switch on a call that is tearing down.
	errLiveCallEnded = errors.New("the live call has ended")
)

// liveSwitchTarget is a validated destination for a voice or UI rebind.
type liveSwitchTarget struct {
	SessionID    string
	Number       int64
	Title        string
	Project      string
	Archived     bool
	Running      bool
	RunningTask  string
	NoOp         bool
	LastActivity time.Time
}

// rebindLiveSession points a live call at another chat session and publishes
// live.session_changed. The caller must have resolved and validated target with
// no live locks held. It reports the previous binding so callers can tell a real
// move (previous != target.SessionID) from a no-op, which emits no event.
func (s *serveServer) rebindLiveSession(record *liveSession, target liveSwitchTarget) (string, error) {
	if record == nil || strings.TrimSpace(target.SessionID) == "" {
		return "", fmt.Errorf("%w: a session number or id is required", errLiveSwitchInvalidTarget)
	}
	targetID := strings.TrimSpace(target.SessionID)
	// Lock order is liveMu then record.mu, matching startLiveController.
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.liveClosed || s.liveSessions[record.id] != record {
		return "", errLiveCallEnded
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.ended {
		// The event stream is terminal, so a later binding change would be
		// invisible to every browser. Refuse instead of moving silently.
		return "", errLiveCallEnded
	}
	previous := record.sessionID
	if targetID == previous {
		return previous, nil
	}
	if other, hosted := s.liveByChat[targetID]; hosted && other != record.id {
		if _, live := s.liveSessions[other]; live {
			return "", errLiveTargetBusy
		}
	}
	delete(s.liveByChat, previous)
	s.liveByChat[targetID] = record.id
	record.sessionID = targetID
	record.lastActivity = time.Now()
	// Publish inside the same critical section so two racing rebinds cannot emit
	// out of order, and with the locked variant: appendEvent takes record.mu.
	record.appendEventLocked(liveEventSessionChanged, map[string]any{
		"session_id": targetID, "session_number": target.Number, "title": target.Title,
	})
	return previous, nil
}

// resolveLiveSwitchTarget validates a switch selector against the durable store
// with no live locks held. Only a durable session number or an exact session id
// is accepted: mapping "the reflow one" to a session is the calling agent's job
// (session_directory first, then confirm), so a misheard word can never move the
// call. A truncated id is exactly such a mishearing — it is rejected rather than
// resolved, because the store's prefix lookup returns the newest match with no
// uniqueness check and would silently bind the call to a different session.
func (s *serveServer) resolveLiveSwitchTarget(ctx context.Context, selector string) (liveSwitchTarget, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return liveSwitchTarget{}, fmt.Errorf("%w: a session number or session id is required", errLiveSwitchInvalidTarget)
	}
	if s == nil || s.store == nil {
		return liveSwitchTarget{}, errors.New("the session store is unavailable")
	}
	var (
		sess *session.Session
		err  error
	)
	if number, parseErr := strconv.ParseInt(strings.TrimPrefix(selector, "#"), 10, 64); parseErr == nil {
		sess, err = s.store.GetByNumber(ctx, number)
	} else {
		sess, err = s.store.Get(ctx, selector)
	}
	if err != nil {
		return liveSwitchTarget{}, fmt.Errorf("look up chat session %q: %w", selector, err)
	}
	if sess == nil {
		return liveSwitchTarget{}, fmt.Errorf("%w: no chat session matches %q", errLiveSwitchInvalidTarget, selector)
	}
	if sess.IsSubagent || strings.TrimSpace(sess.ParentID) != "" {
		// IsSubagent is a transient in-process flag; a durable parent link is the
		// stored form of the same "machine-generated child session" exclusion the
		// sidebar, search, and session_directory (ExcludeSubagents) apply, so a
		// rejected target is also one the directory can never offer. A parented
		// fork is not necessarily a subagent, so the message names both forms.
		return liveSwitchTarget{}, fmt.Errorf("%w: chat session %s is a child session (subagent or fork) and cannot be driven by voice", errLiveSwitchInvalidTarget, sess.ID)
	}
	lastActivity := sess.UpdatedAt
	if lastActivity.IsZero() {
		lastActivity = sess.CreatedAt
	}
	return liveSwitchTarget{
		SessionID: sess.ID, Number: sess.Number, Title: sess.PreferredShortTitle(),
		Project: sess.ProjectName, Archived: sess.Archived, LastActivity: lastActivity,
	}, nil
}

// switchLiveSession validates and performs one rebind for a live call. The voice
// tool and the HTTP endpoint both call this, so they share one validation path
// and cannot disagree about the binding.
func (s *serveServer) switchLiveSession(ctx context.Context, record *liveSession, selector string) (liveSwitchTarget, error) {
	if record == nil {
		return liveSwitchTarget{}, errLiveCallEnded
	}
	target, err := s.resolveLiveSwitchTarget(ctx, selector)
	if err != nil {
		return liveSwitchTarget{}, err
	}
	// Running state is read before the rebind: the caller must warn that the next
	// spoken request is routed as guidance to the target's running task instead
	// of being answered.
	if running := s.runningSessionIDs(ctx); running[target.SessionID] {
		target.Running = true
		target.RunningTask = s.lastUserMessageText(ctx, target.SessionID)
	}
	previous, err := s.rebindLiveSession(record, target)
	if err != nil {
		return liveSwitchTarget{}, err
	}
	target.NoOp = previous == target.SessionID
	if !target.NoOp {
		s.announceLiveBinding(ctx, record, target)
	}
	return target, nil
}

// lastUserMessageText returns the newest user-visible user request, so the agent
// can describe what a running session is working on.
func (s *serveServer) lastUserMessageText(ctx context.Context, sessionID string) string {
	if s == nil || s.store == nil || sessionID == "" {
		return ""
	}
	messages, err := s.getSessionMessagesPageDescending(ctx, sessionID, 0, 20)
	if err != nil {
		return ""
	}
	for _, message := range messages {
		if message.Role != llm.RoleUser {
			continue
		}
		if text := strings.TrimSpace(message.DisplayText()); text != "" {
			return text
		}
		if text := strings.TrimSpace(message.TextContent); text != "" {
			return text
		}
	}
	return ""
}

// announceLiveBinding pushes a short host note into the voice conversation
// naming the new binding. liveSessionOptions already told the model the original
// session_id, so without this note it keeps naming the old session out loud.
// The new session's history is deliberately not re-snapshotted: that would
// rewrite the spoken transcript and burn tokens.
func (s *serveServer) announceLiveBinding(ctx context.Context, record *liveSession, target liveSwitchTarget) {
	provider := record.providerSessionRef()
	if provider == nil {
		return
	}
	label := "chat session " + target.SessionID
	if target.Number > 0 {
		label = fmt.Sprintf("chat session #%d (%s)", target.Number, target.SessionID)
	}
	if title := strings.TrimSpace(target.Title); title != "" {
		label += fmt.Sprintf(", %q", title)
	}
	note := "Host note: this voice call is now bound to " + label + ". The previously bound session is no longer driven by voice; speak about the new session from now on."
	// The rebind has already happened, so the note must not be cancelled by a
	// disconnecting client. It is still bounded, and the lifecycle lock is not
	// held across the provider write.
	noteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), liveBindingAnnounceTimeout)
	defer cancel()
	if err := provider.AppendText(noteCtx, note); err != nil {
		log.Printf("[live] session %s binding note: %v", record.id, err)
	}
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

// watchLiveAudioAttachment prevents an authenticated but abandoned Gemini
// startup from retaining a provider connection indefinitely.
func (s *serveServer) watchLiveAudioAttachment(record *liveSession) {
	timer := time.NewTimer(liveAudioAttachTimeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		if _, ok := s.lookupLiveSession(record.id); !ok || record.audioWasAttached() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.stopLiveSession(ctx, record.id, "audio-attach-timeout")
		cancel()
	case <-s.shutdownCh:
	}
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

// serveLiveDelegator runs delegated voice requests as ordinary chat turns. It
// holds no cached session ID: the call's binding can change, and every
// delegation must resolve it exactly once so one delegation stays atomic with
// respect to a concurrent switch.
type serveLiveDelegator struct {
	server *serveServer
	live   *liveSession
}

// Steer admits a voice follow-up through the same durable, run-fenced path as
// typed steering. It never cancels a task or subscribes to its output again.
// The controller calls this directly, outside Run, so the binding is snapshotted
// here rather than by the caller.
func (d *serveLiveDelegator) Steer(ctx context.Context, request live.DelegationRequest) (bool, error) {
	sessionID := ""
	if d.live != nil {
		sessionID = d.live.boundSession()
	}
	return d.steerSession(ctx, sessionID, request)
}

// steerSession steers the task running in sessionID, which the caller resolved
// from the binding once.
func (d *serveLiveDelegator) steerSession(ctx context.Context, sessionID string, request live.DelegationRequest) (bool, error) {
	if strings.TrimSpace(request.Input) == "" {
		return false, errors.New("the voice delegation contained no request")
	}
	s := d.server
	if s == nil || s.sessionMgr == nil || sessionID == "" {
		return false, errors.New("the session runtime is unavailable")
	}
	manager := s.ensureResponseRuns()
	run := manager.activeRun(sessionID)
	if run == nil {
		return false, nil
	}
	runtime, ok := s.sessionMgr.Get(sessionID)
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
	owned := manager.withExpectedActiveRun(sessionID, run.id, run.runEpoch, func() {
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
// output back to the voice model. The binding is snapshotted once so the whole
// delegation, including any steering it admits for itself, targets one session
// even if the call switches mid-turn. The turn that was already running stays
// owned by the session it started in; the switch takes effect on the next one.
func (d *serveLiveDelegator) Run(ctx context.Context, request live.DelegationRequest, emit func(live.DelegationChunk)) error {
	if strings.TrimSpace(request.Input) == "" {
		return errors.New("the voice delegation contained no request")
	}
	s := d.server
	if s == nil || s.sessionMgr == nil {
		return errors.New("the session runtime is unavailable")
	}
	sessionID := ""
	if d.live != nil {
		sessionID = d.live.boundSession()
	}
	if sessionID == "" {
		return errors.New("the live call is not bound to a chat session")
	}
	if activeID := s.ensureResponseRuns().activeRunID(sessionID); activeID != "" {
		admitted, err := d.steerSession(ctx, sessionID, request)
		if err != nil {
			return err
		}
		if admitted {
			emit(live.DelegationChunk{Channel: live.ChannelQuiet, Text: "Guidance queued for the running task."})
			return nil
		}
		return live.ErrDelegationBusy
	}
	runtime, _, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			return live.ErrDelegationBusy
		}
		return err
	}
	previousResponseID := strings.TrimSpace(runtime.getLastResponseID())
	if previousResponseID == "" {
		previousResponseID = s.latestDurableResponseIDForSession(ctx, sessionID)
	}
	message := llm.UserText(live.DelegationPrompt(request.Input, request.TranscriptDelta))
	message.DisplayText = strings.TrimSpace(request.Input)
	req := s.buildResponsesLLMRequest(responsesCreateRequest{Model: runtime.defaultModel}, runtime, sessionID, true)
	options := startResponseRunOptions{previousResponseID: previousResponseID, uiSession: true, live: d.live}
	options.runtimeSetup = func(req *llm.Request) error {
		if d.live != nil {
			runtime.liveContext = d.live.executionContextFor(sessionID)
		}
		return nil
	}
	run, err := s.startResponseRun(runtime, true, false, []llm.Message{message}, req, sessionID, options)
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
// sequence when a slow subscriber is dropped. A ticker drives progress notes
// through quiet stretches, when no event arrives to prompt them.
func (d *serveLiveDelegator) streamRun(ctx context.Context, run *responseRun, emit func(live.DelegationChunk)) error {
	progress := newLiveProgress(emit, time.Now)
	ticker := time.NewTicker(liveProgressTick)
	defer ticker.Stop()
	after := int64(0)
	for {
		subscription := run.subscribe(after)
		if subscription.snapshotRequired {
			return errors.New("the response stream fell too far behind")
		}
		events := subscription.ch
		// A replayed backlog, and whatever reached the subscription meanwhile, is
		// folded before anything is announced, so a prompt answered within it is
		// never spoken as still pending.
		if done, err := d.applyRunBacklog(subscription.replay, events, &after, emit, progress); done {
			if events != nil {
				run.unsubscribe(events)
			}
			return err
		}
		progress.tick()
		if events == nil {
			return liveRunStatusError(subscription.status)
		}
		closed := false
		for !closed {
			select {
			case <-ctx.Done():
				run.unsubscribe(events)
				return ctx.Err()
			case <-ticker.C:
				// The ticker can win the select over queued events; fold them
				// first so the tick sees the state they leave.
				if done, err := d.applyRunBacklog(nil, events, &after, emit, progress); done {
					run.unsubscribe(events)
					return err
				}
				progress.tick()
			case event, open := <-events:
				if !open {
					closed = true
					continue
				}
				done, err := d.applyRunBacklog([]responseRunEvent{event}, events, &after, emit, progress)
				if done {
					run.unsubscribe(events)
					return err
				}
				progress.tick()
				if d.live != nil {
					d.live.touch()
				}
			}
		}
		run.unsubscribe(events)
	}
}

// liveRunDrainLimit bounds how many already-queued events one batch folds, so
// a stream that never pauses still lets progress tick.
const liveRunDrainLimit = 256

// applyRunBacklog applies events already in hand, then every event the
// subscription already holds, so progress is ticked on the state the batch
// leaves: a prompt and its answer that arrive together are never announced. It
// never waits for an event; a nil or closed channel ends the batch, and the
// caller's next receive sees the close.
func (d *serveLiveDelegator) applyRunBacklog(backlog []responseRunEvent, events <-chan responseRunEvent, after *int64, emit func(live.DelegationChunk), progress *liveProgress) (bool, error) {
	for _, event := range backlog {
		*after = event.Sequence
		if done, err := d.applyRunEvent(event, emit, progress); done {
			return true, err
		}
	}
	for range liveRunDrainLimit {
		select {
		case event, open := <-events:
			if !open {
				return false, nil
			}
			*after = event.Sequence
			if done, err := d.applyRunEvent(event, emit, progress); done {
				return true, err
			}
		default:
			return false, nil
		}
	}
	return false, nil
}

// applyRunEvent maps one run event to voice output and reports whether the run
// reached a terminal state. Progress state is updated but not ticked; the
// caller decides when notes are due.
func (d *serveLiveDelegator) applyRunEvent(event responseRunEvent, emit func(live.DelegationChunk), progress *liveProgress) (bool, error) {
	switch event.Event {
	case "response.output_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(event.Data, &payload); err == nil && payload.Delta != "" {
			emit(live.DelegationChunk{Channel: live.ChannelSpeakable, Text: payload.Delta})
		}
	case "response.completed":
		return true, nil
	case "response.cancelled":
		return true, errors.New("the request was cancelled")
	case "response.failed":
		return true, liveRunFailure(event.Data)
	}
	progress.observe(event)
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
