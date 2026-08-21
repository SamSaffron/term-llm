package llm

import (
	"bytes"
	"context"
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

const responsesWebSocketBetaHeader = "responses_websockets=2026-02-06"

type responsesWebSocketFrameTypeError struct {
	messageType int
}

func (e *responsesWebSocketFrameTypeError) Error() string {
	return fmt.Sprintf("Responses WebSocket returned unsupported frame type %d", e.messageType)
}

type responsesWebSocketConnection struct {
	conn *websocket.Conn

	readMu         sync.Mutex
	readQueue      [][]byte
	readErr        error
	readReady      chan struct{}
	responseActive bool
	idleTimeout    time.Duration
	idleTimer      *time.Timer
	idleGeneration uint64
}

func newResponsesWebSocketConnection(conn *websocket.Conn, idleTimeout time.Duration) *responsesWebSocketConnection {
	if idleTimeout == 0 {
		idleTimeout = 5 * time.Minute
	}
	ws := &responsesWebSocketConnection{
		conn:        conn,
		readReady:   make(chan struct{}, 1),
		idleTimeout: idleTimeout,
	}

	pingHandler := conn.PingHandler()
	conn.SetPingHandler(func(appData string) error {
		ws.noteReadActivity()
		return pingHandler(appData)
	})
	conn.SetPongHandler(func(string) error {
		ws.noteReadActivity()
		return nil
	})

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
		c.armIdleTimerLocked()
		c.readQueue = append(c.readQueue, data)
		c.readMu.Unlock()
		c.signalReadReady()
	}
}

func (c *responsesWebSocketConnection) startResponse() error {
	c.readMu.Lock()
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
	c.armIdleTimerLocked()
	c.readMu.Unlock()
	return nil
}

func (c *responsesWebSocketConnection) finishResponse() {
	c.readMu.Lock()
	if !c.responseActive {
		c.readMu.Unlock()
		return
	}
	c.responseActive = false
	c.disarmIdleTimerLocked()
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

func (c *responsesWebSocketConnection) noteReadActivity() {
	c.readMu.Lock()
	if c.responseActive && c.readErr == nil {
		c.armIdleTimerLocked()
	}
	c.readMu.Unlock()
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

func (c *responsesWebSocketConnection) failReadingLocked(err error) {
	if c.readErr == nil {
		c.readErr = err
	}
	c.responseActive = false
	c.disarmIdleTimerLocked()
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
	c.readMu.Lock()
	c.failReadingLocked(errors.New("Responses WebSocket connection closed"))
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
	c.wsMu.Lock()

	conn, reused, err := c.ensureWebSocket(ctx, req)
	if err != nil {
		c.wsMu.Unlock()
		return nil, err
	}

	wireReq := c.prepareWebSocketContinuationLocked(req, buildContinuationInput, buildFullInput)
	if c.WebSocketServerState && !reused {
		// ChatGPT's previous_response_id chain is local to a WebSocket. A fresh
		// connection cannot continue state created by the socket it replaced.
		c.clearLastResponseIDIfGeneration(responseStateGeneration, wireReq.SessionID, wireReq.PreviousResponseID)
		c.wsLastRequest = nil
		wireReq.PreviousResponseID = ""
		wireReq.Input = buildFullInput()
	}

	body, err := prepareResponsesWebSocketRequest(wireReq, reused, debugRaw)
	if err != nil {
		c.wsMu.Unlock()
		return nil, err
	}

	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		defer c.wsMu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := conn.startResponse(); err != nil {
			c.discardWebSocketLocked()
			return err
		}
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

		for {
			data, err := conn.nextMessage(ctx)
			if err != nil {
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
					wireReq.PreviousResponseID = ""
					wireReq.Input = buildFullInput()
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

		// Park the pump before publishing EventDone. Any already-queued trailing
		// application frame invalidates the connection, and subsequent pings/closes
		// remain serviced without an inference idle timer.
		conn.finishResponse()
		responseActive = false

		// Stop the cancellation watcher before emitting the terminal EventDone.
		// Consumers commonly call Close immediately after receiving EventDone; if the
		// watcher is still active, that Close can race with this goroutine returning
		// and close an otherwise healthy WebSocket that should be reused for the next
		// turn.
		stopWatchingContext()

		if err := handler.Finish(send); err != nil {
			return err
		}
		lastReq := wireReq
		// Future continuation checks only compare non-input request metadata. Do not
		// rebuild or retain the full transcript after every completed WebSocket turn.
		lastReq.Input = nil
		lastReq.Messages = nil
		lastReq.PreviousResponseID = ""
		c.wsLastRequest = &lastReq
		return nil
	}), nil
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

func (c *ResponsesClient) prepareWebSocketContinuationLocked(req ResponsesRequest, buildContinuationInput func() []ResponsesInputItem, buildFullInput func() []ResponsesInputItem) ResponsesRequest {
	lastResponseID, _, responseStateSessionID := c.responseState()
	if responseStateSessionID != req.SessionID {
		lastResponseID = ""
	}
	if !c.websocketServerStateEnabled() || lastResponseID == "" {
		req.PreviousResponseID = ""
		req.Input = buildFullInput()
		return req
	}

	if req.PreviousResponseID == "" {
		req.PreviousResponseID = lastResponseID
	}

	useFullInput := func() ResponsesRequest {
		req.PreviousResponseID = ""
		req.Input = buildFullInput()
		return req
	}

	if c.wsLastRequest == nil {
		if c.WebSocketServerState {
			// Connection-local continuation has no baseline on this socket.
			return useFullInput()
		}
		// Other Responses providers may expose durable previous_response_id state
		// that predates this client connection.
		if req.Input != nil {
			return req
		}
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

	if req.Input != nil {
		return req
	}

	// The caller already knows how to build an incremental continuation; prefer
	// that over rebuilding and rescanning the full transcript on every follow-up.
	if continuation := buildContinuationInput(); len(continuation) > 0 {
		req.Input = continuation
		return req
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
		if c.wsConn.healthy() && c.wsConnSessionID == req.SessionID && c.wsConnBetaHeader == betaHeader {
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
						c.wsConn = newResponsesWebSocketConnection(conn, c.WebSocketIdleTimeout)
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
	c.wsConn = newResponsesWebSocketConnection(conn, c.WebSocketIdleTimeout)
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
	if c.wsConn == nil {
		return
	}
	closeTimeout := 5 * time.Second
	_ = c.wsConn.conn.SetWriteDeadline(time.Now().Add(closeTimeout))
	_ = c.wsConn.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = c.wsConn.close()
	c.wsConn = nil
	c.wsConnSessionID = ""
	c.wsConnBetaHeader = ""
	c.wsLastRequest = nil
}
