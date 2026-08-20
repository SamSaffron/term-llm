package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

var errResponseClientMessageAlreadyCommitted = errors.New("response client message is already committed")

const maxResponseClientMessageIDLength = 200

type followUpClaimLease struct {
	once   sync.Once
	engine *llm.Engine
	ids    []string
}

func (l *followUpClaimLease) Release() {
	if l == nil || l.engine == nil || len(l.ids) == 0 {
		return
	}
	l.once.Do(func() { l.engine.ReleaseClaimedInterjections(l.ids) })
}

type resolvedResponsesRequest struct {
	req                responsesCreateRequest
	inputMessages      []llm.Message
	replaceHistory     bool
	sessionID          string
	previousResponseID string
	previousDurable    bool
	freshConversation  bool
	durableRuntime     bool
	uiStream           bool
	idempotencyKey     string
}

func validateResponseReasoningMode(provider, model, mode string, explicit bool) (normalized string, clearStale bool, err error) {
	normalized = strings.ToLower(strings.TrimSpace(mode))
	if normalized == "" {
		return "", false, nil
	}
	if normalized != "standard" && normalized != "pro" {
		if explicit {
			return "", false, fmt.Errorf("reasoning.mode must be standard or pro")
		}
		return "", true, nil
	}
	if !llm.SupportsReasoningMode(provider, model) {
		if explicit {
			return "", false, fmt.Errorf("reasoning.mode is not supported by provider %q model %q", provider, model)
		}
		return "", true, nil
	}
	return normalized, false, nil
}

func prepareResponseClientMessageIDs(messages []llm.Message, requestID string, firstParty bool) (bool, error) {
	requestID = strings.TrimSpace(requestID)
	if !firstParty {
		// Item-level identities are reserved for the first-party UI contract.
		for i := range messages {
			messages[i].ClientMessageID = ""
		}
		if requestID != "" {
			for i := len(messages) - 1; i >= 0; i-- {
				if messages[i].Role == llm.RoleUser {
					messages[i].ClientMessageID = requestID
					break
				}
			}
		}
		return false, nil
	}

	userIndexes := make([]int, 0, len(messages))
	for i := range messages {
		if messages[i].Role == llm.RoleUser {
			userIndexes = append(userIndexes, i)
		}
	}
	identifiedBatch := len(messages) > 1 && len(userIndexes) == len(messages)
	if !identifiedBatch {
		if len(userIndexes) > 0 {
			last := userIndexes[len(userIndexes)-1]
			itemID := strings.TrimSpace(messages[last].ClientMessageID)
			if itemID != "" && itemID != requestID {
				return false, fmt.Errorf("top-level client_message_id must match the final user message")
			}
			for _, index := range userIndexes {
				messages[index].ClientMessageID = ""
			}
			messages[last].ClientMessageID = requestID
		}
		return false, nil
	}

	ids := make([]string, 0, len(messages))
	seen := make(map[string]struct{}, len(messages))
	for i := range messages {
		id := strings.TrimSpace(messages[i].ClientMessageID)
		if id == "" {
			return false, fmt.Errorf("each batched user message requires client_message_id")
		}
		if len(id) > maxResponseClientMessageIDLength {
			return false, fmt.Errorf("client_message_id is too long")
		}
		if _, duplicate := seen[id]; duplicate {
			return false, fmt.Errorf("duplicate client_message_id %q in user batch", id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if ids[len(ids)-1] != requestID {
		return false, fmt.Errorf("top-level client_message_id must match the final batched user message")
	}
	return true, nil
}

func responseClientMessageIDs(messages []llm.Message) []string {
	ids := make([]string, 0, len(messages))
	for i := range messages {
		if messages[i].Role != llm.RoleUser {
			continue
		}
		if id := strings.TrimSpace(messages[i].ClientMessageID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
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
	clientMessageID := strings.TrimSpace(req.ClientMessageID)
	firstParty := isFirstPartyUIResponseRequest(r)
	if firstParty && clientMessageID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "client_message_id is required for first-party requests")
		return
	}
	if len(clientMessageID) > maxResponseClientMessageIDLength {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "client_message_id is too long")
		return
	}
	identifiedUserBatch, err := prepareResponseClientMessageIDs(inputMessages, clientMessageID, firstParty)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if identifiedUserBatch && req.PreviousResponseID != "" {
		replaceHistory = false
	}
	if req.Branch && strings.TrimSpace(req.PreviousResponseID) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "branch requires previous_response_id")
		return
	}
	if req.BranchContext != nil && !req.Branch {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "branch_context requires branch=true")
		return
	}

	// External /v1/responses callers follow OpenAI-style chaining:
	// previous_response_id continues a conversation; no previous response means a
	// fresh conversation, even if a session_id header is reused for persistence.
	headerSessionID := strings.TrimSpace(r.Header.Get("session_id"))
	sessionID := ""
	previousDurable := false
	resolvedPreviousResponseID := req.PreviousResponseID
	branched := false
	if req.PreviousResponseID != "" {
		if req.Branch {
			idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
			if idempotencyKey == "" {
				idempotencyKey = responseIdempotencyKeyFromRequest(r)
			}
			durable, status, msg := s.resolveDurableBranch(ctx, req.PreviousResponseID, headerSessionID, inputMessages, identifiedUserBatch, req.ExpectedRev, idempotencyKey, req.BranchContext)
			if status != 0 {
				errType := "invalid_request_error"
				if status == http.StatusConflict {
					errType = "conflict_error"
				} else if status >= http.StatusInternalServerError {
					errType = "server_error"
				}
				writeOpenAIError(w, status, errType, msg)
				return
			}
			sessionID = durable.sessionID
			resolvedPreviousResponseID = durable.latestID
			previousDurable = true
			branched = true
			w.Header().Set("x-session-id", sessionID)
			if durable.latestID != durableResponseMessagePrefix+"0" {
				w.Header().Set("x-branch-anchor-id", durable.latestID)
			}
		} else if durable, status, msg := s.resolveDurablePreviousResponseID(ctx, req.PreviousResponseID, headerSessionID, inputMessages, identifiedUserBatch); status != 0 {
			errType := "invalid_request_error"
			if status == http.StatusConflict {
				errType = "conflict_error"
			}
			writeOpenAIError(w, status, errType, msg)
			return
		} else if durable.sessionID != "" {
			sessionID = durable.sessionID
			previousDurable = true
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

	if _, branchContextActive := s.branchNotes.Load(sessionID); branchContextActive {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "branch context is still being prepared")
		return
	}

	runIdempotencyKey := responseRunIdempotencyKey(r, req)
	s.handleResolvedResponses(w, r, ctx, resolvedResponsesRequest{
		req:                req,
		inputMessages:      inputMessages,
		replaceHistory:     replaceHistory,
		sessionID:          sessionID,
		previousResponseID: resolvedPreviousResponseID,
		previousDurable:    previousDurable,
		freshConversation:  req.PreviousResponseID == "",
		durableRuntime:     branched,
		uiStream:           branchUsesFirstPartyUIStream(r, branched),
		idempotencyKey:     runIdempotencyKey,
	})
}

func (s *serveServer) handleResolvedResponses(w http.ResponseWriter, r *http.Request, ctx context.Context, rr resolvedResponsesRequest) {
	req := rr.req
	inputMessages := rr.inputMessages
	if isFirstPartyUIResponseRequest(r) && len(responseClientMessageIDs(inputMessages)) == 0 {
		clientMessageID := strings.TrimSpace(req.ClientMessageID)
		if clientMessageID != "" {
			for i := len(inputMessages) - 1; i >= 0; i-- {
				if inputMessages[i].Role == llm.RoleUser {
					inputMessages[i].ClientMessageID = clientMessageID
					break
				}
			}
		}
	}
	replaceHistory := rr.replaceHistory
	sessionID := rr.sessionID
	previousResponseID := rr.previousResponseID
	previousDurable := rr.previousDurable
	freshConversation := rr.freshConversation
	idempotencyKey := strings.TrimSpace(rr.idempotencyKey)
	if req.Stream && idempotencyKey != "" {
		// Streaming response runs retain their event log for the response-run
		// retention window, so an idempotency replay can attach directly without
		// rebuilding runtime/provider state. Replay is unconditional for a matching
		// session/key; first-party UI keys are unique per logical user message.
		if run, ok := s.ensureResponseRuns().getByIdempotencyKey(sessionID, idempotencyKey); ok {
			w.Header().Set("x-response-id", run.id)
			s.setReplaySessionNumberHeader(ctx, w, sessionID)
			s.streamResponseRunEvents(ctx, w, run, 0)
			return
		}
	}
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
	req.Model = requestedRuntime.model
	req.ReasoningEffort = requestedRuntime.effort
	persistedRuntime := requestedRuntime
	if !freshConversation {
		persistedRuntime = s.persistedRuntimeSettings(ctx, sessionID, defaultProvider)
		if requestedRuntime.reasoningMode == "" && persistedRuntime.reasoningMode != "" {
			if req.Reasoning == nil {
				req.Reasoning = &responsesReasoningRequest{}
			}
			req.Reasoning.Mode = persistedRuntime.reasoningMode
		}
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
		if errors.Is(err, errServeSessionPersistence) {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
			return true
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return true
	}

	var runtime *serveRuntime
	var stateful bool
	var modelSwapExec *responseModelSwapExecution
	var followUpOwner *serveRuntime
	var followUpOwnerStateful bool
	var followUpClaims *followUpClaimLease

	claimUIFollowUp := func(rt *serveRuntime, ownsSession bool) error {
		clientMessageIDs := responseClientMessageIDs(inputMessages)
		if !ownsSession || len(clientMessageIDs) == 0 || !isFirstPartyUIResponseRequest(r) || rt == nil {
			return nil
		}

		rt.mu.Lock()
		if rt.store != nil && !rt.ensurePersistedSession(ctx, sessionID, inputMessages) {
			rt.mu.Unlock()
			return errors.New("failed to hydrate session history before client message claim")
		}
		history := copyLLMMessageSlice(rt.history)
		rt.mu.Unlock()

		trailingStart := len(history)
		for trailingStart > 0 && history[trailingStart-1].Role == llm.RoleUser {
			trailingStart--
		}
		foundInHistory := make(map[string]bool, len(clientMessageIDs))
		trailingUnanswered := make(map[string]bool, len(clientMessageIDs))
		for i := range history {
			id := strings.TrimSpace(history[i].ClientMessageID)
			if id == "" {
				continue
			}
			foundInHistory[id] = true
			if i >= trailingStart {
				trailingUnanswered[id] = true
			}
		}

		durableMessages := make(map[string]*session.Message)
		if rt.store != nil {
			var lookupErr error
			durableMessages, lookupErr = session.FindMessagesByClientMessageIDs(ctx, rt.store, sessionID, clientMessageIDs)
			if lookupErr != nil {
				return fmt.Errorf("lookup client_message_ids: %w", lookupErr)
			}
		}
		for _, clientMessageID := range clientMessageIDs {
			_, durableExists := durableMessages[clientMessageID]
			if (foundInHistory[clientMessageID] || durableExists) && !trailingUnanswered[clientMessageID] {
				return fmt.Errorf("%w: %q", errResponseClientMessageAlreadyCommitted, clientMessageID)
			}
		}

		claims := make([]llm.InterjectionClaimStatus, len(clientMessageIDs))
		for i := range claims {
			claims[i] = llm.InterjectionClaimNotFound
		}
		if rt.engine != nil {
			claims = rt.engine.ClaimInterjections(clientMessageIDs)
		}
		claimedIDs := make([]string, 0, len(clientMessageIDs))
		hasNewClaim := false
		hasExistingOwner := false
		for _, claim := range claims {
			hasNewClaim = hasNewClaim || claim == llm.InterjectionClaimed
			hasExistingOwner = hasExistingOwner || claim == llm.InterjectionClaimFollowUpOwned
		}
		if hasNewClaim && hasExistingOwner {
			return errServeSessionBusy
		}
		for i, claim := range claims {
			switch claim {
			case llm.InterjectionClaimed:
				claimedIDs = append(claimedIDs, clientMessageIDs[i])
			case llm.InterjectionClaimCommitted:
				return fmt.Errorf("%w: %q", errResponseClientMessageAlreadyCommitted, clientMessageIDs[i])
			case llm.InterjectionClaimFollowUpOwned:
				if !trailingUnanswered[clientMessageIDs[i]] {
					return errServeSessionBusy
				}
			}
		}
		if len(claimedIDs) > 0 {
			followUpClaims = &followUpClaimLease{engine: rt.engine, ids: claimedIDs}
		}
		return nil
	}

	freshProvider := reqProvider
	if freshConversation && freshProvider == "" {
		freshProvider = defaultProvider
	}
	if swapPlan.enabled {
		previousRuntime, previousStateful, getErr := s.runtimeForProviderModelRequest(ctx, sessionID, swapPlan.previousProvider, swapPlan.previousModel)
		if handleRuntimeErr(getErr) {
			return
		}
		// Durable message-backed response IDs were already validated against the
		// persisted session tail before we selected a runtime. Do not reject them
		// because an in-memory runtime still remembers an older ephemeral response
		// ID from before a restart/resume.
		if previousResponseID != "" {
			if !previousDurable {
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
		followUpOwner = modelSwapExec.previous
		followUpOwnerStateful = true
		if followUpOwner == nil {
			followUpOwner = previousRuntime
			followUpOwnerStateful = previousStateful
		}
	} else {
		var err error
		if previousResponseID != "" || rr.durableRuntime {
			runtime, stateful, err = s.runtimeForProviderRequest(ctx, sessionID, reqProvider)
		} else {
			runtime, stateful, err = s.runtimeForFreshProviderRequest(ctx, sessionID, freshProvider)
		}
		if handleRuntimeErr(err) {
			return
		}
		if freshConversation {
			providerForNormalization := reqProvider
			if providerForNormalization == "" {
				providerForNormalization = runtimeProviderKey(runtime)
			}
			req.Model, req.ReasoningEffort = normalizeProviderModelEffort(providerForNormalization, req.Model, req.ReasoningEffort)
		}

		// Enforce chaining from the latest in-memory response only for ephemeral
		// response IDs. Durable message-backed IDs are checked against the persisted
		// message tail in resolveDurablePreviousResponseID; the runtime can have an
		// older lastResponseID after a restart or history reload.
		if previousResponseID != "" {
			if !previousDurable {
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
			}
			s.populateResponsesToolResultNames(ctx, sessionID, runtime, inputMessages)
		}
		followUpOwner = runtime
		followUpOwnerStateful = stateful
	}

	effectiveProvider := strings.TrimSpace(reqProvider)
	if effectiveProvider == "" {
		effectiveProvider = runtimeProviderKey(runtime)
	}
	effectiveModel := strings.TrimSpace(req.Model)
	if effectiveModel == "" {
		effectiveModel = strings.TrimSpace(runtime.defaultModel)
	}
	modeRequestedExplicitly := requestedRuntime.reasoningMode != ""
	reasoningMode := ""
	if req.Reasoning != nil {
		reasoningMode = req.Reasoning.Mode
	}
	reasoningMode, clearStaleReasoningMode, reasoningModeErr := validateResponseReasoningMode(effectiveProvider, effectiveModel, reasoningMode, modeRequestedExplicitly)
	if reasoningModeErr != nil {
		if modelSwapExec != nil {
			modelSwapExec.markRolledBack()
		}
		if !stateful {
			s.unregisterResponseIDs(runtime)
			runtime.Close()
		}
		handleRuntimeErr(reasoningModeErr)
		return
	}
	if req.Reasoning != nil {
		req.Reasoning.Mode = reasoningMode
	}
	if swapPlan.enabled {
		swapPlan.requestedReasoningMode = reasoningMode
		modelSwapExec.plan.requestedReasoningMode = reasoningMode
	}
	if freshConversation {
		s.syncPersistedSessionRuntime(ctx, sessionID, runtime, req.Model, req.ReasoningEffort, reasoningMode, true, req.WorktreeDir, req.UseDefaultWorkspace)
	}
	if err := claimUIFollowUp(followUpOwner, followUpOwnerStateful); err != nil {
		if modelSwapExec != nil {
			modelSwapExec.markRolledBack()
		}
		if !stateful {
			s.unregisterResponseIDs(runtime)
			runtime.Close()
		}
		if errors.Is(err, errResponseClientMessageAlreadyCommitted) {
			writeOpenAIError(w, http.StatusConflict, "client_message_already_committed", err.Error())
		} else if errors.Is(err, errServeSessionBusy) {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", err.Error())
		} else {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		}
		return
	}
	claimsTransferred := false
	defer func() {
		if !claimsTransferred {
			followUpClaims.Release()
		}
	}()

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

	if isFirstPartyUIResponseRequest(r) {
		s.augmentMessagesWithMentions(ctx, runtime, sessionID, req.WorktreeDir, inputMessages)
	}

	// Idempotency is currently backed by response-run event replay, so only
	// streaming stateful requests register keys. Non-streaming responses are not
	// cached/replayed here.
	runIdempotencyKey := idempotencyKey
	if !stateful {
		runIdempotencyKey = ""
	}
	searchFromTools, ptcRequested, requestedTools, passthroughTools := parseRequestedTools(req.Tools)
	search := runtime.search || searchFromTools
	toolChoice := parseToolChoice(req.ToolChoice)
	includeServerTools := req.IncludeServerTools || isFirstPartyUIResponseRequest(r)
	serverTools := responseServerTools(runtime, requestedTools, includeServerTools)
	tools := appendResponsePassthroughTools(serverTools, passthroughTools, runtime.toolMap)
	if len(tools) == 0 {
		toolChoice = llm.ToolChoice{}
	}
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}

	reasoningEffort := normalizeReasoningEffort(req.ReasoningEffort)
	responsesOptions := &llm.ResponsesOptions{}
	if req.Reasoning != nil {
		if nestedEffort := normalizeReasoningEffort(req.Reasoning.Effort); nestedEffort != "" {
			reasoningEffort = nestedEffort
		}
		responsesOptions.ReasoningMode = req.Reasoning.Mode
		responsesOptions.ReasoningContext = req.Reasoning.Context
	}
	if req.MultiAgent != nil {
		responsesOptions.MultiAgent = llm.MultiAgentOptions{Enabled: req.MultiAgent.Enabled, EnabledSet: true, MaxConcurrentSubagents: req.MultiAgent.MaxConcurrentSubagents}
	}
	if req.PromptCacheOptions != nil {
		responsesOptions.PromptCache = llm.PromptCacheOptions{Mode: req.PromptCacheOptions.Mode, TTL: req.PromptCacheOptions.TTL}
	}
	if ptcRequested {
		responsesOptions.ProgrammaticToolCalling.Enabled = true
		responsesOptions.ProgrammaticToolCalling.EnabledSet = true
		for _, tool := range tools {
			for _, caller := range tool.AllowedCallers {
				if caller == "programmatic" {
					responsesOptions.ProgrammaticToolCalling.Tools = append(responsesOptions.ProgrammaticToolCalling.Tools, tool.Name)
					break
				}
			}
		}
	}
	if responsesOptions.IsZero() {
		responsesOptions = nil
	}
	if !freshConversation && req.Reasoning != nil {
		if clearStaleReasoningMode || ((reasoningMode == "standard" || reasoningMode == "pro") && llm.SupportsReasoningMode(effectiveProvider, effectiveModel)) {
			s.syncPersistedSessionReasoningMode(ctx, sessionID, runtime, reasoningMode)
		}
	}

	llmReq := llm.Request{
		SessionID:           sessionID,
		Model:               strings.TrimSpace(req.Model),
		ReasoningEffort:     reasoningEffort,
		Responses:           responsesOptions,
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
		var claimsDone func()
		if followUpClaims != nil {
			claimsDone = followUpClaims.Release
			claimsTransferred = true
		}
		if rr.uiStream && stateful {
			s.streamUIResponses(w, r, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, previousResponseID, resetResponseIDsOnSuccess, modelSwapExec, runIdempotencyKey, claimsDone)
		} else {
			started := s.streamResponses(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, previousResponseID, resetResponseIDsOnSuccess, modelSwapExec, runIdempotencyKey, claimsDone)
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
	missing := missingResponsesToolResultNames(messages)
	if len(missing) == 0 {
		return
	}

	names := make(map[string]string, len(missing))
	collectParts := func(parts []llm.Part) bool {
		for _, part := range parts {
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
		return len(names) == len(missing)
	}
	collectHistory := func(history []llm.Message) bool {
		for i := len(history) - 1; i >= 0; i-- {
			if collectParts(history[i].Parts) {
				return true
			}
		}
		return len(names) == len(missing)
	}

	if runtime != nil && collectHistory(runtime.snapshotHistory()) {
		applyResponsesToolResultNames(messages, names)
		return
	}
	if len(names) < len(missing) && s.store != nil && sessionID != "" {
		s.collectResponsesToolCallNamesFromStore(ctx, sessionID, collectParts)
	}
	applyResponsesToolResultNames(messages, names)
}

func missingResponsesToolResultNames(messages []llm.Message) map[string]bool {
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
	return missing
}

const responsesToolNameLookupPageSize = 128

func (s *serveServer) collectResponsesToolCallNamesFromStore(ctx context.Context, sessionID string, collectParts func([]llm.Part) bool) {
	if pager, ok := s.store.(session.MessagesDescendingPager); ok {
		beforeSeq := 0
		for {
			page, err := pager.GetMessagesPageDescending(ctx, sessionID, beforeSeq, responsesToolNameLookupPageSize)
			if err != nil || len(page) == 0 {
				return
			}
			for i := range page {
				if collectParts(page[i].Parts) {
					return
				}
			}
			if len(page) < responsesToolNameLookupPageSize {
				return
			}
			nextBeforeSeq := page[len(page)-1].Sequence
			if nextBeforeSeq <= 0 || (beforeSeq > 0 && nextBeforeSeq >= beforeSeq) {
				return
			}
			beforeSeq = nextBeforeSeq
		}
	}

	stored, err := s.store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return
	}
	for i := len(stored) - 1; i >= 0; i-- {
		if collectParts(stored[i].Parts) {
			return
		}
	}
}

func applyResponsesToolResultNames(messages []llm.Message, names map[string]string) {
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

func (s *serveServer) setReplaySessionNumberHeader(ctx context.Context, w http.ResponseWriter, sessionID string) {
	if s == nil || s.store == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	sess, err := s.store.Get(ctx, sessionID)
	if err != nil || sess == nil || sess.Number <= 0 {
		return
	}
	w.Header().Set("x-session-number", strconv.FormatInt(sess.Number, 10))
}

func responseIdempotencyKeyFromRequest(r *http.Request) string {
	if r == nil {
		return ""
	}
	// Prefer the standard header. X-Term-LLM-Request-ID is accepted for the
	// first-party UI/WebRTC fallback path and is only registered for streaming
	// stateful response runs (see runIdempotencyKey above).
	for _, name := range []string{"Idempotency-Key", "X-Idempotency-Key", "X-Term-LLM-Request-ID"} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func responseRunIdempotencyKey(r *http.Request, req responsesCreateRequest) string {
	key := responseIdempotencyKeyFromRequest(r)
	if req.Branch && strings.TrimSpace(req.IdempotencyKey) != "" {
		// The branch body key scopes both child materialization and replay of
		// that child's first run. Non-branch requests retain header semantics.
		key = strings.TrimSpace(req.IdempotencyKey)
	}
	return key
}

func branchUsesFirstPartyUIStream(r *http.Request, branched bool) bool {
	return branched && isFirstPartyUIResponseRequest(r)
}

func isFirstPartyUIResponseRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.TrimSpace(r.Header.Get("X-Term-LLM-UI-Version")) != ""
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

func (s *serveServer) streamResponses(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, previousResponseID string, resetResponseIDsOnSuccess bool, modelSwap *responseModelSwapExecution, idempotencyKey string, onDone func()) bool {
	return s.streamResponseRun(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID, startResponseRunOptions{
		previousResponseID:        previousResponseID,
		resetResponseIDsOnSuccess: resetResponseIDsOnSuccess,
		modelSwap:                 modelSwap,
		idempotencyKey:            idempotencyKey,
		onDone:                    onDone,
	})
}
