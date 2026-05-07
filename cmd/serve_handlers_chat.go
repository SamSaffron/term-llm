package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func (s *serveServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.responseTimeout())
	defer cancel()

	var req chatCompletionsRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	reqStart := time.Now()
	s.verboseLog("→ POST /v1/chat/completions model=%s tools=%d msgs=%d stream=%v body=%d bytes",
		req.Model, len(req.Tools), len(req.Messages), req.Stream, r.ContentLength)
	defer func() { s.verboseLog("← POST /v1/chat/completions completed in %s", time.Since(reqStart)) }()
	if len(req.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	messages, replaceHistory, err := parseChatMessages(req.Messages)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	sessionID := resolveRequestSessionID(r)
	if sessionID == "" {
		sessionID = ensureSessionID(w)
	}
	runtime, stateful, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) || errors.Is(err, errServeSessionLimitReached) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !stateful {
		defer runtime.Close()
	}

	populateMissingToolResultNames(messages, runtime.snapshotHistory())

	search := runtime.search
	requestedTools := parseChatRequestedToolNames(req.Tools)
	toolChoice := parseToolChoice(req.ToolChoice)

	serverTools := runtime.selectTools(requestedTools)
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
	for _, ct := range chatToolsToSpecs(req.Tools) {
		if !serverNames[ct.Name] {
			tools = append(tools, ct)
		}
	}
	if len(tools) == 0 {
		toolChoice = llm.ToolChoice{}
	}
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}

	llmReq := llm.Request{
		SessionID:           sessionID,
		Model:               strings.TrimSpace(req.Model),
		Tools:               tools,
		ToolChoice:          toolChoice,
		ParallelToolCalls:   parallel,
		Search:              search,
		ForceExternalSearch: runtime.forceExternalSearch,
		MaxTurns:            runtime.maxTurns,
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
		s.streamChatCompletions(ctx, w, runtime, stateful, replaceHistory, messages, llmReq, req.StreamOptions, sessionID)
		return
	}

	result, err := runtime.Run(ctx, stateful, replaceHistory, messages, llmReq)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeOpenAIError(w, http.StatusRequestTimeout, "timeout_error", responseRunTimeoutMessage(s.responseTimeout()))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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
	writeJSON(w, http.StatusOK, chatCompletionFinalResponse(result, model))
}

func (s *serveServer) streamChatCompletions(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, streamOpts *chatStreamOptions, sessionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	setSSEHeaders(w)
	flusher.Flush()
	respID := "chatcmpl_" + sessionOrRandomID(sessionID)
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	pingMu, stopPing := sseKeepalive(w, flusher, 20*time.Second)

	ctx, cancelShutdown := s.contextWithShutdown(ctx)
	defer cancelShutdown()

	first := true
	toolCallSeen := false
	toolCallIndex := 0
	result, err := runtime.RunWithEvents(ctx, stateful, replaceHistory, inputMessages, llmReq, func(ev llm.Event) error {
		pingMu.Lock()
		defer pingMu.Unlock()
		var writeErr error
		switch ev.Type {
		case llm.EventTextDelta:
			delta := map[string]any{"content": ev.Text}
			if first {
				delta["role"] = "assistant"
				first = false
			}
			writeErr = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{"index": 0, "delta": delta}},
			})
		case llm.EventToolCall:
			if ev.Tool == nil {
				return nil
			}
			if s.cfg.suppressServerTools && runtime.isServerExecutedTool(ev.Tool.Name) {
				return nil
			}
			toolCallSeen = true
			if first {
				if err := writeChatStreamChunk(w, map[string]any{
					"id":      respID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}}},
				}); err != nil {
					return err
				}
				flusher.Flush()
				first = false
			}
			idx := toolCallIndex
			toolCallIndex++
			writeErr = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": idx,
							"id":    ev.Tool.ID,
							"type":  "function",
							"function": map[string]any{
								"name":      ev.Tool.Name,
								"arguments": string(ev.Tool.Arguments),
							},
						}},
					},
				}},
			})
		case llm.EventHeartbeat:
			writeErr = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{}}},
				"heartbeat": map[string]any{
					"call_id":   ev.ToolCallID,
					"tool_name": ev.ToolName,
				},
			})
		case llm.EventInterjection:
			writeErr = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{"interjection": ev.Text},
				}},
			})
		}
		if writeErr != nil {
			return writeErr
		}
		flusher.Flush()
		return nil
	})
	stopPing() // wait for keepalive goroutine before any final writes

	if err != nil {
		errType := "invalid_request_error"
		errMessage := err.Error()
		if errors.Is(err, errServeSessionBusy) {
			errType = "conflict_error"
		} else if errors.Is(err, context.DeadlineExceeded) {
			errType = "timeout_error"
			errMessage = responseRunTimeoutMessage(s.responseTimeout())
		}
		_ = writeChatStreamChunk(w, map[string]any{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "error", "delta": map[string]any{}}},
			"error":   map[string]any{"message": errMessage, "type": errType},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	finishReason := "stop"
	if toolCallSeen {
		finishReason = "tool_calls"
	}
	_ = writeChatStreamChunk(w, map[string]any{
		"id":      respID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
	})
	if streamOpts != nil && streamOpts.IncludeUsage {
		promptTokens := result.Usage.InputTokens + result.Usage.CachedInputTokens
		totalTokens := promptTokens + result.Usage.OutputTokens
		_ = writeChatStreamChunk(w, map[string]any{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{},
			"usage": map[string]any{
				"prompt_tokens":     promptTokens,
				"completion_tokens": result.Usage.OutputTokens,
				"total_tokens":      totalTokens,
				"prompt_tokens_details": map[string]any{
					"cached_tokens":      result.Usage.CachedInputTokens,
					"cache_write_tokens": result.Usage.CacheWriteTokens,
				},
			},
		})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}
