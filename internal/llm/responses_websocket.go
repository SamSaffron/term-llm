package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const responsesWebSocketBetaHeader = "responses_websockets=2026-02-06"

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

	Model              string               `json:"model"`
	Instructions       string               `json:"instructions,omitempty"`
	Input              []ResponsesInputItem `json:"input"`
	Tools              []any                `json:"tools,omitempty"`
	ToolChoice         any                  `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                `json:"parallel_tool_calls,omitempty"`
	MaxOutputTokens    int                  `json:"max_output_tokens,omitempty"`
	Temperature        *float64             `json:"temperature,omitempty"`
	TopP               *float64             `json:"top_p,omitempty"`
	Reasoning          *ResponsesReasoning  `json:"reasoning,omitempty"`
	Include            []string             `json:"include,omitempty"`
	PromptCacheKey     string               `json:"prompt_cache_key,omitempty"`
	Store              *bool                `json:"store,omitempty"`
	Generate           *bool                `json:"generate,omitempty"`
	PreviousResponseID string               `json:"previous_response_id,omitempty"`
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
		Include:            req.Include,
		PromptCacheKey:     req.PromptCacheKey,
		Store:              req.Store,
		Generate:           req.Generate,
		PreviousResponseID: req.PreviousResponseID,
	}
}

func (c *ResponsesClient) writeResponsesWebSocketRequestLocked(conn *websocket.Conn, req ResponsesRequest, reused bool, debugRaw bool) error {
	wsReq := newResponsesWSRequest(req)
	body, err := json.Marshal(wsReq)
	if err != nil {
		return fmt.Errorf("failed to marshal Responses WebSocket request: %w", err)
	}
	if debugRaw {
		var prettyBody bytes.Buffer
		json.Indent(&prettyBody, body, "", "  ")
		DebugRawSection(debugRaw, fmt.Sprintf("Responses WebSocket Request (reused=%t)", reused), prettyBody.String())
	}
	writeTimeout := c.WebSocketWriteTimeout
	if writeTimeout == 0 {
		writeTimeout = 30 * time.Second
	}
	if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set Responses WebSocket write deadline: %w", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, body); err != nil {
		return fmt.Errorf("write Responses WebSocket request: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	return nil
}

func (c *ResponsesClient) streamWebSocketPrepared(ctx context.Context, req ResponsesRequest, buildFullInput func() []ResponsesInputItem, debugRaw bool, responseStateGeneration uint64) (Stream, error) {
	c.wsMu.Lock()
	wireReq := c.prepareWebSocketContinuationLocked(req, buildFullInput)

	conn, reused, err := c.ensureWebSocket(ctx, wireReq)
	if err != nil {
		c.wsMu.Unlock()
		return nil, err
	}

	if err := c.writeResponsesWebSocketRequestLocked(conn, wireReq, reused, debugRaw); err != nil {
		c.discardWebSocketLocked()
		c.wsMu.Unlock()
		return nil, err
	}

	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		defer c.wsMu.Unlock()

		ctxDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-ctxDone:
			}
		}()
		defer close(ctxDone)

		handler := newResponsesStreamEventHandler(c, responseStateGeneration, debugRaw, "Responses WebSocket", c.websocketServerStateEnabled())
		retriedFullState := false
		idleTimeout := c.WebSocketIdleTimeout
		if idleTimeout == 0 {
			idleTimeout = 5 * time.Minute
		}
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(idleTimeout))
		})

		for {
			_ = conn.SetReadDeadline(time.Now().Add(idleTimeout))
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				c.discardWebSocketLocked()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("read Responses WebSocket event: %w", err)
			}
			if messageType != websocket.TextMessage {
				c.discardWebSocketLocked()
				return fmt.Errorf("Responses WebSocket returned unsupported frame type %d", messageType)
			}

			eventType, err := responsesJSONEventType(data)
			if err != nil {
				c.discardWebSocketLocked()
				return fmt.Errorf("decode Responses WebSocket event envelope: %w", err)
			}
			completed, err := handler.HandleJSONEvent(data, eventType, send)
			if err != nil {
				if wsErr, ok := err.(*responsesAPIEventError); ok && wsErr.APIError != nil {
					switch wsErr.APIError.Code {
					case "previous_response_not_found":
						c.clearLastResponseIDIfGeneration(responseStateGeneration)
					case "websocket_connection_limit_reached":
						// The documented 60-minute connection limit is recovered by
						// dropping the socket; the next Stream call reconnects lazily.
					}
				}
				if !retriedFullState && !handler.Emitted() && wireReq.PreviousResponseID != "" && isPreviousResponseIDRejected(err) {
					retriedFullState = true
					c.clearLastResponseIDIfGeneration(responseStateGeneration)
					c.wsLastRequest = nil
					c.wsLastResponseItems = nil
					wireReq.PreviousResponseID = ""
					wireReq.Input = buildFullInput()
					handler = newResponsesStreamEventHandler(c, responseStateGeneration, debugRaw, "Responses WebSocket", c.websocketServerStateEnabled())
					if debugRaw {
						DebugRawSection(debugRaw, "Responses WebSocket Full-State Retry", err.Error())
					}
					if err := c.writeResponsesWebSocketRequestLocked(conn, wireReq, true, debugRaw); err != nil {
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

		if err := handler.Finish(send); err != nil {
			return err
		}
		fullReq := wireReq
		fullReq.Input = append([]ResponsesInputItem(nil), buildFullInput()...)
		fullReq.PreviousResponseID = ""
		c.wsLastRequest = &fullReq
		c.wsLastResponseItems = handler.OutputItems()
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
	return strings.Contains(msg, "previous_response_id") && (strings.Contains(msg, "unsupported") || strings.Contains(msg, "not found"))
}

func (c *ResponsesClient) prepareWebSocketContinuationLocked(req ResponsesRequest, buildFullInput func() []ResponsesInputItem) ResponsesRequest {
	if !c.websocketServerStateEnabled() || c.LastResponseID == "" {
		req.PreviousResponseID = ""
		req.Input = buildFullInput()
		return req
	}

	if req.PreviousResponseID == "" {
		req.PreviousResponseID = c.LastResponseID
	}

	if c.wsLastRequest == nil {
		req.PreviousResponseID = ""
		req.Input = buildFullInput()
		return req
	}

	prevComparable := responsesRequestNonInputComparable(*c.wsLastRequest)
	currentComparable := responsesRequestNonInputComparable(req)
	if !reflect.DeepEqual(prevComparable, currentComparable) {
		// Tool schemas, model parameters, or other non-input fields changed. Start a
		// fresh chain instead of risking previous_response_id with incompatible state.
		req.PreviousResponseID = ""
		req.Input = buildFullInput()
		return req
	}

	if req.Input != nil {
		return req
	}

	fullInput := buildFullInput()

	baseline := append([]ResponsesInputItem{}, c.wsLastRequest.Input...)
	baseline = append(baseline, c.wsLastResponseItems...)
	if responsesInputHasPrefix(fullInput, baseline) {
		req.PreviousResponseID = c.LastResponseID
		req.Input = append([]ResponsesInputItem(nil), fullInput[len(baseline):]...)
		return req
	}

	// Fall back to the historical suffix filter for providers that do not echo
	// response output in exactly the same shape as our local transcript, but only
	// when the suffix is non-empty and obviously a new user/tool continuation.
	filtered := filterToNewInput(fullInput)
	if len(filtered) > 0 && len(filtered) < len(fullInput) {
		req.PreviousResponseID = c.LastResponseID
		req.Input = filtered
		return req
	}

	req.PreviousResponseID = ""
	req.Input = fullInput
	return req
}

func responsesRequestNonInputComparable(req ResponsesRequest) any {
	return struct {
		Model             string
		Instructions      string
		Tools             any
		ToolChoice        any
		ParallelToolCalls *bool
		MaxOutputTokens   int
		Temperature       *float64
		TopP              *float64
		Reasoning         *ResponsesReasoning
		Include           []string
		PromptCacheKey    string
		Store             *bool
		Generate          *bool
	}{
		Model:             req.Model,
		Instructions:      req.Instructions,
		Tools:             normalizeJSONLikeForCompare(req.Tools),
		ToolChoice:        normalizeJSONLikeForCompare(req.ToolChoice),
		ParallelToolCalls: req.ParallelToolCalls,
		MaxOutputTokens:   req.MaxOutputTokens,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		Reasoning:         req.Reasoning,
		Include:           req.Include,
		PromptCacheKey:    req.PromptCacheKey,
		Store:             req.Store,
		Generate:          req.Generate,
	}
}

func normalizeJSONLikeForCompare(v any) any {
	if v == nil {
		return nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return v
	}
	return normalizeDecodedJSONForCompare(decoded)
}

func normalizeDecodedJSONForCompare(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for key, value := range x {
			x[key] = normalizeDecodedJSONForCompare(value)
		}
		return x
	case []any:
		for i, value := range x {
			x[i] = normalizeDecodedJSONForCompare(value)
		}
		if allStrings(x) {
			sort.Slice(x, func(i, j int) bool { return x[i].(string) < x[j].(string) })
		}
		return x
	default:
		return v
	}
}

func allStrings(values []any) bool {
	for _, value := range values {
		if _, ok := value.(string); !ok {
			return false
		}
	}
	return true
}

func responsesInputHasPrefix(input, prefix []ResponsesInputItem) bool {
	if len(prefix) > len(input) {
		return false
	}
	for i := range prefix {
		if !reflect.DeepEqual(input[i], prefix[i]) {
			return false
		}
	}
	return true
}

func (c *ResponsesClient) ensureWebSocket(ctx context.Context, req ResponsesRequest) (*websocket.Conn, bool, error) {
	if c.wsConn != nil {
		return c.wsConn, true, nil
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
	for key, value := range c.ExtraHeaders {
		header.Set(key, value)
	}
	// The Responses WebSocket beta header replaces provider HTTP beta values
	// (notably ChatGPT's "responses=experimental") for the WS handshake.
	header.Set("OpenAI-Beta", responsesWebSocketBetaHeader)

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
						c.wsConn = conn
						return conn, false, nil
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
	c.wsConn = conn
	return conn, false, nil
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
	_ = c.wsConn.SetWriteDeadline(time.Now().Add(closeTimeout))
	_ = c.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = c.wsConn.Close()
	c.wsConn = nil
	c.wsLastRequest = nil
	c.wsLastResponseItems = nil
}
