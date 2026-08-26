package llm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	responsesWebSocketBetaHeader               = "responses_websockets=2026-02-06"
	defaultResponsesWebSocketIdleTimeout       = 5 * time.Minute
	defaultResponsesWebSocketFirstEventTimeout = 15 * time.Second
	defaultResponsesWebSocketParkedTimeout     = 30 * time.Second
	defaultResponsesWebSocketMaxAge            = 55 * time.Minute
)

type responsesWebSocketState string

const (
	responsesWebSocketStateActive     responsesWebSocketState = "active"
	responsesWebSocketStatePinnedTool responsesWebSocketState = "pinned_tool"
	responsesWebSocketStateIdle       responsesWebSocketState = "idle"
	responsesWebSocketStateClosed     responsesWebSocketState = "closed"
)

type responsesWebSocketContinuationContextKey struct{}

func withResponsesWebSocketContinuationLifetime(ctx context.Context) context.Context {
	if ctx == nil {
		return ctx
	}
	return context.WithValue(ctx, responsesWebSocketContinuationContextKey{}, ctx)
}

func responsesWebSocketContinuationLifetime(ctx context.Context) context.Context {
	if ctx != nil {
		if lifetime, ok := ctx.Value(responsesWebSocketContinuationContextKey{}).(context.Context); ok && lifetime != nil {
			return lifetime
		}
	}
	return ctx
}

type responsesWebSocketFirstEventTimeoutError struct {
	timeout time.Duration
}

func (e *responsesWebSocketFirstEventTimeoutError) Error() string {
	return fmt.Sprintf("Responses WebSocket first response event timeout after %s", e.timeout)
}

type responsesWebSocketFrameTypeError struct {
	messageType int
}

func (e *responsesWebSocketFrameTypeError) Error() string {
	return fmt.Sprintf("Responses WebSocket returned unsupported frame type %d", e.messageType)
}

type responsesWebSocketConnection struct {
	conn  *websocket.Conn
	lease *responsesWebSocketLease

	readMu                 sync.Mutex
	readQueue              [][]byte
	readErr                error
	readReady              chan struct{}
	responseActive         bool
	responseEventReceived  bool
	idleTimeout            time.Duration
	idleTimer              *time.Timer
	idleGeneration         uint64
	firstEventTimeout      time.Duration
	firstEventTimer        *time.Timer
	firstEventGeneration   uint64
	parkedTimeout          time.Duration
	parkedTimer            *time.Timer
	parkedGeneration       uint64
	continuationResponseID string
	continuationCallIDs    map[string]struct{}
	continuationStop       chan struct{}
	ownerSessionID         string
	state                  responsesWebSocketState
	lastReuseFrom          responsesWebSocketState
	connectedAt            time.Time
	responseStartedAt      time.Time
	lastApplicationAt      time.Time
	applicationFrames      uint64
	lastControlAt          time.Time
	pingFrames             uint64
	pongFrames             uint64
}

func newResponsesWebSocketConnection(conn *websocket.Conn, lease *responsesWebSocketLease, ownerSessionID string, idleTimeout, firstEventTimeout, parkedTimeout time.Duration) *responsesWebSocketConnection {
	if idleTimeout == 0 {
		idleTimeout = defaultResponsesWebSocketIdleTimeout
	}
	if firstEventTimeout == 0 {
		firstEventTimeout = defaultResponsesWebSocketFirstEventTimeout
	}
	if parkedTimeout == 0 {
		parkedTimeout = defaultResponsesWebSocketParkedTimeout
	}
	now := time.Now()
	ws := &responsesWebSocketConnection{
		conn:              conn,
		lease:             lease,
		readReady:         make(chan struct{}, 1),
		idleTimeout:       idleTimeout,
		firstEventTimeout: firstEventTimeout,
		parkedTimeout:     parkedTimeout,
		ownerSessionID:    ownerSessionID,
		state:             responsesWebSocketStateIdle,
		connectedAt:       now,
	}
	pingHandler := conn.PingHandler()
	conn.SetPingHandler(func(appData string) error {
		ws.noteControlActivity(true)
		return pingHandler(appData)
	})
	conn.SetPongHandler(func(string) error {
		ws.noteControlActivity(false)
		return nil
	})
	if !lease.attach(ws) {
		ws.readErr = errors.New("Responses WebSocket lease was released before connection attachment")
		_ = conn.Close()
		return ws
	}
	// Bound the dial-to-start window as well as the post-response parked window.
	// The response goroutine disarms this timer in startResponse; if it is never
	// scheduled or is abandoned, the pool reservation cannot leak indefinitely.
	ws.readMu.Lock()
	ws.armParkedTimerLocked()
	ws.readMu.Unlock()

	go ws.readPump()
	return ws
}

func (c *responsesWebSocketConnection) readPump() {
	for {
		messageType, data, err := c.conn.ReadMessage()
		if err != nil {
			c.finishReading(err)
			_ = c.conn.Close()
			return
		}
		if messageType != websocket.TextMessage {
			err := &responsesWebSocketFrameTypeError{messageType: messageType}
			c.finishReading(err)
			_ = c.conn.Close()
			return
		}

		c.readMu.Lock()
		if !c.responseActive {
			err := errors.New("Responses WebSocket received an application frame while no response was active")
			c.failReadingLocked(err)
			c.readMu.Unlock()
			c.signalReadReady()
			_ = c.conn.Close()
			return
		}
		c.responseEventReceived = true
		c.lastApplicationAt = time.Now()
		c.applicationFrames++
		c.disarmFirstEventTimerLocked()
		c.readQueue = append(c.readQueue, data)
		c.readMu.Unlock()
		c.signalReadReady()
	}
}

func (c *responsesWebSocketConnection) reserveResponse(sessionID, previousResponseID string, input []ResponsesInputItem) bool {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readErr != nil || c.ownerSessionID != sessionID || c.state == responsesWebSocketStateClosed || c.state == responsesWebSocketStateActive {
		return false
	}
	if c.state == responsesWebSocketStateIdle && time.Since(c.connectedAt) >= defaultResponsesWebSocketMaxAge {
		return false
	}
	if c.state == responsesWebSocketStatePinnedTool && !c.matchesToolContinuationLocked(previousResponseID, input) {
		return false
	}
	if !c.lease.activate() {
		return false
	}
	c.lastReuseFrom = c.state
	c.disarmParkedTimerLocked()
	if c.state == responsesWebSocketStatePinnedTool {
		c.stopContinuationWatcherLocked()
		c.continuationResponseID = ""
		c.continuationCallIDs = nil
	}
	return true
}

func (c *responsesWebSocketConnection) matchesToolContinuationLocked(previousResponseID string, input []ResponsesInputItem) bool {
	if c.continuationResponseID == "" || previousResponseID != c.continuationResponseID || len(c.continuationCallIDs) == 0 {
		return false
	}
	outputs := make(map[string]struct{}, len(c.continuationCallIDs))
	for _, item := range input {
		if item.Type != "function_call_output" {
			continue
		}
		if _, expected := c.continuationCallIDs[item.CallID]; !expected {
			return false
		}
		outputs[item.CallID] = struct{}{}
	}
	if len(outputs) != len(c.continuationCallIDs) {
		return false
	}
	return true
}

func (c *responsesWebSocketConnection) stopContinuationWatcherLocked() {
	if c.continuationStop != nil {
		close(c.continuationStop)
		c.continuationStop = nil
	}
}

func (c *responsesWebSocketConnection) startResponse() error {
	c.readMu.Lock()
	c.disarmParkedTimerLocked()
	if !c.lease.activate() {
		c.readMu.Unlock()
		return errors.New("Responses WebSocket connection was evicted from the pool")
	}
	if c.readErr != nil {
		err := c.readErr
		c.readMu.Unlock()
		return err
	}
	if c.responseActive {
		c.readMu.Unlock()
		return errors.New("Responses WebSocket response already active")
	}
	if len(c.readQueue) != 0 {
		err := errors.New("Responses WebSocket had stale application frames before response start")
		c.failReadingLocked(err)
		c.readMu.Unlock()
		c.signalReadReady()
		_ = c.conn.Close()
		return err
	}
	c.responseActive = true
	c.state = responsesWebSocketStateActive
	c.responseEventReceived = false
	c.responseStartedAt = time.Now()
	c.lastApplicationAt = time.Time{}
	c.applicationFrames = 0
	c.lastControlAt = time.Time{}
	c.pingFrames = 0
	c.pongFrames = 0
	c.armIdleTimerLocked()
	c.armFirstEventTimerLocked()
	c.readMu.Unlock()
	return nil
}

func (c *responsesWebSocketConnection) retainForToolContinuation(ctx context.Context, responseID string, callIDs []string) bool {
	responseID = strings.TrimSpace(responseID)
	expected := make(map[string]struct{}, len(callIDs))
	for _, callID := range callIDs {
		if callID = strings.TrimSpace(callID); callID != "" {
			expected[callID] = struct{}{}
		}
	}
	if responseID == "" || len(expected) == 0 {
		return false
	}

	c.readMu.Lock()
	if !c.responseActive || c.readErr != nil {
		c.readMu.Unlock()
		return false
	}
	c.responseActive = false
	c.state = responsesWebSocketStatePinnedTool
	c.disarmIdleTimerLocked()
	c.disarmFirstEventTimerLocked()
	c.disarmParkedTimerLocked()
	stale := len(c.readQueue) != 0
	if stale {
		c.readQueue = nil
		c.failReadingLocked(errors.New("Responses WebSocket received trailing application frames after response completion"))
		c.readMu.Unlock()
		c.signalReadReady()
		_ = c.conn.Close()
		return false
	}
	c.stopContinuationWatcherLocked()
	c.continuationResponseID = responseID
	c.continuationCallIDs = expected
	stop := make(chan struct{})
	c.continuationStop = stop
	c.readMu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			_ = c.closeWithError(fmt.Errorf("Responses WebSocket tool continuation abandoned: %w", context.Cause(ctx)))
		case <-stop:
		}
	}()
	return true
}

func (c *responsesWebSocketConnection) finishResponse() {
	c.readMu.Lock()
	if !c.responseActive {
		c.readMu.Unlock()
		return
	}
	c.responseActive = false
	c.state = responsesWebSocketStateIdle
	c.disarmIdleTimerLocked()
	c.disarmFirstEventTimerLocked()
	c.disarmParkedTimerLocked()
	c.lease.park()
	stale := len(c.readQueue) != 0
	if stale {
		c.readQueue = nil
		c.failReadingLocked(errors.New("Responses WebSocket received trailing application frames after response completion"))
	}
	c.readMu.Unlock()
	if stale {
		c.signalReadReady()
		_ = c.conn.Close()
	}
}

func (c *responsesWebSocketConnection) noteControlActivity(ping bool) {
	c.readMu.Lock()
	if c.responseActive && c.readErr == nil {
		c.lastControlAt = time.Now()
		if ping {
			c.pingFrames++
		} else {
			c.pongFrames++
		}
	}
	c.readMu.Unlock()
}

func (c *responsesWebSocketConnection) noteApplicationProgress() {
	c.readMu.Lock()
	if c.responseActive && c.readErr == nil {
		c.armIdleTimerLocked()
	}
	c.readMu.Unlock()
}

type responsesWebSocketActivitySnapshot struct {
	ResponseActive    bool
	State             responsesWebSocketState
	LastReuseFrom     responsesWebSocketState
	OwnerSessionID    string
	ConnectedAt       time.Time
	ResponseStartedAt time.Time
	LastApplicationAt time.Time
	ApplicationFrames uint64
	LastControlAt     time.Time
	PingFrames        uint64
	PongFrames        uint64
	ReadError         string
}

func (c *responsesWebSocketConnection) activitySnapshot() responsesWebSocketActivitySnapshot {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	snapshot := responsesWebSocketActivitySnapshot{
		ResponseActive:    c.responseActive,
		State:             c.state,
		LastReuseFrom:     c.lastReuseFrom,
		OwnerSessionID:    c.ownerSessionID,
		ConnectedAt:       c.connectedAt,
		ResponseStartedAt: c.responseStartedAt,
		LastApplicationAt: c.lastApplicationAt,
		ApplicationFrames: c.applicationFrames,
		LastControlAt:     c.lastControlAt,
		PingFrames:        c.pingFrames,
		PongFrames:        c.pongFrames,
	}
	if c.readErr != nil {
		snapshot.ReadError = c.readErr.Error()
	}
	return snapshot
}

func (c *responsesWebSocketConnection) armIdleTimerLocked() {
	c.idleGeneration++
	generation := c.idleGeneration
	if c.idleTimer != nil {
		c.idleTimer.Stop()
	}
	c.idleTimer = time.AfterFunc(c.idleTimeout, func() {
		c.readMu.Lock()
		if !c.responseActive || c.readErr != nil || c.idleGeneration != generation {
			c.readMu.Unlock()
			return
		}
		c.failReadingLocked(fmt.Errorf("Responses WebSocket response idle timeout after %s", c.idleTimeout))
		c.readMu.Unlock()
		c.signalReadReady()
		_ = c.conn.Close()
	})
}

func (c *responsesWebSocketConnection) disarmIdleTimerLocked() {
	c.idleGeneration++
	if c.idleTimer != nil {
		c.idleTimer.Stop()
		c.idleTimer = nil
	}
}

func (c *responsesWebSocketConnection) armFirstEventTimerLocked() {
	c.firstEventGeneration++
	generation := c.firstEventGeneration
	if c.firstEventTimer != nil {
		c.firstEventTimer.Stop()
	}
	c.firstEventTimer = time.AfterFunc(c.firstEventTimeout, func() {
		c.readMu.Lock()
		if !c.responseActive || c.responseEventReceived || c.readErr != nil || c.firstEventGeneration != generation {
			c.readMu.Unlock()
			return
		}
		c.failReadingLocked(&responsesWebSocketFirstEventTimeoutError{timeout: c.firstEventTimeout})
		c.readMu.Unlock()
		c.signalReadReady()
		_ = c.conn.Close()
	})
}

func (c *responsesWebSocketConnection) disarmFirstEventTimerLocked() {
	c.firstEventGeneration++
	if c.firstEventTimer != nil {
		c.firstEventTimer.Stop()
		c.firstEventTimer = nil
	}
}

func (c *responsesWebSocketConnection) armParkedTimerLocked() {
	c.parkedGeneration++
	generation := c.parkedGeneration
	if c.parkedTimer != nil {
		c.parkedTimer.Stop()
	}
	c.parkedTimer = time.AfterFunc(c.parkedTimeout, func() {
		c.readMu.Lock()
		if c.responseActive || c.readErr != nil || c.parkedGeneration != generation {
			c.readMu.Unlock()
			return
		}
		c.failReadingLocked(fmt.Errorf("Responses WebSocket parked connection timeout after %s", c.parkedTimeout))
		c.readMu.Unlock()
		c.signalReadReady()
		_ = c.conn.Close()
	})
}

func (c *responsesWebSocketConnection) disarmParkedTimerLocked() {
	c.parkedGeneration++
	if c.parkedTimer != nil {
		c.parkedTimer.Stop()
		c.parkedTimer = nil
	}
}

func (c *responsesWebSocketConnection) failReadingLocked(err error) {
	if c.readErr == nil {
		c.readErr = err
	}
	c.responseActive = false
	c.state = responsesWebSocketStateClosed
	c.continuationResponseID = ""
	c.continuationCallIDs = nil
	c.stopContinuationWatcherLocked()
	c.disarmIdleTimerLocked()
	c.disarmFirstEventTimerLocked()
	c.disarmParkedTimerLocked()
	c.lease.release()
}

func (c *responsesWebSocketConnection) finishReading(err error) {
	c.readMu.Lock()
	c.failReadingLocked(err)
	c.readMu.Unlock()
	c.signalReadReady()
}

func (c *responsesWebSocketConnection) signalReadReady() {
	select {
	case c.readReady <- struct{}{}:
	default:
	}
}

func (c *responsesWebSocketConnection) nextMessage(ctx context.Context) ([]byte, error) {
	for {
		c.readMu.Lock()
		if len(c.readQueue) > 0 {
			data := c.readQueue[0]
			c.readQueue[0] = nil
			c.readQueue = c.readQueue[1:]
			if len(c.readQueue) == 0 {
				c.readQueue = nil
			}
			c.readMu.Unlock()
			return data, nil
		}
		err := c.readErr
		c.readMu.Unlock()
		if err != nil {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.readReady:
		}
	}
}

func (c *responsesWebSocketConnection) healthy() bool {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	return c.readErr == nil
}

func (c *responsesWebSocketConnection) close() error {
	return c.closeWithError(errors.New("Responses WebSocket connection closed"))
}

func (c *responsesWebSocketConnection) closeWithError(err error) error {
	c.readMu.Lock()
	c.failReadingLocked(err)
	c.readMu.Unlock()
	c.signalReadReady()
	return c.conn.Close()
}

func responsesWebSocketURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
		// Already a websocket URL.
	default:
		return "", fmt.Errorf("unsupported Responses WebSocket URL scheme %q", u.Scheme)
	}
	return u.String(), nil
}

type responsesWSRequest struct {
	Type string `json:"type"`

	Model              string                       `json:"model"`
	Instructions       string                       `json:"instructions,omitempty"`
	Input              []ResponsesInputItem         `json:"input"`
	Tools              []any                        `json:"tools,omitempty"`
	ToolChoice         any                          `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                        `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens    int                          `json:"max_output_tokens,omitempty"`
	Temperature        *float64                     `json:"temperature,omitempty"`
	TopP               *float64                     `json:"top_p,omitempty"`
	Reasoning          *ResponsesReasoning          `json:"reasoning,omitempty"`
	MultiAgent         *ResponsesMultiAgent         `json:"multi_agent,omitempty"`
	PromptCacheOptions *ResponsesPromptCacheOptions `json:"prompt_cache_options,omitempty"`
	Include            []string                     `json:"include,omitempty"`
	PromptCacheKey     string                       `json:"prompt_cache_key,omitempty"`
	Store              *bool                        `json:"store,omitempty"`
	Generate           *bool                        `json:"generate,omitempty"`
	StreamOptions      *ResponsesStreamOptions      `json:"stream_options,omitempty"`
	PreviousResponseID string                       `json:"previous_response_id,omitempty"`
	ServiceTier        string                       `json:"service_tier,omitempty"`
}

func newResponsesWSRequest(req ResponsesRequest) responsesWSRequest {
	return responsesWSRequest{
		Type:               "response.create",
		Model:              req.Model,
		Instructions:       req.Instructions,
		Input:              req.Input,
		Tools:              req.Tools,
		ToolChoice:         req.ToolChoice,
		ParallelToolCalls:  req.ParallelToolCalls,
		MaxOutputTokens:    req.MaxOutputTokens,
		Temperature:        req.Temperature,
		TopP:               req.TopP,
		Reasoning:          req.Reasoning,
		MultiAgent:         req.MultiAgent,
		PromptCacheOptions: req.PromptCacheOptions,
		Include:            req.Include,
		PromptCacheKey:     req.PromptCacheKey,
		Store:              req.Store,
		Generate:           req.Generate,
		StreamOptions:      req.StreamOptions,
		PreviousResponseID: req.PreviousResponseID,
		ServiceTier:        req.ServiceTier,
	}
}

func prepareResponsesWebSocketRequest(req ResponsesRequest, reused bool, debugRaw bool) ([]byte, error) {
	wsReq := newResponsesWSRequest(req)
	body, err := json.Marshal(wsReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Responses WebSocket request: %w", err)
	}
	if debugRaw {
		var prettyBody bytes.Buffer
		json.Indent(&prettyBody, body, "", "  ")
		DebugRawSection(debugRaw, fmt.Sprintf("Responses WebSocket Request (reused=%t)", reused), prettyBody.String())
	}
	return body, nil
}

func (c *ResponsesClient) writeResponsesWebSocketRequestLocked(conn *responsesWebSocketConnection, body []byte) error {
	writeTimeout := c.WebSocketWriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 30 * time.Second
	}
	if err := conn.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set Responses WebSocket write deadline: %w", err)
	}
	if err := conn.conn.WriteMessage(websocket.TextMessage, body); err != nil {
		return fmt.Errorf("write Responses WebSocket request: %w", err)
	}
	_ = conn.conn.SetWriteDeadline(time.Time{})
	return nil
}

func (c *ResponsesClient) streamWebSocketPrepared(ctx context.Context, req ResponsesRequest, buildContinuationInput func() []ResponsesInputItem, buildFullInput func() []ResponsesInputItem, debugRaw bool, responseStateGeneration uint64) (Stream, error) {
	parentCtx := responsesWebSocketContinuationLifetime(ctx)
	c.wsMu.Lock()

	fullRequestInput := append([]ResponsesInputItem(nil), buildFullInput()...)
	wireReq := c.prepareWebSocketContinuationLocked(req, buildContinuationInput, func() []ResponsesInputItem { return fullRequestInput })
	conn, reused, err := c.ensureWebSocket(ctx, wireReq)
	if err != nil {
		c.wsMu.Unlock()
		return nil, err
	}

	if c.WebSocketServerState && !reused {
		// ChatGPT's previous_response_id chain is local to a WebSocket. A fresh
		// connection cannot continue state created by the socket it replaced.
		c.clearLastResponseIDIfGeneration(responseStateGeneration, wireReq.SessionID, wireReq.PreviousResponseID)
		c.wsLastRequest = nil
		c.wsContinuationBaseline = nil
		wireReq.PreviousResponseID = ""
		wireReq.Input = fullRequestInput
	}

	body, err := prepareResponsesWebSocketRequest(wireReq, reused, debugRaw)
	if err != nil {
		c.wsMu.Unlock()
		return nil, err
	}

	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		defer c.wsMu.Unlock()
		if err := ctx.Err(); err != nil {
			c.discardWebSocketLocked()
			return err
		}
		if err := conn.startResponse(); err != nil {
			c.discardWebSocketLocked()
			return err
		}
		leaseAdmittedAtStart, leaseActiveAtStart, poolConnectionsAtStart := conn.lease.diagnosticSnapshot()
		responseActive := true
		defer func() {
			if responseActive {
				conn.finishResponse()
			}
		}()
		if err := c.writeResponsesWebSocketRequestLocked(conn, body); err != nil {
			c.discardWebSocketLocked()
			return err
		}

		ctxDone := make(chan struct{})
		var stopCtxWatcher sync.Once
		stopWatchingContext := func() {
			stopCtxWatcher.Do(func() { close(ctxDone) })
		}
		go func() {
			select {
			case <-ctx.Done():
				// If the stream has already reached a clean terminal event, the
				// caller may close the stream before this watcher is scheduled. In
				// that case both ctx.Done and ctxDone can be ready; prefer preserving
				// the reusable WebSocket instead of racing to close it.
				select {
				case <-ctxDone:
					return
				default:
				}
				_ = conn.close()
			case <-ctxDone:
			}
		}()
		defer stopWatchingContext()

		handler := newResponsesStreamEventHandler(c, responseStateGeneration, debugRaw, "Responses WebSocket", c.websocketServerStateEnabled(), wireReq.SessionID, wireReq.suppressReasoningSummaryDeltas())
		retriedFullState := false
		var (
			protocolEventCount    uint64
			meaningfulEventCount  uint64
			lastProtocolEvent     string
			lastProtocolEventAt   time.Time
			lastMeaningfulEvent   string
			lastMeaningfulEventAt time.Time
		)

		emitTerminationDiagnostic := func(readErr error) {
			now := time.Now()
			activity := conn.activitySnapshot()
			leaseAdmitted, leaseActive, poolConnections := conn.lease.diagnosticSnapshot()
			data := map[string]any{
				"transport":                         "responses_websocket",
				"error":                             readErr.Error(),
				"context_error":                     contextErrorString(ctx),
				"context_cause":                     contextCauseString(ctx),
				"session_id":                        wireReq.SessionID,
				"connection_reused":                 reused,
				"connection_reused_from_state":      activity.LastReuseFrom,
				"connection_state":                  activity.State,
				"connection_owner_session_id":       activity.OwnerSessionID,
				"connection_response_active":        activity.ResponseActive,
				"connection_read_error":             activity.ReadError,
				"pool_lease_admitted_at_start":      leaseAdmittedAtStart,
				"pool_lease_active_at_start":        leaseActiveAtStart,
				"pool_connections_for_key_at_start": poolConnectionsAtStart,
				"pool_lease_admitted_at_diagnostic": leaseAdmitted,
				"pool_lease_active_at_diagnostic":   leaseActive,
				"pool_connections_at_diagnostic":    poolConnections,
				"previous_response_id_present":      wireReq.PreviousResponseID != "",
				"protocol_event_count":              protocolEventCount,
				"last_protocol_event":               lastProtocolEvent,
				"meaningful_event_count":            meaningfulEventCount,
				"last_meaningful_event":             lastMeaningfulEvent,
				"application_frame_count":           activity.ApplicationFrames,
				"ping_frame_count":                  activity.PingFrames,
				"pong_frame_count":                  activity.PongFrames,
			}
			addDebugAgeMillis(data, "connection_age_ms", now, activity.ConnectedAt)
			addDebugAgeMillis(data, "response_elapsed_ms", now, activity.ResponseStartedAt)
			addDebugAgeMillis(data, "last_application_frame_age_ms", now, activity.LastApplicationAt)
			addDebugAgeMillis(data, "last_control_frame_age_ms", now, activity.LastControlAt)
			addDebugAgeMillis(data, "last_protocol_event_age_ms", now, lastProtocolEventAt)
			addDebugAgeMillis(data, "last_meaningful_event_age_ms", now, lastMeaningfulEventAt)
			emitDebugDiagnostic(ctx, "responses_websocket_terminated", data)
			if debugRaw {
				if encoded, err := json.MarshalIndent(data, "", "  "); err == nil {
					DebugRawSection(true, "Responses WebSocket Termination Diagnostic", string(encoded))
				}
			}
		}

		for {
			data, err := conn.nextMessage(ctx)
			if err != nil {
				emitTerminationDiagnostic(err)
				c.discardWebSocketLocked()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				var frameTypeErr *responsesWebSocketFrameTypeError
				if errors.As(err, &frameTypeErr) {
					return frameTypeErr
				}
				if finishErr := handler.FinishIncomplete(send); finishErr != nil {
					return &StreamIncompleteError{Transport: "Responses WebSocket", Terminal: "response.completed", Err: finishErr}
				}
				return &StreamIncompleteError{Transport: "Responses WebSocket", Terminal: "response.completed", Err: err}
			}

			eventType, err := responsesJSONEventType(data)
			if err != nil {
				c.discardWebSocketLocked()
				return fmt.Errorf("decode Responses WebSocket event envelope: %w", err)
			}
			protocolEventCount++
			lastProtocolEvent = eventType
			lastProtocolEventAt = time.Now()
			if responsesWebSocketEventIsMeaningfulProgress(eventType) {
				meaningfulEventCount++
				lastMeaningfulEvent = eventType
				lastMeaningfulEventAt = lastProtocolEventAt
				conn.noteApplicationProgress()
			}
			completed, err := handler.HandleJSONEvent(data, eventType, send)
			if err != nil {
				if (eventType == "response.completed" || eventType == "response.incomplete") && !json.Valid(data) {
					c.discardWebSocketLocked()
					if finishErr := handler.FinishIncomplete(send); finishErr != nil {
						err = errors.Join(err, finishErr)
					}
					return &StreamIncompleteError{
						Transport: "Responses WebSocket",
						Terminal:  eventType,
						Err:       err,
					}
				}
				if wsErr, ok := err.(*responsesAPIEventError); ok && wsErr.APIError != nil {
					switch wsErr.APIError.Code {
					case "previous_response_not_found":
						c.clearLastResponseIDIfGeneration(responseStateGeneration, wireReq.SessionID, wireReq.PreviousResponseID)
					case "websocket_connection_limit_reached":
						// The documented 60-minute connection limit is recovered by
						// dropping the socket; the next Stream call reconnects lazily.
					}
				}
				if !retriedFullState && !handler.Emitted() && wireReq.PreviousResponseID != "" && isPreviousResponseIDRejected(err) {
					retriedFullState = true
					c.clearLastResponseIDIfGeneration(responseStateGeneration, wireReq.SessionID, wireReq.PreviousResponseID)
					c.wsLastRequest = nil
					c.wsContinuationBaseline = nil
					wireReq.PreviousResponseID = ""
					wireReq.Input = fullRequestInput
					handler = newResponsesStreamEventHandler(c, responseStateGeneration, debugRaw, "Responses WebSocket", c.websocketServerStateEnabled(), wireReq.SessionID, wireReq.suppressReasoningSummaryDeltas())
					if debugRaw {
						DebugRawSection(debugRaw, "Responses WebSocket Full-State Retry", err.Error())
					}
					retryBody, prepareErr := prepareResponsesWebSocketRequest(wireReq, true, debugRaw)
					if prepareErr != nil {
						c.discardWebSocketLocked()
						return prepareErr
					}
					if err := c.writeResponsesWebSocketRequestLocked(conn, retryBody); err != nil {
						c.discardWebSocketLocked()
						return err
					}
					continue
				}
				c.discardWebSocketLocked()
				return err
			}
			if completed {
				break
			}
		}

		// Stop the response-scoped cancellation watcher before transferring socket
		// ownership to a possible tool continuation. Stream.Close commonly follows
		// EventDone and must not close a socket retained by the parent agentic run.
		stopWatchingContext()

		lastResponseID, _, stateSessionID := c.responseState()
		if stateSessionID == wireReq.SessionID {
			c.wsContinuationBaseline = newResponsesWebSocketContinuationBaseline(wireReq.SessionID, lastResponseID, fullRequestInput, handler.OutputItems())
		} else {
			c.wsContinuationBaseline = nil
		}
		retainedForTool := stateSessionID == wireReq.SessionID && conn.retainForToolContinuation(parentCtx, lastResponseID, handler.FunctionCallIDs())
		if retainedForTool {
			responseActive = false
			lastReq := wireReq
			// Future continuation checks only compare non-input request metadata. Do
			// not rebuild or retain the full transcript during local tool execution.
			lastReq.Input = nil
			lastReq.Messages = nil
			lastReq.PreviousResponseID = ""
			c.wsLastRequest = &lastReq
		} else {
			conn.finishResponse()
			responseActive = false
			lastReq := wireReq
			lastReq.Input = nil
			lastReq.Messages = nil
			lastReq.PreviousResponseID = ""
			c.wsLastRequest = &lastReq
		}

		if err := handler.Finish(send); err != nil {
			if retainedForTool {
				c.discardWebSocketLocked()
			}
			return err
		}
		return nil
	}), nil
}

func responsesWebSocketEventIsMeaningfulProgress(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "", "response.created", "response.queued", "response.in_progress":
		return false
	default:
		return true
	}
}

func addDebugAgeMillis(data map[string]any, key string, now, then time.Time) {
	if data == nil || key == "" || then.IsZero() {
		return
	}
	age := now.Sub(then)
	if age < 0 {
		age = 0
	}
	data[key] = age.Milliseconds()
}

func contextErrorString(ctx context.Context) string {
	if ctx == nil || ctx.Err() == nil {
		return ""
	}
	return ctx.Err().Error()
}

func contextCauseString(ctx context.Context) string {
	if ctx == nil || context.Cause(ctx) == nil {
		return ""
	}
	return context.Cause(ctx).Error()
}

func isPreviousResponseIDRejected(err error) bool {
	if wsErr, ok := err.(*responsesAPIEventError); ok && wsErr.APIError != nil {
		if wsErr.APIError.Code == "previous_response_not_found" {
			return true
		}
		if wsErr.APIError.Param == "previous_response_id" {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "previous_response_id") &&
		(strings.Contains(msg, "invalid") || strings.Contains(msg, "unsupported") || strings.Contains(msg, "not found"))
}

type responsesWebSocketContinuationBaseline struct {
	sessionID      string
	responseID     string
	requestInput   [][sha256.Size]byte
	responseOutput [][sha256.Size]byte
}

func newResponsesWebSocketContinuationBaseline(sessionID, responseID string, requestInput, responseOutput []ResponsesInputItem) *responsesWebSocketContinuationBaseline {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return nil
	}
	requestFingerprints, ok := responsesInputFingerprints(requestInput)
	if !ok {
		return nil
	}
	responseFingerprints, ok := responsesInputFingerprints(responseOutput)
	if !ok {
		return nil
	}
	return &responsesWebSocketContinuationBaseline{
		sessionID:      sessionID,
		responseID:     responseID,
		requestInput:   requestFingerprints,
		responseOutput: responseFingerprints,
	}
}

func responsesInputFingerprints(items []ResponsesInputItem) ([][sha256.Size]byte, bool) {
	fingerprints := make([][sha256.Size]byte, len(items))
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, false
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var normalized any
		if err := decoder.Decode(&normalized); err != nil {
			return nil, false
		}
		canonical, err := json.Marshal(normalized)
		if err != nil {
			return nil, false
		}
		fingerprints[i] = sha256.Sum256(canonical)
	}
	return fingerprints, true
}

func (b *responsesWebSocketContinuationBaseline) incrementalSuffix(sessionID, responseID string, current []ResponsesInputItem) ([]ResponsesInputItem, bool) {
	if b == nil || b.sessionID != sessionID || b.responseID != responseID {
		return nil, false
	}
	currentFingerprints, ok := responsesInputFingerprints(current)
	if !ok {
		return nil, false
	}
	prefixLen := len(b.requestInput) + len(b.responseOutput)
	if len(currentFingerprints) < prefixLen {
		return nil, false
	}
	for i, expected := range b.requestInput {
		if currentFingerprints[i] != expected {
			return nil, false
		}
	}
	for i, expected := range b.responseOutput {
		if currentFingerprints[len(b.requestInput)+i] != expected {
			return nil, false
		}
	}
	return append([]ResponsesInputItem(nil), current[prefixLen:]...), true
}

func (c *ResponsesClient) prepareWebSocketContinuationLocked(req ResponsesRequest, buildContinuationInput func() []ResponsesInputItem, buildFullInput func() []ResponsesInputItem) ResponsesRequest {
	lastResponseID, _, responseStateSessionID := c.responseState()
	if responseStateSessionID != req.SessionID {
		lastResponseID = ""
	}
	fullInput := buildFullInput()
	if !c.websocketServerStateEnabled() || lastResponseID == "" {
		req.PreviousResponseID = ""
		req.Input = fullInput
		return req
	}

	if req.PreviousResponseID == "" {
		req.PreviousResponseID = lastResponseID
	}

	useFullInput := func() ResponsesRequest {
		req.PreviousResponseID = ""
		req.Input = fullInput
		return req
	}

	if c.wsLastRequest == nil {
		if c.WebSocketServerState {
			// Connection-local continuation has no baseline on this socket.
			return useFullInput()
		}
		// Other Responses providers may expose durable previous_response_id state
		// that predates this client connection.
		if continuation := buildContinuationInput(); len(continuation) > 0 {
			req.Input = continuation
			return req
		}
		return useFullInput()
	}

	if !responsesRequestNonInputEqual(*c.wsLastRequest, req) {
		// Tool schemas, model parameters, or other non-input fields changed. Start a
		// fresh chain instead of risking previous_response_id with incompatible state.
		return useFullInput()
	}

	if suffix, ok := c.wsContinuationBaseline.incrementalSuffix(req.SessionID, req.PreviousResponseID, fullInput); ok {
		req.Input = suffix
		return req
	}

	// A durable provider may expose previous_response_id state imported without a
	// local prefix baseline. Preserve the legacy suffix builder for that case only;
	// connection-local ChatGPT state must always prove the complete ordered prefix.
	if c.wsContinuationBaseline == nil && !c.WebSocketServerState {
		if continuation := buildContinuationInput(); len(continuation) > 0 {
			req.Input = continuation
			return req
		}
	}
	return useFullInput()
}

func responsesRequestNonInputEqual(prev, current ResponsesRequest) bool {
	return prev.Model == current.Model &&
		prev.Instructions == current.Instructions &&
		jsonLikeEqualForCompare(prev.Tools, current.Tools) &&
		jsonLikeEqualForCompare(prev.ToolChoice, current.ToolChoice) &&
		reflect.DeepEqual(prev.ParallelToolCalls, current.ParallelToolCalls) &&
		prev.MaxOutputTokens == current.MaxOutputTokens &&
		reflect.DeepEqual(prev.Temperature, current.Temperature) &&
		reflect.DeepEqual(prev.TopP, current.TopP) &&
		reflect.DeepEqual(prev.Reasoning, current.Reasoning) &&
		reflect.DeepEqual(prev.MultiAgent, current.MultiAgent) &&
		reflect.DeepEqual(prev.PromptCacheOptions, current.PromptCacheOptions) &&
		reflect.DeepEqual(prev.Include, current.Include) &&
		reflect.DeepEqual(prev.ExtraHeaders, current.ExtraHeaders) &&
		prev.PromptCacheKey == current.PromptCacheKey &&
		reflect.DeepEqual(prev.Store, current.Store) &&
		reflect.DeepEqual(prev.Generate, current.Generate) &&
		reflect.DeepEqual(prev.StreamOptions, current.StreamOptions)
}

func jsonLikeEqualForCompare(a, b any) bool {
	return jsonLikeValueEqualForCompare(reflect.ValueOf(a), reflect.ValueOf(b))
}

func jsonLikeValueEqualForCompare(a, b reflect.Value) bool {
	originalA := a
	originalB := b
	a = indirectJSONLikeValueForCompare(a)
	b = indirectJSONLikeValueForCompare(b)
	if !a.IsValid() || !b.IsValid() {
		return !a.IsValid() && !b.IsValid()
	}

	if jsonLikeMarshalerForCompare(originalA) || jsonLikeMarshalerForCompare(originalB) || jsonLikeMarshalerForCompare(a) || jsonLikeMarshalerForCompare(b) {
		return jsonLikeMarshaledEqualForCompare(originalA, originalB)
	}

	if av, ok := jsonNumberValueForCompare(a); ok {
		bv, ok := jsonNumberValueForCompare(b)
		return ok && av == bv
	}

	if jsonLikeObjectForCompare(a) && jsonLikeObjectForCompare(b) {
		return jsonLikeObjectsEqualForCompare(a, b)
	}
	if jsonLikeArrayForCompare(a) && jsonLikeArrayForCompare(b) {
		return jsonLikeArraysEqualForCompare(a, b)
	}
	if a.Kind() != b.Kind() {
		return false
	}

	switch a.Kind() {
	case reflect.String:
		return a.String() == b.String()
	case reflect.Bool:
		return a.Bool() == b.Bool()
	default:
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
}

func indirectJSONLikeValueForCompare(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func jsonLikeMarshalerForCompare(v reflect.Value) bool {
	if !v.IsValid() || !v.CanInterface() {
		return false
	}
	_, ok := v.Interface().(json.Marshaler)
	return ok
}

func jsonLikeMarshaledEqualForCompare(a, b reflect.Value) bool {
	if !a.IsValid() || !b.IsValid() || !a.CanInterface() || !b.CanInterface() {
		return false
	}
	aRaw, err := json.Marshal(a.Interface())
	if err != nil {
		return false
	}
	bRaw, err := json.Marshal(b.Interface())
	if err != nil {
		return false
	}
	var aDecoded any
	if err := json.Unmarshal(aRaw, &aDecoded); err != nil {
		return false
	}
	var bDecoded any
	if err := json.Unmarshal(bRaw, &bDecoded); err != nil {
		return false
	}
	return jsonLikeValueEqualForCompare(reflect.ValueOf(aDecoded), reflect.ValueOf(bDecoded))
}

func jsonNumberValueForCompare(v reflect.Value) (float64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return float64(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	default:
		return 0, false
	}
}

func jsonLikeObjectForCompare(v reflect.Value) bool {
	return v.Kind() == reflect.Struct || (v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String)
}

func jsonLikeObjectsEqualForCompare(a, b reflect.Value) bool {
	if a.Kind() == reflect.Map && b.Kind() == reflect.Map {
		if a.Len() != b.Len() {
			return false
		}
		iter := a.MapRange()
		for iter.Next() {
			key := iter.Key()
			other := b.MapIndex(key)
			if !other.IsValid() || !jsonLikeValueEqualForCompare(iter.Value(), other) {
				return false
			}
		}
		return true
	}

	aFields, ok := jsonLikeObjectFieldsForCompare(a)
	if !ok {
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
	bFields, ok := jsonLikeObjectFieldsForCompare(b)
	if !ok {
		return reflect.DeepEqual(a.Interface(), b.Interface())
	}
	if len(aFields) != len(bFields) {
		return false
	}
	for key, value := range aFields {
		other, ok := bFields[key]
		if !ok || !jsonLikeValueEqualForCompare(value, other) {
			return false
		}
	}
	return true
}

func jsonLikeObjectFieldsForCompare(v reflect.Value) (map[string]reflect.Value, bool) {
	switch v.Kind() {
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return nil, false
		}
		fields := make(map[string]reflect.Value, v.Len())
		iter := v.MapRange()
		for iter.Next() {
			fields[iter.Key().String()] = iter.Value()
		}
		return fields, true
	case reflect.Struct:
		typ := v.Type()
		fields := make(map[string]reflect.Value, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name, omitEmpty, ok := jsonFieldNameForCompare(field)
			if !ok {
				continue
			}
			value := v.Field(i)
			if omitEmpty && jsonIsEmptyValueForCompare(value) {
				continue
			}
			fields[name] = value
		}
		return fields, true
	default:
		return nil, false
	}
}

func jsonFieldNameForCompare(field reflect.StructField) (name string, omitEmpty bool, ok bool) {
	if field.PkgPath != "" {
		return "", false, false
	}

	name = field.Name
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, false
	}
	if tag != "" {
		parts := strings.Split(tag, ",")
		if parts[0] != "" {
			name = parts[0]
		}
		for _, option := range parts[1:] {
			if option == "omitempty" {
				omitEmpty = true
			}
		}
	}
	return name, omitEmpty, true
}

func jsonIsEmptyValueForCompare(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	default:
		return false
	}
}

func jsonLikeArrayForCompare(v reflect.Value) bool {
	return v.Kind() == reflect.Array || v.Kind() == reflect.Slice
}

func jsonLikeArraysEqualForCompare(a, b reflect.Value) bool {
	if a.Len() != b.Len() {
		return false
	}
	if jsonLikeStringArrayForCompare(a) && jsonLikeStringArrayForCompare(b) {
		return jsonLikeStringArraysEqualForCompare(a, b)
	}
	for i := 0; i < a.Len(); i++ {
		if !jsonLikeValueEqualForCompare(a.Index(i), b.Index(i)) {
			return false
		}
	}
	return true
}

func jsonLikeStringArrayForCompare(v reflect.Value) bool {
	for i := 0; i < v.Len(); i++ {
		if _, ok := jsonStringValueForCompare(v.Index(i)); !ok {
			return false
		}
	}
	return true
}

func jsonLikeStringArraysEqualForCompare(a, b reflect.Value) bool {
	counts := make(map[string]int, a.Len())
	for i := 0; i < a.Len(); i++ {
		value, _ := jsonStringValueForCompare(a.Index(i))
		counts[value]++
	}
	for i := 0; i < b.Len(); i++ {
		value, ok := jsonStringValueForCompare(b.Index(i))
		if !ok || counts[value] == 0 {
			return false
		}
		counts[value]--
		if counts[value] == 0 {
			delete(counts, value)
		}
	}
	return len(counts) == 0
}

func jsonStringValueForCompare(v reflect.Value) (string, bool) {
	v = indirectJSONLikeValueForCompare(v)
	if !v.IsValid() || v.Kind() != reflect.String {
		return "", false
	}
	return v.String(), true
}

func (c *ResponsesClient) ensureWebSocket(ctx context.Context, req ResponsesRequest) (*responsesWebSocketConnection, bool, error) {
	betaHeader := ""
	if c.ExtraHeaders != nil {
		betaHeader = c.ExtraHeaders["OpenAI-Beta"]
	}
	if req.ExtraHeaders != nil {
		betaHeader = composeBetaHeader(betaHeader, req.ExtraHeaders["OpenAI-Beta"])
	}
	betaHeader = composeBetaHeader(betaHeader, responsesWebSocketBetaHeader)
	if c.wsConn != nil {
		if c.wsConn.healthy() && c.wsConnSessionID == req.SessionID && c.wsConnBetaHeader == betaHeader && c.wsConn.reserveResponse(req.SessionID, req.PreviousResponseID, req.Input) {
			return c.wsConn, true, nil
		}
		// SessionID and feature betas are WebSocket handshake state. A read
		// error also makes the connection ineligible for reuse, even when it
		// arrived while no response stream was consuming application frames.
		c.discardWebSocketLocked()
	}
	wsURL := c.WebSocketURL
	if wsURL == "" {
		var err error
		wsURL, err = responsesWebSocketURL(c.BaseURL)
		if err != nil {
			return nil, false, err
		}
	}

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if c.GetAuthHeader != nil {
		header.Set("Authorization", c.GetAuthHeader())
	}
	if req.SessionID != "" {
		header.Set("session_id", req.SessionID)
	}
	applyResponsesHeaders(header, c.ExtraHeaders, req.ExtraHeaders)
	// WebSocket transport and request-scoped feature betas are additive.
	header.Set("OpenAI-Beta", betaHeader)

	lease, err := sharedResponsesWebSocketPool.acquire(c.websocketAdmissionKey())
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if lease != nil {
			lease.release()
		}
	}()
	adoptConnection := func(conn *websocket.Conn) *responsesWebSocketConnection {
		ws := newResponsesWebSocketConnection(conn, lease, req.SessionID, c.WebSocketIdleTimeout, c.WebSocketFirstEventTimeout, c.WebSocketParkedTimeout)
		lease = nil
		return ws
	}

	connectTimeout := c.WebSocketConnectTimeout
	if connectTimeout == 0 {
		connectTimeout = 30 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  connectTimeout,
		EnableCompression: false,
	}
	dialOnce := func(dialCtx context.Context, h http.Header) (*websocket.Conn, *http.Response, error) {
		return dialer.DialContext(dialCtx, wsURL, h)
	}
	conn, resp, err := dialOnce(dialCtx, header)
	if err != nil {
		if resp != nil {
			defer closeWebSocketHandshakeResponse(resp)
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				if c.OnAuthRetry != nil {
					if retryErr := c.OnAuthRetry(ctx); retryErr != nil {
						return nil, false, retryErr
					}
					retryCtx, retryCancel := context.WithTimeout(ctx, connectTimeout)
					defer retryCancel()
					conn, retryResp, retryErr := dialOnce(retryCtx, headerWithFreshAuth(header, c))
					if retryErr == nil {
						c.wsConn = adoptConnection(conn)
						c.wsConnSessionID = req.SessionID
						c.wsConnBetaHeader = betaHeader
						return c.wsConn, false, nil
					}
					if retryResp != nil {
						defer closeWebSocketHandshakeResponse(retryResp)
						return nil, false, fmt.Errorf("Responses WebSocket handshake failed after re-auth (status %d): %w", retryResp.StatusCode, retryErr)
					}
					return nil, false, fmt.Errorf("connect Responses WebSocket after re-auth: %w", retryErr)
				}
			}
			return nil, false, fmt.Errorf("Responses WebSocket handshake failed (status %d): %w", resp.StatusCode, err)
		}
		if strings.Contains(err.Error(), "426") {
			return nil, false, fmt.Errorf("Responses WebSocket upgrade required: %w", err)
		}
		return nil, false, fmt.Errorf("connect Responses WebSocket: %w", err)
	}
	c.wsConn = adoptConnection(conn)
	c.wsConnSessionID = req.SessionID
	c.wsConnBetaHeader = betaHeader
	return c.wsConn, false, nil
}

func closeWebSocketHandshakeResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

func headerWithFreshAuth(header http.Header, c *ResponsesClient) http.Header {
	fresh := header.Clone()
	if c.GetAuthHeader != nil {
		fresh.Set("Authorization", c.GetAuthHeader())
	}
	return fresh
}

func (c *ResponsesClient) closeWebSocket() {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	c.discardWebSocketLocked()
}

func (c *ResponsesClient) discardWebSocketLocked() {
	c.closeWebSocketConnectionLocked(true)
}

func (c *ResponsesClient) closeWebSocketConnectionLocked(clearLastRequest bool) {
	if c.wsConn == nil {
		if clearLastRequest {
			c.wsLastRequest = nil
			c.wsContinuationBaseline = nil
		}
		return
	}
	closeTimeout := 5 * time.Second
	_ = c.wsConn.conn.SetWriteDeadline(time.Now().Add(closeTimeout))
	_ = c.wsConn.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = c.wsConn.close()
	c.wsConn = nil
	c.wsConnSessionID = ""
	c.wsConnBetaHeader = ""
	if clearLastRequest {
		c.wsLastRequest = nil
		c.wsContinuationBaseline = nil
	}
}
