package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	var req responsesCreateRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	reqStart := time.Now()
	s.verboseLog("→ POST /v1/responses model=%s tools=%d stream=%v body=%d bytes",
		req.Model, len(req.Tools), req.Stream, r.ContentLength)
	defer func() { s.verboseLog("← POST /v1/responses completed in %s", time.Since(reqStart)) }()

	inputMessages, replaceHistory, err := parseResponsesInput(req.Input)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(inputMessages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
		return
	}

	// Resolve session: previous_response_id for chaining, otherwise fresh.
	// session_id header provides the ID for persistence but does NOT reuse
	// an existing conversation without explicit chaining.
	sessionID := ""
	if req.PreviousResponseID != "" {
		sid, ok := s.responseToSession.Load(req.PreviousResponseID)
		if !ok {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("previous_response_id %q not found (session may have expired)", req.PreviousResponseID))
			return
		}
		sidStr, isStr := sid.(string)
		if !isStr || sidStr == "" {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "corrupted session mapping")
			return
		}
		sessionID = sidStr
	}
	if sessionID == "" {
		// No chaining — unconditionally fresh conversation.
		sessionID = strings.TrimSpace(r.Header.Get("session_id"))
		if sessionID == "" {
			sessionID = session.NewID()
		}
		w.Header().Set("x-session-id", sessionID)
		replaceHistory = true
	}
	// Use requested provider for new sessions only. For chained requests,
	// recover the persisted session provider so runtime recreation preserves
	// the original provider/model continuity after eviction.
	reqProvider := strings.TrimSpace(req.Provider)
	if req.PreviousResponseID != "" && reqProvider == "" && s.store != nil {
		if sess, getErr := s.store.Get(ctx, sessionID); getErr == nil && sess != nil {
			reqProvider = strings.TrimSpace(sess.ProviderKey)
			if reqProvider == "" {
				reqProvider = resolveSessionProviderKey(s.cfgRef, sess)
			}
		}
	}
	defaultProvider := ""
	if s.cfgRef != nil {
		defaultProvider = strings.TrimSpace(s.cfgRef.DefaultProvider)
	}
	var runtime *serveRuntime
	var stateful bool
	freshConversation := req.PreviousResponseID == ""
	freshProvider := reqProvider
	if freshConversation && freshProvider == "" {
		freshProvider = defaultProvider
	}
	if req.PreviousResponseID != "" {
		runtime, stateful, err = s.runtimeForProviderRequest(ctx, sessionID, reqProvider)
	} else {
		runtime, stateful, err = s.runtimeForFreshProviderRequest(ctx, sessionID, freshProvider)
	}
	if err != nil {
		if errors.Is(err, errServeSessionBusy) || errors.Is(err, errServeSessionLimitReached) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if freshConversation {
		s.sessionToResponse.Delete(sessionID)
		s.syncPersistedSessionRuntime(ctx, sessionID, runtime)
	}

	// Enforce chaining from the latest response only. Stale response IDs that
	// map to a valid session but don't match the runtime's last response would
	// produce incorrect branching (the context wouldn't match what the client
	// expects from that response).
	if req.PreviousResponseID != "" {
		lastRespID := runtime.getLastResponseID()
		if lastRespID == "" {
			if latest, ok := s.sessionToResponse.Load(sessionID); ok {
				if latestStr, ok := latest.(string); ok {
					lastRespID = strings.TrimSpace(latestStr)
				}
			}
		}
		if lastRespID != "" && req.PreviousResponseID != lastRespID {
			writeOpenAIError(w, http.StatusConflict, "conflict_error",
				fmt.Sprintf("previous_response_id %q is stale; latest is %q", req.PreviousResponseID, lastRespID))
			if !stateful {
				s.unregisterResponseIDs(runtime)
				runtime.Close()
			}
			return
		}
		s.populateResponsesToolResultNames(ctx, sessionID, runtime, inputMessages)
	}
	cleanupRuntime := !stateful
	if cleanupRuntime {
		defer func() {
			if !cleanupRuntime {
				return
			}
			runtime.Close()
			s.unregisterResponseIDs(runtime)
		}()
	}

	searchFromTools, requestedTools, passthroughTools := parseRequestedTools(req.Tools)
	search := runtime.search || searchFromTools
	toolChoice := parseToolChoice(req.ToolChoice)
	tools := appendResponsePassthroughTools(runtime.selectTools(requestedTools), passthroughTools, runtime.toolMap)
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

	if req.MaxOutputTokens > 0 {
		llmReq.MaxOutputTokens = req.MaxOutputTokens
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
		if isServeUIRequest(r) && stateful {
			s.streamUIResponses(w, r, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, req.PreviousResponseID)
		} else {
			started := s.streamResponses(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, req.PreviousResponseID)
			if !stateful && started {
				cleanupRuntime = false
			}
		}
		return
	}

	result, err := runtime.Run(ctx, stateful, replaceHistory, inputMessages, llmReq)
	if err != nil {
		if errors.Is(err, errServeSessionBusy) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
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

	setSessionNumberHeader(w, runtime)

	respID := "resp_" + randomSuffix()
	s.registerResponseID(runtime, respID, sessionID)

	writeJSON(w, http.StatusOK, responsesFinalResponse(result, model, respID))
}

func (s *serveServer) populateResponsesToolResultNames(ctx context.Context, sessionID string, runtime *serveRuntime, messages []llm.Message) {
	missing := map[string]bool{}
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type != llm.PartToolResult || part.ToolResult == nil {
				continue
			}
			id := strings.TrimSpace(part.ToolResult.ID)
			if id == "" || strings.TrimSpace(part.ToolResult.Name) != "" {
				continue
			}
			missing[id] = true
		}
	}
	if len(missing) == 0 {
		return
	}

	names := map[string]string{}
	collect := func(history []llm.Message) {
		for i := len(history) - 1; i >= 0; i-- {
			for _, part := range history[i].Parts {
				if part.Type != llm.PartToolCall || part.ToolCall == nil {
					continue
				}
				id := strings.TrimSpace(part.ToolCall.ID)
				if id == "" || !missing[id] || names[id] != "" {
					continue
				}
				if name := strings.TrimSpace(part.ToolCall.Name); name != "" {
					names[id] = name
				}
			}
			if len(names) == len(missing) {
				return
			}
		}
	}

	if s.store != nil && sessionID != "" {
		if stored, err := s.store.GetMessages(ctx, sessionID, 0, 0); err == nil {
			persisted := make([]llm.Message, 0, len(stored))
			for _, msg := range stored {
				persisted = append(persisted, msg.ToLLMMessage())
			}
			collect(persisted)
		}
	}
	if len(names) < len(missing) && runtime != nil {
		collect(runtime.snapshotHistory())
	}
	if len(names) == 0 {
		return
	}

	for msgIndex := range messages {
		for partIndex := range messages[msgIndex].Parts {
			part := &messages[msgIndex].Parts[partIndex]
			if part.Type != llm.PartToolResult || part.ToolResult == nil {
				continue
			}
			if strings.TrimSpace(part.ToolResult.Name) != "" {
				continue
			}
			if name := names[strings.TrimSpace(part.ToolResult.ID)]; name != "" {
				part.ToolResult.Name = name
			}
		}
	}
}

func appendResponsePassthroughTools(serverTools []llm.ToolSpec, passthroughTools []llm.ToolSpec, toolMap map[string]string) []llm.ToolSpec {
	if len(passthroughTools) == 0 {
		return serverTools
	}

	selected := make(map[string]bool, len(serverTools))
	for _, spec := range serverTools {
		selected[spec.Name] = true
	}

	for _, spec := range passthroughTools {
		if selected[spec.Name] {
			continue
		}
		if mapped, ok := toolMap[spec.Name]; ok && selected[mapped] {
			continue
		}
		serverTools = append(serverTools, spec)
		selected[spec.Name] = true
	}

	return serverTools
}

func (s *serveServer) streamResponses(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, previousResponseID string) bool {
	return s.streamResponseRun(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, startResponseRunOptions{
		previousResponseID: previousResponseID,
	})
}
