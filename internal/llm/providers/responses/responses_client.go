package responses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func applyResponsesHeaders(header http.Header, base, request map[string]string) {
	for key, value := range base {
		header.Set(key, value)
	}
	for key, value := range request {
		if strings.EqualFold(key, "OpenAI-Beta") {
			header.Set(key, composeBetaHeader(header.Get(key), value))
		} else {
			header.Set(key, value)
		}
	}
}

func readResponsesAPIErrorBody(resp *http.Response) []byte {
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponsesAPIErrorBodyBytes+1))
	if len(body) <= maxResponsesAPIErrorBodyBytes {
		return body
	}

	truncated := make([]byte, 0, maxResponsesAPIErrorBodyBytes+len(truncatedResponsesAPIErrorBodySuffix))
	truncated = append(truncated, body[:maxResponsesAPIErrorBodyBytes]...)
	truncated = append(truncated, truncatedResponsesAPIErrorBodySuffix...)
	return truncated
}

func (c *ResponsesClient) responseState() (lastResponseID string, generation uint64, sessionID string) {
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	return c.LastResponseID, c.responseStateGeneration, c.responseStateSessionID
}

func (c *ResponsesClient) clearLastResponseIDIfGeneration(generation uint64, sessionID, responseID string) {
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	if c.responseStateGeneration != generation {
		return
	}
	if c.responseStateSessionID != sessionID {
		return
	}
	if responseID != "" && c.LastResponseID != responseID {
		return
	}
	c.LastResponseID = ""
	c.responseStateSessionID = ""
}

func (c *ResponsesClient) setLastResponseIDIfGeneration(generation uint64, responseID, sessionID string) {
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	if c.responseStateGeneration != generation {
		return
	}
	c.LastResponseID = responseID
	c.responseStateSessionID = sessionID
}

func cloneResponsesClientFreshConversation(c *ResponsesClient) *ResponsesClient {
	return cloneResponsesClient(c)
}

// cloneResponsesClientForkConversation creates an independent client whose
// first request branches from the live client's current server-side response.
// The clone may advance its own response chain without mutating the live one.
func cloneResponsesClientForkConversation(c *ResponsesClient) (*ResponsesClient, bool) {
	if c == nil || c.DisableServerState {
		return nil, false
	}
	lastResponseID, generation, _ := c.responseState()
	if lastResponseID == "" {
		return nil, false
	}
	clone := cloneResponsesClient(c)
	clone.LastResponseID = lastResponseID
	clone.responseStateGeneration = generation
	// Helper requests do not carry the main runtime's session ID. Pin the fork
	// to their empty session key so ResponsesClient uses the copied parent.
	clone.responseStateSessionID = ""
	return clone, true
}

func cloneResponsesClient(c *ResponsesClient) *ResponsesClient {
	if c == nil {
		return nil
	}

	var extraHeaders map[string]string
	if c.ExtraHeaders != nil {
		extraHeaders = make(map[string]string, len(c.ExtraHeaders))
		for key, value := range c.ExtraHeaders {
			extraHeaders[key] = value
		}
	}

	return &ResponsesClient{
		BaseURL:                    c.BaseURL,
		GetAuthHeader:              c.GetAuthHeader,
		ExtraHeaders:               extraHeaders,
		HTTPClient:                 c.HTTPClient,
		DisableServerState:         c.DisableServerState,
		UseWebSocket:               c.UseWebSocket,
		WebSocketServerState:       c.WebSocketServerState,
		WebSocketURL:               c.WebSocketURL,
		WebSocketPoolKey:           c.WebSocketPoolKey,
		WebSocketConnectTimeout:    c.WebSocketConnectTimeout,
		WebSocketWriteTimeout:      c.WebSocketWriteTimeout,
		WebSocketIdleTimeout:       c.WebSocketIdleTimeout,
		WebSocketFirstEventTimeout: c.WebSocketFirstEventTimeout,
		WebSocketParkedTimeout:     c.WebSocketParkedTimeout,
		HandleError:                c.HandleError,
		OnAuthRetry:                c.OnAuthRetry,
	}
}

// Stream makes a streaming request to the Responses API and returns events via a Stream
func (c *ResponsesClient) Stream(ctx context.Context, req ResponsesRequest, debugRaw bool) (Stream, error) {
	fullInput := req.Input
	fullInputBuilt := req.Input != nil
	buildFullInput := func() []ResponsesInputItem {
		if fullInputBuilt {
			return fullInput
		}
		if req.ExtractInstructionsFromMessages {
			_, fullInput = BuildResponsesInputWithInstructionsAndFilePolicy(req.Messages, req.FileUploadPolicy)
		} else {
			fullInput = BuildResponsesInputWithFilePolicy(req.Messages, req.FileUploadPolicy)
		}
		fullInputBuilt = true
		return fullInput
	}

	continuationInput := []ResponsesInputItem(nil)
	continuationBuilt := false
	buildContinuationInput := func() []ResponsesInputItem {
		if continuationBuilt {
			return continuationInput
		}
		if len(req.Messages) > 0 {
			continuationInput = buildResponsesContinuationInputWithFilePolicy(req.Messages, req.FileUploadPolicy, req.IncludeDeveloperInContinuation)
			continuationInput = trimResponsesContinuationToDiscoveryOutput(continuationInput)
		} else {
			continuationInput = filterToNewInput(buildFullInput())
		}
		continuationBuilt = true
		return continuationInput
	}

	lastResponseID, responseStateGeneration, responseStateSessionID := c.responseState()
	if lastResponseID != "" && responseStateSessionID != req.SessionID {
		lastResponseID = ""
	}

	wsReq := req
	httpPayload := req
	if lastResponseID != "" {
		if c.websocketServerStateEnabled() {
			wsReq.PreviousResponseID = lastResponseID
			// WebSocket continuation derives its suffix from the full ordered input
			// under wsMu; never trust a pre-trimmed caller suffix for local state.
			wsReq.Input = nil
		} else {
			wsReq.Input = buildFullInput()
		}
		if !c.DisableServerState {
			httpPayload.PreviousResponseID = lastResponseID
			httpPayload.Input = buildContinuationInput()
		} else {
			httpPayload.Input = buildFullInput()
		}
	} else {
		wsReq.PreviousResponseID = ""
		wsReq.Input = nil
		httpPayload.PreviousResponseID = ""
		httpPayload.Input = buildFullInput()
	}

	if c.UseWebSocket && !c.websocketDisabled && !req.ForceHTTP {
		buildWebSocketFallbacks := func(nextAttempt int) []responsesWebSocketFallback {
			fallbacks := make([]responsesWebSocketFallback, 0, responsesWebSocketMaxAttempts-nextAttempt+2)
			for attempt := nextAttempt; attempt <= responsesWebSocketMaxAttempts; attempt++ {
				attempt := attempt
				wait := responsesWebSocketBackoff(attempt - 1)
				fallbacks = append(fallbacks, responsesWebSocketFallback{
					retry: &Event{
						Type:             EventRetry,
						RetryAttempt:     attempt,
						RetryMaxAttempts: responsesWebSocketMaxAttempts,
						RetryWaitSecs:    wait.Seconds(),
					},
					open: func() (Stream, error) {
						c.closeWebSocket()
						if debugRaw {
							debugRawSection(debugRaw, "Responses WebSocket Retry", fmt.Sprintf("stream failed before emitting events; retrying WebSocket attempt %d/%d after %s", attempt, responsesWebSocketMaxAttempts, wait))
						}
						if err := sleepWithContext(ctx, wait); err != nil {
							return nil, err
						}
						return c.streamWebSocketPrepared(ctx, wsReq, buildContinuationInput, buildFullInput, debugRaw, responseStateGeneration)
					},
				})
			}
			if !req.ForceWebSocket {
				fallbacks = append(fallbacks, responsesWebSocketFallback{
					disableWebSocket: func() { c.websocketDisabled = true },
					open: func() (Stream, error) {
						c.closeWebSocket()
						if debugRaw {
							debugRawSection(debugRaw, "Responses WebSocket Fallback", "stream failed before emitting events")
						}
						return c.streamHTTPPrepared(ctx, httpPayload, buildFullInput, responseStateGeneration, debugRaw)
					},
				})
			}
			return fallbacks
		}

		stream, err := c.streamWebSocketPrepared(ctx, wsReq, buildContinuationInput, buildFullInput, debugRaw, responseStateGeneration)
		if err == nil {
			return &responsesWebSocketFallbackStream{
				current:   stream,
				fallbacks: buildWebSocketFallbacks(2),
			}, nil
		}
		if errors.Is(err, errResponsesWebSocketPoolSaturated) {
			if req.ForceWebSocket {
				return nil, fmt.Errorf("required Responses WebSocket unavailable: %w", err)
			}
			c.closeWebSocket()
			return c.streamHTTPPrepared(ctx, httpPayload, buildFullInput, responseStateGeneration, debugRaw)
		}
		lastErr := err
		for attempt := 2; attempt <= responsesWebSocketMaxAttempts; attempt++ {
			wait := responsesWebSocketBackoff(attempt - 1)
			c.closeWebSocket()
			if debugRaw {
				debugRawSection(debugRaw, "Responses WebSocket Retry", fmt.Sprintf("initial stream setup failed: %v; retrying WebSocket attempt %d/%d after %s", lastErr, attempt, responsesWebSocketMaxAttempts, wait))
			}
			if err := sleepWithContext(ctx, wait); err != nil {
				return nil, err
			}
			stream, err = c.streamWebSocketPrepared(ctx, wsReq, buildContinuationInput, buildFullInput, debugRaw, responseStateGeneration)
			if err == nil {
				return &responsesWebSocketFallbackStream{
					current:   stream,
					fallbacks: buildWebSocketFallbacks(attempt + 1),
				}, nil
			}
			if errors.Is(err, errResponsesWebSocketPoolSaturated) {
				if req.ForceWebSocket {
					return nil, fmt.Errorf("required Responses WebSocket unavailable: %w", err)
				}
				c.closeWebSocket()
				return c.streamHTTPPrepared(ctx, httpPayload, buildFullInput, responseStateGeneration, debugRaw)
			}
			lastErr = err
		}
		c.websocketDisabled = true
		c.closeWebSocket()
		if debugRaw {
			debugRawSection(debugRaw, "Responses WebSocket Fallback", lastErr.Error())
		}
		if req.ForceWebSocket {
			return nil, fmt.Errorf("required Responses WebSocket failed: %w", lastErr)
		}
	}
	if req.ForceWebSocket {
		return nil, fmt.Errorf("required Responses WebSocket transport is unavailable")
	}

	return c.streamHTTPPrepared(ctx, httpPayload, buildFullInput, responseStateGeneration, debugRaw)
}

func (c *ResponsesClient) streamHTTPPrepared(ctx context.Context, httpPayload ResponsesRequest, buildFullInput func() []ResponsesInputItem, responseStateGeneration uint64, debugRaw bool) (Stream, error) {
	body, err := json.Marshal(httpPayload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	if debugRaw {
		var prettyBody bytes.Buffer
		json.Indent(&prettyBody, body, "", "  ")
		debugRawSection(debugRaw, "Responses API Request", prettyBody.String())
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.GetAuthHeader != nil {
		httpReq.Header.Set("Authorization", c.GetAuthHeader())
	}
	if httpPayload.SessionID != "" {
		httpReq.Header.Set("session_id", httpPayload.SessionID)
	}
	applyResponsesHeaders(httpReq.Header, c.ExtraHeaders, httpPayload.ExtraHeaders)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = defaultHTTPClient()
	}

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Responses API request failed: %w", err)
	}

	// Check for error responses synchronously so retry logic can handle them
	if resp.StatusCode != http.StatusOK {
		respBody := readResponsesAPIErrorBody(resp)

		debugRawHTTPErrorResponse(debugRaw, "Responses API Error Response", resp, respBody)

		// Provider-specific error handling (e.g., ChatGPT rate limits)
		if c.HandleError != nil {
			if err := c.HandleError(resp.StatusCode, respBody, resp.Header); err != nil {
				return nil, err
			}
		}

		// Check for previous_response_id not found error.
		if resp.StatusCode == http.StatusNotFound && httpPayload.PreviousResponseID != "" {
			// Clear state and retry with full history
			c.clearLastResponseIDIfGeneration(responseStateGeneration, httpPayload.SessionID, httpPayload.PreviousResponseID)
			retryPayload := httpPayload
			retryPayload.PreviousResponseID = ""
			retryPayload.Input = buildFullInput()
			// Re-marshal without previous_response_id
			body, err = json.Marshal(retryPayload)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal retry request: %w", err)
			}
			httpReq, err = http.NewRequestWithContext(ctx, "POST", c.BaseURL, bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("failed to create retry request: %w", err)
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpReq.Header.Set("Accept", "text/event-stream")
			if c.GetAuthHeader != nil {
				httpReq.Header.Set("Authorization", c.GetAuthHeader())
			}
			if retryPayload.SessionID != "" {
				httpReq.Header.Set("session_id", retryPayload.SessionID)
			}
			applyResponsesHeaders(httpReq.Header, c.ExtraHeaders, retryPayload.ExtraHeaders)
			resp, err = httpClient.Do(httpReq)
			if err != nil {
				return nil, fmt.Errorf("Responses API retry request failed: %w", err)
			}
			if resp.StatusCode != http.StatusOK {
				retryBody := readResponsesAPIErrorBody(resp)
				return nil, newHTTPStatusErrorMessagef(resp, retryBody, "Responses API error (status %d): %s", resp.StatusCode, responsesAPIErrorDetail(retryBody))
			}
		} else if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if c.OnAuthRetry != nil {
				if err := c.OnAuthRetry(ctx); err != nil {
					return nil, err
				}
				// Retry with refreshed credentials
				httpReq, err = http.NewRequestWithContext(ctx, "POST", c.BaseURL, bytes.NewReader(body))
				if err != nil {
					return nil, fmt.Errorf("failed to create auth retry request: %w", err)
				}
				httpReq.Header.Set("Content-Type", "application/json")
				httpReq.Header.Set("Accept", "text/event-stream")
				if c.GetAuthHeader != nil {
					httpReq.Header.Set("Authorization", c.GetAuthHeader())
				}
				if httpPayload.SessionID != "" {
					httpReq.Header.Set("session_id", httpPayload.SessionID)
				}
				applyResponsesHeaders(httpReq.Header, c.ExtraHeaders, httpPayload.ExtraHeaders)
				resp, err = httpClient.Do(httpReq)
				if err != nil {
					return nil, fmt.Errorf("Responses API auth retry request failed: %w", err)
				}
				if resp.StatusCode != http.StatusOK {
					retryBody := readResponsesAPIErrorBody(resp)
					return nil, newHTTPStatusErrorMessagef(resp, retryBody, "Responses API error after re-auth (status %d): %s", resp.StatusCode, responsesAPIErrorDetail(retryBody))
				}
			} else {
				if apiMessage := responsesAPIErrorMessage(respBody); apiMessage != "" {
					return nil, newHTTPStatusErrorMessagef(resp, respBody, "Responses API error (status %d): %s", resp.StatusCode, apiMessage)
				}
				return nil, newHTTPStatusErrorMessagef(resp, respBody, "Responses API authentication failed (status %d): token may be invalid or expired", resp.StatusCode)
			}
		} else {
			return nil, newHTTPStatusErrorMessagef(resp, respBody, "Responses API error (status %d): %s", resp.StatusCode, responsesAPIErrorDetail(respBody))
		}
	}

	// Capture client reference for the goroutine
	client := c

	// Create async stream for successful response
	return newEventStreamWithCancelHook(ctx, func() { _ = resp.Body.Close() }, func(ctx context.Context, send eventSender) error {
		defer resp.Body.Close()

		reader := bufio.NewReader(resp.Body)

		var lastEventType string
		var eventData []byte
		handler := newResponsesStreamEventHandler(client, responseStateGeneration, debugRaw, "Responses API SSE", !client.DisableServerState, httpPayload.SessionID, httpPayload.suppressReasoningSummaryDeltas())
		sawTerminal := false

		flushEvent := func() (bool, error) {
			if len(eventData) == 0 {
				lastEventType = ""
				return false, nil
			}

			data := eventData
			eventType := lastEventType
			lastEventType = ""

			stop, err := handler.HandleJSONEvent(data, eventType, send)
			if err != nil && (eventType == "response.completed" || eventType == "response.incomplete") && !json.Valid(data) {
				// A truncated terminal event is a transport failure, not a provider
				// rejection. Classifying it as incomplete lets the engine preserve
				// committed tool progress and resume instead of surfacing a misleading
				// invalid_request_error to the user.
				if finishErr := handler.FinishIncomplete(send); finishErr != nil {
					err = errors.Join(err, finishErr)
				}
				return false, &StreamIncompleteError{
					Transport: "Responses API SSE",
					Terminal:  eventType,
					Err:       err,
				}
			}
			if stop {
				sawTerminal = true
			}
			eventData = data[:0]
			return stop, err
		}

		for {
			line, eof, err := readSSELineBytes(reader)
			if err != nil {
				return fmt.Errorf("Responses API streaming error: %w", err)
			}

			if len(line) == 0 {
				stop, err := flushEvent()
				if err != nil {
					return err
				}
				if stop || eof {
					break
				}
				continue
			}

			if i := bytes.IndexByte(line, ':'); i >= 0 {
				field, value := line[:i], line[i+1:]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				if bytes.Equal(field, sseEventField) && len(eventData) > 0 {
					stop, err := flushEvent()
					if err != nil {
						return err
					}
					if stop {
						break
					}
				}
				if bytes.Equal(field, sseDataField) && bytes.Equal(value, sseDoneData) && len(eventData) > 0 {
					stop, err := flushEvent()
					if err != nil {
						return err
					}
					if stop {
						break
					}
				}
				switch {
				case bytes.Equal(field, sseEventField):
					lastEventType = string(value)
				case bytes.Equal(field, sseDataField):
					if len(eventData) > 0 {
						eventData = append(eventData, '\n')
					}
					eventData = append(eventData, value...)
				}
				if bytes.Equal(field, sseDataField) && bytes.Equal(value, sseDoneData) {
					stop, err := flushEvent()
					if err != nil {
						return err
					}
					if stop || eof {
						break
					}
					continue
				}
			}

			if eof {
				stop, err := flushEvent()
				if err != nil {
					return err
				}
				if stop {
					break
				}
				break
			}
		}

		if !sawTerminal {
			if err := handler.FinishIncomplete(send); err != nil {
				return &StreamIncompleteError{Transport: "Responses API SSE", Terminal: "response.completed or [DONE]", Err: err}
			}
			return &StreamIncompleteError{Transport: "Responses API SSE", Terminal: "response.completed or [DONE]"}
		}

		return handler.Finish(send)
	}), nil
}

type NonRecoverableStreamError struct {
	Err error
}

func (e *NonRecoverableStreamError) Error() string {
	if e == nil || e.Err == nil {
		return "Responses WebSocket disconnected after partial output; automatic retry is unsafe without rewinding. Partial response preserved."
	}
	return fmt.Sprintf("Responses WebSocket disconnected after partial output; automatic retry is unsafe without rewinding. Partial response preserved. Original error: %v", e.Err)
}

func (e *NonRecoverableStreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type responsesWebSocketFallback struct {
	retry            *Event
	open             func() (Stream, error)
	disableWebSocket func()
}

type responsesWebSocketFallbackStream struct {
	current         Stream
	fallbacks       []responsesWebSocketFallback
	emitted         bool
	pendingFallback *responsesWebSocketFallback
}

func (s *responsesWebSocketFallbackStream) skipWebSocketRetriesForImmediateHTTP(err error) bool {
	var firstEventTimeout *responsesWebSocketFirstEventTimeoutError
	if !errors.As(err, &firstEventTimeout) && !errors.Is(err, errResponsesWebSocketPoolSaturated) {
		return false
	}
	// A transport heartbeat can prove that a socket is alive while the backend
	// has silently dropped this response. Likewise, a saturated account-level
	// connection budget cannot be improved by waiting behind another session.
	// Skip directly to HTTP; ForceWebSocket requests have no such fallback.
	for len(s.fallbacks) > 0 && s.fallbacks[0].retry != nil {
		s.fallbacks = s.fallbacks[1:]
	}
	if len(s.fallbacks) > 0 {
		s.fallbacks[0].disableWebSocket = nil
	}
	return true
}

func responsesWebSocketBackoff(retryAttempt int) time.Duration {
	if retryAttempt < 1 {
		retryAttempt = 1
	}
	backoff := responsesWebSocketBaseBackoff << (retryAttempt - 1)
	if backoff > responsesWebSocketMaxBackoff {
		return responsesWebSocketMaxBackoff
	}
	return backoff
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *responsesWebSocketFallbackStream) Recv() (Event, error) {
	if s.pendingFallback != nil {
		return s.openPendingFallback()
	}
	event, err := s.current.Recv()
	if err != nil {
		if err == io.EOF || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || len(s.fallbacks) == 0 {
			return event, err
		}
		if s.skipWebSocketRetriesForImmediateHTTP(err) && len(s.fallbacks) == 0 {
			return event, err
		}
		if s.emitted {
			return event, &NonRecoverableStreamError{Err: err}
		}
		return s.switchToNextFallback(err)
	}
	if event.Type == EventError {
		if s.skipWebSocketRetriesForImmediateHTTP(event.Err) && len(s.fallbacks) == 0 {
			return event, nil
		}
		if !s.emitted && len(s.fallbacks) > 0 {
			return s.switchToNextFallback(event.Err)
		}
	}
	if event.Type != EventHeartbeat && event.Type != EventRetry {
		s.emitted = true
	}
	return event, nil
}

func (s *responsesWebSocketFallbackStream) openPendingFallback() (Event, error) {
	fb := s.pendingFallback
	s.pendingFallback = nil
	if fb.disableWebSocket != nil {
		fb.disableWebSocket()
	}
	stream, err := fb.open()
	if err != nil {
		return s.switchToNextFallback(err)
	}
	s.current = stream
	return s.Recv()
}

func (s *responsesWebSocketFallbackStream) switchToNextFallback(previousErr error) (Event, error) {
	_ = s.current.Close()
	lastErr := previousErr
	if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
		return Event{}, lastErr
	}
	if s.skipWebSocketRetriesForImmediateHTTP(lastErr) && len(s.fallbacks) == 0 {
		return Event{}, lastErr
	}
	for len(s.fallbacks) > 0 {
		fb := s.fallbacks[0]
		s.fallbacks = s.fallbacks[1:]
		if fb.retry != nil {
			s.pendingFallback = &fb
			return *fb.retry, nil
		}
		if fb.disableWebSocket != nil {
			fb.disableWebSocket()
		}
		stream, err := fb.open()
		if err != nil {
			lastErr = err
			if errors.Is(lastErr, context.Canceled) || errors.Is(lastErr, context.DeadlineExceeded) {
				return Event{}, lastErr
			}
			if s.skipWebSocketRetriesForImmediateHTTP(lastErr) && len(s.fallbacks) == 0 {
				return Event{}, lastErr
			}
			continue
		}
		s.current = stream
		return s.Recv()
	}
	return Event{}, lastErr
}

func (s *responsesWebSocketFallbackStream) Close() error {
	return s.current.Close()
}

// ResetConversation clears server state (called on /clear or new conversation)
func (c *ResponsesClient) ResetConversation() {
	c.closeWebSocket()
	c.responseStateMu.Lock()
	defer c.responseStateMu.Unlock()
	c.responseStateGeneration++
	c.LastResponseID = ""
	c.responseStateSessionID = ""
	c.websocketDisabled = false
	c.wsLastRequest = nil
	c.wsContinuationBaseline = nil
}

func (c *ResponsesClient) websocketServerStateEnabled() bool {
	return c.WebSocketServerState || !c.DisableServerState
}

func trimResponsesContinuationToDiscoveryOutput(input []ResponsesInputItem) []ResponsesInputItem {
	lastOutput := -1
	for i, item := range input {
		if len(item.Raw) == 0 {
			continue
		}
		var ref struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item.Raw, &ref) == nil && ref.Type == "tool_search_output" {
			lastOutput = i
		}
	}
	if lastOutput >= 0 {
		return input[lastOutput:]
	}
	return input
}

// filterToNewInput returns only the new input items for a server-state continuation.
func filterToNewInput(input []ResponsesInputItem) []ResponsesInputItem {
	// Tool follow-up turns append function_call_output items (and sometimes
	// synthetic/user messages such as image parts) with no new user message.
	// When present at the end of the input, send only that trailing suffix.
	start := len(input)
	sawToolOutput := false
	for start > 0 {
		item := input[start-1]
		if item.Type == "message" && item.Role == "user" && !sawToolOutput {
			// A trailing user message may be the next user turn or rich/image
			// content appended after a tool result.
			start--
			continue
		}
		if item.Type == "function_call_output" {
			sawToolOutput = true
			start--
			continue
		}
		// If we have already seen a trailing tool output, a user message before
		// it is part of the prior prompt, not new incremental input.
		break
	}
	if sawToolOutput {
		return input[start:]
	}

	// Otherwise fall back to the latest user message and any following items.
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role == "user" {
			return input[i:]
		}
	}
	return input
}

// responsesToolState tracks streaming tool calls from the Responses API
