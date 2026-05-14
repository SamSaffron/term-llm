package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type resolvedResponsesRequest struct {
	req                responsesCreateRequest
	inputMessages      []llm.Message
	replaceHistory     bool
	sessionID          string
	previousResponseID string
	freshConversation  bool
	uiStream           bool
}

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
	ctx, cancel := context.WithTimeout(r.Context(), s.responseTimeout())
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

	// External /v1/responses callers follow OpenAI-style chaining:
	// previous_response_id continues a conversation; no previous response means a
	// fresh conversation, even if a session_id header is reused for persistence.
	headerSessionID := strings.TrimSpace(r.Header.Get("session_id"))
	sessionID := ""
	if req.PreviousResponseID != "" {
		if durable, status, msg := s.resolveDurablePreviousResponseID(ctx, req.PreviousResponseID, headerSessionID, inputMessages); status != 0 {
			errType := "invalid_request_error"
			if status == http.StatusConflict {
				errType = "conflict_error"
			}
			writeOpenAIError(w, status, errType, msg)
			return
		} else if durable.sessionID != "" {
			sessionID = durable.sessionID
		} else {
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
			if headerSessionID != "" && headerSessionID != sidStr {
				writeOpenAIError(w, http.StatusConflict, "conflict_error",
					fmt.Sprintf("session_id %q conflicts with previous_response_id session %q", headerSessionID, sidStr))
				return
			}
			sessionID = sidStr
		}
	}
	if sessionID == "" {
		sessionID = headerSessionID
		if sessionID == "" {
			sessionID = session.NewID()
		}
		w.Header().Set("x-session-id", sessionID)
		// External Responses API semantics: no previous_response_id means the
		// supplied input is the new whole conversation for this persisted ID.
		replaceHistory = true
	}

	s.handleResolvedResponses(w, r, ctx, resolvedResponsesRequest{
		req:                req,
		inputMessages:      inputMessages,
		replaceHistory:     replaceHistory,
		sessionID:          sessionID,
		previousResponseID: req.PreviousResponseID,
		freshConversation:  req.PreviousResponseID == "",
	})
}

func (s *serveServer) handleResolvedResponses(w http.ResponseWriter, r *http.Request, ctx context.Context, rr resolvedResponsesRequest) {
	req := rr.req
	inputMessages := rr.inputMessages
	replaceHistory := rr.replaceHistory
	sessionID := rr.sessionID
	previousResponseID := rr.previousResponseID
	freshConversation := rr.freshConversation
	// Chained requests are locked to the persisted provider/model/
	// reasoning_effort unless the client explicitly asks for a mid-conversation
	// model swap. External bare session_id requests start a fresh conversation,
	// even when reusing an existing session ID, so fresh conversations may choose
	// new runtime settings and syncPersistedSessionRuntime will update the row.
	// First-party UI append requests are stateful appends.
	defaultProvider := ""
	if s.cfgRef != nil {
		defaultProvider = strings.TrimSpace(s.cfgRef.DefaultProvider)
	}
	requestedRuntime := responseRequestedRuntime(req, defaultProvider)
	persistedRuntime := requestedRuntime
	if !freshConversation {
		persistedRuntime = s.persistedRuntimeSettings(ctx, sessionID, defaultProvider)
	}
	swapPlan := responseModelSwapPlan{}
	if !freshConversation {
		swapPlan = buildResponseModelSwapPlan(req, persistedRuntime, requestedRuntime)
	}

	reqProvider := strings.TrimSpace(req.Provider)
	if !freshConversation && !swapPlan.enabled && s.store != nil {
		if persistedRuntime.provider != "" {
			reqProvider = persistedRuntime.provider
			if persistedRuntime.model != "" {
				req.Model = persistedRuntime.model
			}
			req.ReasoningEffort = persistedRuntime.effort
		}
	}
	if swapPlan.enabled {
		reqProvider = swapPlan.requestedProvider
		req.Model = swapPlan.requestedModel
		req.ReasoningEffort = swapPlan.requestedEffort
	}

	handleRuntimeErr := func(err error) bool {
		if err == nil {
			return false
		}
		if errors.Is(err, errServeSessionBusy) {
			if req.Stream {
				model := strings.TrimSpace(req.Model)
				if model == "" {
					if existing, ok := s.sessionMgr.Get(sessionID); ok && existing != nil {
						model = existing.defaultModel
					}
				}
				s.streamFailedResponseRun(ctx, w, sessionID, previousResponseID, model, "conflict_error", err.Error())
				return true
			}
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return true
		}
		if errors.Is(err, errServeSessionLimitReached) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
			return true
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}

	var runtime *serveRuntime
	var stateful bool
	var modelSwapExec *responseModelSwapExecution
	freshProvider := reqProvider
	if freshConversation && freshProvider == "" {
		freshProvider = defaultProvider
	}
	if swapPlan.enabled {
		previousRuntime, previousStateful, getErr := s.runtimeForProviderModelRequest(ctx, sessionID, swapPlan.previousProvider, swapPlan.previousModel)
		if handleRuntimeErr(getErr) {
			return
		}
		// Enforce chaining from the latest response before replacing the session runtime.
		if previousResponseID != "" {
			lastRespID := previousRuntime.getLastResponseID()
			if lastRespID == "" {
				if latest, ok := s.sessionToResponse.Load(sessionID); ok {
					if latestStr, ok := latest.(string); ok {
						lastRespID = strings.TrimSpace(latestStr)
					}
				}
			}
			if lastRespID != "" && previousResponseID != lastRespID {
				writeOpenAIError(w, http.StatusConflict, "conflict_error",
					fmt.Sprintf("previous_response_id %q is stale; latest is %q", previousResponseID, lastRespID))
				if !previousStateful {
					s.unregisterResponseIDs(previousRuntime)
					previousRuntime.Close()
				}
				return
			}
			s.populateResponsesToolResultNames(ctx, sessionID, previousRuntime, inputMessages)
		}
		var err error
		modelSwapExec, err = s.beginResponseModelSwap(ctx, sessionID, swapPlan, inputMessages)
		if handleRuntimeErr(err) {
			return
		}
		runtime = modelSwapExec.candidate
		stateful = true
	} else {
		var err error
		if previousResponseID != "" || rr.uiStream {
			runtime, stateful, err = s.runtimeForProviderRequest(ctx, sessionID, reqProvider)
		} else {
			runtime, stateful, err = s.runtimeForFreshProviderRequest(ctx, sessionID, freshProvider)
		}
		if handleRuntimeErr(err) {
			return
		}
		if freshConversation {
			s.syncPersistedSessionRuntime(ctx, sessionID, runtime, strings.TrimSpace(req.Model), normalizeReasoningEffort(req.ReasoningEffort))
		}

		// Enforce chaining from the latest response only. Stale response IDs that
		// map to a valid session but don't match the runtime's last response would
		// produce incorrect branching (the context wouldn't match what the client
		// expects from that response).
		if previousResponseID != "" {
			lastRespID := runtime.getLastResponseID()
			if lastRespID == "" {
				if latest, ok := s.sessionToResponse.Load(sessionID); ok {
					if latestStr, ok := latest.(string); ok {
						lastRespID = strings.TrimSpace(latestStr)
					}
				}
			}
			if lastRespID != "" && previousResponseID != lastRespID {
				writeOpenAIError(w, http.StatusConflict, "conflict_error",
					fmt.Sprintf("previous_response_id %q is stale; latest is %q", previousResponseID, lastRespID))
				if !stateful {
					s.unregisterResponseIDs(runtime)
					runtime.Close()
				}
				return
			}
			s.populateResponsesToolResultNames(ctx, sessionID, runtime, inputMessages)
		}
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
	serverTools := responseServerTools(runtime, requestedTools, req.IncludeServerTools)
	tools := appendResponsePassthroughTools(serverTools, passthroughTools, runtime.toolMap)
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
		ReasoningEffort:     normalizeReasoningEffort(req.ReasoningEffort),
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

	resetResponseIDsOnSuccess := freshConversation || swapPlan.enabled
	if req.Stream && s.store != nil {
		if num := runtime.ensureSessionInStore(r.Context(), sessionID, inputMessages); num > 0 {
			w.Header().Set("x-session-number", strconv.FormatInt(num, 10))
		}
	}
	if req.Stream {
		if rr.uiStream && stateful {
			s.streamUIResponses(w, r, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, previousResponseID, resetResponseIDsOnSuccess, modelSwapExec)
		} else {
			started := s.streamResponses(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, previousResponseID, resetResponseIDsOnSuccess, modelSwapExec)
			if !stateful && started {
				cleanupRuntime = false
			}
		}
		return
	}

	result, _, err := s.runResponseWithModelSwapFallback(ctx, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, modelSwapExec)
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

	setSessionNumberHeader(w, runtime)

	created := time.Now().Unix()
	respID, err := s.storeCompletedResponseRun(runtime, sessionID, previousResponseID, model, created, result, resetResponseIDsOnSuccess)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, responsesFinalResponse(result, model, respID, created))
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

func responseServerTools(runtime *serveRuntime, requested map[string]bool, includeServerTools bool) []llm.ToolSpec {
	if includeServerTools {
		return runtime.selectTools(nil)
	}
	return runtime.selectTools(requested)
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

func (s *serveServer) streamResponses(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, previousResponseID string, resetResponseIDsOnSuccess bool, modelSwap *responseModelSwapExecution) bool {
	return s.streamResponseRun(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, startResponseRunOptions{
		previousResponseID:        previousResponseID,
		resetResponseIDsOnSuccess: resetResponseIDsOnSuccess,
		modelSwap:                 modelSwap,
	})
}
