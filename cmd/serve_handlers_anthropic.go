package cmd

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func (s *serveServer) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeAnthropicError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	var req anthropicMessagesRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	reqStart := time.Now()
	sysLen := len(req.System)
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	s.verboseLog("→ POST /v1/messages model=%s tools=%d msgs=%d roles=%v system=%d stream=%v body=%d bytes",
		req.Model, len(req.Tools), len(req.Messages), roles, sysLen, req.Stream, r.ContentLength)
	defer func() { s.verboseLog("← POST /v1/messages completed in %s", time.Since(reqStart)) }()
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	messages, err := parseAnthropicMessages(req.Messages)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Prepend system message if provided
	if sysText := parseAnthropicSystem(req.System); sysText != "" {
		messages = append([]llm.Message{llm.SystemText(sysText)}, messages...)
	}

	// Anthropic Messages API: each request carries the full conversation,
	// so always replace session history instead of appending.
	replaceHistory := true

	sessionID := resolveRequestSessionID(r)
	if sessionID == "" {
		sessionID = ensureSessionID(w)
	}
	runtime, stateful, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) || errors.Is(err, errServeSessionLimitReached) {
			writeAnthropicError(w, http.StatusConflict, "api_error", err.Error())
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !stateful {
		defer runtime.Close()
	}

	search := runtime.search
	requestedTools := parseAnthropicRequestedToolNames(req.Tools)
	toolChoice := parseAnthropicToolChoice(req.ToolChoice)

	// Merge requested server tools (engine can execute) with client tools
	// (passthrough). Server tools win on name collision so the engine executes
	// its own version. Client tools the engine doesn't recognise are forwarded
	// to the LLM and returned as tool_use blocks for the client to handle.
	//
	// ToolMap targets (e.g. "search" when mapping "WebSearch" → "search") are
	// excluded from the server list — the LLM will see the client's tool name
	// and the engine redirects execution via ToolMap.
	serverTools := []llm.ToolSpec(nil)
	if len(requestedTools) > 0 {
		serverTools = runtime.selectTools(requestedTools)
	}
	mappedTargets := make(map[string]bool)
	if runtime.toolMap != nil {
		for name := range requestedTools {
			if mapped, ok := runtime.toolMap[name]; ok {
				mappedTargets[mapped] = true
			}
		}
	}
	serverNames := make(map[string]bool, len(serverTools))
	tools := make([]llm.ToolSpec, 0, len(serverTools)+len(req.Tools))
	for _, t := range serverTools {
		serverNames[t.Name] = true
		if !mappedTargets[t.Name] {
			tools = append(tools, t)
		}
	}
	for _, ct := range anthropicToolsToSpecs(req.Tools) {
		if !serverNames[ct.Name] {
			tools = append(tools, ct)
		}
	}
	if len(tools) == 0 {
		toolChoice = llm.ToolChoice{}
	}

	// Don't pass the client's model name to the provider — clients like
	// Claude Code send their own model names (e.g. "claude-sonnet-4-6")
	// which the backend provider won't recognize. Leave Model empty so
	// chooseModel falls through to the provider's configured model.
	// The response uses runtime.defaultModel (the server-side model name).
	llmReq := llm.Request{
		SessionID:           sessionID,
		Tools:               tools,
		ToolChoice:          toolChoice,
		ParallelToolCalls:   true,
		Search:              search,
		ForceExternalSearch: runtime.forceExternalSearch,
		ToolMap:             runtime.toolMap,
		Debug:               runtime.debug,
		DebugRaw:            runtime.debugRaw,
	}
	if req.MaxTokens > 0 {
		llmReq.MaxOutputTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		llmReq.Temperature = *req.Temperature
		llmReq.TemperatureSet = true
	}
	if req.TopP != nil {
		llmReq.TopP = *req.TopP
		llmReq.TopPSet = true
	}

	if req.Stream {
		s.streamAnthropicMessages(ctx, w, runtime, stateful, replaceHistory, messages, llmReq, sessionID)
		return
	}

	result, err := runtime.Run(ctx, stateful, replaceHistory, messages, llmReq)
	if err != nil {
		s.verboseLog("✗ POST /v1/messages error: %v", err)
		if errors.Is(err, errServeSessionBusy) {
			writeAnthropicError(w, http.StatusConflict, "api_error", err.Error())
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if s.cfg.suppressServerTools {
		filtered := make([]llm.ToolCall, 0, len(result.ToolCalls))
		for _, call := range result.ToolCalls {
			if !runtime.isServerExecutedTool(call.Name) {
				filtered = append(filtered, call)
			}
		}
		result.ToolCalls = filtered
	}

	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	writeJSON(w, http.StatusOK, anthropicMessagesFinalResponse(result, model))
}

func (s *serveServer) streamAnthropicMessages(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming not supported")
		return
	}

	setSSEHeaders(w)
	flusher.Flush()

	msgID := "msg_" + sessionOrRandomID(sessionID)
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}

	pingMu, stopPing := sseKeepalive(w, flusher, 20*time.Second)

	var (
		blockIndex int
		openBlock  string // "", "text", or "thinking"
		toolSeen   bool
	)

	// closeBlock closes the currently open content block, if any.
	closeBlock := func() {
		if openBlock != "" {
			_ = writeAnthropicSSE(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIndex,
			})
			flusher.Flush()
			blockIndex++
			openBlock = ""
		}
	}

	// Send message_start
	pingMu.Lock()
	_ = writeAnthropicSSE(w, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
	flusher.Flush()
	pingMu.Unlock()

	result, err := runtime.RunWithEvents(ctx, stateful, replaceHistory, inputMessages, llmReq, func(ev llm.Event) error {
		pingMu.Lock()
		defer pingMu.Unlock()
		var writeErr error
		switch ev.Type {
		case llm.EventTextDelta:
			if openBlock != "text" {
				closeBlock()
				writeErr = writeAnthropicSSE(w, "content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
				if writeErr != nil {
					return writeErr
				}
				flusher.Flush()
				openBlock = "text"
			}
			writeErr = writeAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": ev.Text},
			})
		case llm.EventToolCall:
			if ev.Tool == nil {
				return nil
			}
			// Suppress tool_use blocks for server-executed tools —
			// the engine handles these internally and the client
			// should only see the final text result.
			if s.cfg.suppressServerTools && runtime.isServerExecutedTool(ev.Tool.Name) {
				return nil
			}
			toolSeen = true
			closeBlock()
			// content_block_start for tool_use
			writeErr = writeAnthropicSSE(w, "content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": blockIndex,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    ev.Tool.ID,
					"name":  ev.Tool.Name,
					"input": map[string]any{},
				},
			})
			if writeErr != nil {
				return writeErr
			}
			flusher.Flush()
			// content_block_delta with input_json_delta
			writeErr = writeAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": string(ev.Tool.Arguments),
				},
			})
			if writeErr != nil {
				return writeErr
			}
			flusher.Flush()
			// content_block_stop
			writeErr = writeAnthropicSSE(w, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": blockIndex,
			})
			blockIndex++
		case llm.EventReasoningDelta:
			if openBlock != "thinking" {
				closeBlock()
				writeErr = writeAnthropicSSE(w, "content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]any{"type": "thinking", "thinking": ""},
				})
				if writeErr != nil {
					return writeErr
				}
				flusher.Flush()
				openBlock = "thinking"
			}
			writeErr = writeAnthropicSSE(w, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
			})
		}
		if writeErr != nil {
			return writeErr
		}
		flusher.Flush()
		return nil
	})
	stopPing()

	// Close any open content block
	closeBlock()

	stopReason := "end_turn"
	if toolSeen {
		stopReason = "tool_use"
	}
	if err != nil {
		s.verboseLog("✗ POST /v1/messages stream error: %v", err)
		if errors.Is(err, errServeSessionBusy) {
			_ = writeAnthropicSSE(w, "error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": err.Error(),
				},
			})
			flusher.Flush()
			return
		}
		_ = writeAnthropicSSE(w, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": err.Error(),
			},
		})
		flusher.Flush()
		return
	}

	outputTokens := 0
	inputTokens := 0
	if result.Usage.OutputTokens > 0 || result.Usage.InputTokens > 0 {
		outputTokens = result.Usage.OutputTokens
		inputTokens = result.Usage.InputTokens
	}

	_ = writeAnthropicSSE(w, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	})
	flusher.Flush()

	_ = writeAnthropicSSE(w, "message_stop", map[string]any{
		"type": "message_stop",
	})
	flusher.Flush()

	// Also emit a final ping with usage for clients that use it
	_ = writeAnthropicSSE(w, "ping", map[string]any{
		"type": "ping",
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})
	flusher.Flush()
}
