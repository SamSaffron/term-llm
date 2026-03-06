package cmd

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/serveui"
	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *serveServer) handleUI(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ui {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	html := serveui.IndexHTML()
	if prefix := s.cfg.uiPrefix; prefix != "" && prefix != "/ui" {
		snippet := `<script>window.TERM_LLM_UI_PREFIX=` + "`" + prefix + "`" + `;</script></head>`
		html = bytes.Replace(html, []byte("</head>"), []byte(snippet), 1)
	}
	_, _ = w.Write(html)
}

func (s *serveServer) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	filename := strings.TrimPrefix(r.URL.Path, "/images/")
	if filename == "" || strings.Contains(filename, "/") || strings.Contains(filename, "..") {
		http.NotFound(w, r)
		return
	}

	outputDir := s.cfgRef.Image.OutputDir
	if outputDir == "" {
		outputDir = "~/Pictures/term-llm"
	}
	if strings.HasPrefix(outputDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "cannot resolve home directory")
			return
		}
		outputDir = filepath.Join(home, outputDir[2:])
	}

	filePath := filepath.Join(outputDir, filename)
	absDir, err := filepath.EvalSymlinks(outputDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	absFile, err := filepath.EvalSymlinks(filePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(absFile, absDir+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Add("Vary", "Authorization, Cookie")
	http.ServeFile(w, r, absFile)
}

func (s *serveServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	sessions, err := s.store.List(r.Context(), session.ListOptions{Limit: 100})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to list sessions")
		return
	}

	type sessionEntry struct {
		ID           string `json:"id"`
		Summary      string `json:"summary"`
		CreatedAt    int64  `json:"created_at"`
		MessageCount int    `json:"message_count"`
	}

	result := make([]sessionEntry, 0, len(sessions))
	for _, sess := range sessions {
		result = append(result, sessionEntry{
			ID:           sess.ID,
			Summary:      sess.Summary,
			CreatedAt:    sess.CreatedAt.UnixMilli(),
			MessageCount: sess.MessageCount,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"sessions": result})
}

func (s *serveServer) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	// Parse /v1/sessions/{id}/...
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 1 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	sessionID := parts[0]
	suffix := ""
	if len(parts) > 1 {
		suffix = parts[1]
	}

	if suffix == "interrupt" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return
		}
		s.handleSessionInterrupt(w, r, sessionID)
		return
	}

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	if suffix != "messages" {
		http.NotFound(w, r)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	msgs, err := s.store.GetMessages(r.Context(), sessionID, limit, offset)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get messages")
		return
	}

	type partEntry struct {
		Type       string `json:"type"`
		Text       string `json:"text,omitempty"`
		ToolName   string `json:"tool_name,omitempty"`
		ToolArgs   string `json:"tool_arguments,omitempty"`
		ToolCallID string `json:"tool_call_id,omitempty"`
	}

	type messageEntry struct {
		Role      string      `json:"role"`
		Parts     []partEntry `json:"parts"`
		CreatedAt int64       `json:"created_at"`
	}

	result := make([]messageEntry, 0, len(msgs))
	for _, msg := range msgs {
		entry := messageEntry{
			Role:      string(msg.Role),
			CreatedAt: msg.CreatedAt.UnixMilli(),
		}
		for _, p := range msg.Parts {
			switch p.Type {
			case llm.PartText:
				if p.Text != "" {
					entry.Parts = append(entry.Parts, partEntry{
						Type: "text",
						Text: p.Text,
					})
				}
			case llm.PartToolCall:
				if p.ToolCall != nil {
					pe := partEntry{
						Type:       "tool_call",
						ToolName:   p.ToolCall.Name,
						ToolCallID: p.ToolCall.ID,
					}
					if len(p.ToolCall.Arguments) > 0 {
						pe.ToolArgs = string(p.ToolCall.Arguments)
					}
					entry.Parts = append(entry.Parts, pe)
				}
			case llm.PartToolResult:
				// Omitted: UI ignores tool_result parts and they bloat payloads.
			}
		}
		if len(entry.Parts) == 0 {
			entry.Parts = []partEntry{}
		}
		result = append(result, entry)
	}

	writeJSON(w, http.StatusOK, map[string]any{"messages": result})
}

func (s *serveServer) handleSessionInterrupt(w http.ResponseWriter, r *http.Request, sessionID string) {
	var req sessionInterruptRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "message is required")
		return
	}

	rt, ok := s.sessionMgr.Get(sessionID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}

	fastProvider, fastErr := llm.NewFastProvider(s.cfgRef, rt.providerKey)
	if fastErr != nil {
		log.Printf("[serve] fast provider unavailable for interrupt: %v", fastErr)
	}
	action, interruptErr := rt.Interrupt(r.Context(), msg, fastProvider)
	if interruptErr != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", interruptErr.Error())
		return
	}

	actionName := "queue"
	switch action {
	case llm.InterruptCancel:
		actionName = "cancel"
	case llm.InterruptInterject:
		actionName = "interject"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"action": actionName,
	})
}

func (s *serveServer) auth(next http.HandlerFunc) http.HandlerFunc {
	if !s.cfg.requireAuth {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}

		var gotToken string
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(auth, prefix) {
			gotToken = strings.TrimSpace(strings.TrimPrefix(auth, prefix))
		} else if r.Method == http.MethodGet {
			// Cookie fallback only on safe GET requests (e.g. <img src> fetches
			// that cannot set Authorization headers).
			if cookie, err := r.Cookie("term_llm_token"); err == nil && cookie.Value != "" {
				if decoded, decErr := url.QueryUnescape(cookie.Value); decErr == nil {
					gotToken = decoded
				} else {
					gotToken = cookie.Value
				}
			}
		}

		if gotToken == "" || subtle.ConstantTimeCompare([]byte(gotToken), []byte(s.cfg.token)) != 1 {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid authentication credentials")
			return
		}
		next(w, r)
	}
}

func (s *serveServer) cors(next http.HandlerFunc) http.HandlerFunc {
	allowed := make(map[string]struct{}, len(s.cfg.corsOrigins))
	allowAll := false
	for _, origin := range s.cfg.corsOrigins {
		o := strings.TrimSpace(origin)
		if o == "" {
			continue
		}
		if o == "*" {
			allowAll = true
			continue
		}
		allowed[o] = struct{}{}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if _, ok := allowed[origin]; ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, session_id")
			w.Header().Set("Access-Control-Expose-Headers", "x-session-id")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

func (s *serveServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	provider, err := s.getModelsProvider()
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	models := make([]llm.ModelInfo, 0)
	if lister, ok := provider.(interface {
		ListModels(context.Context) ([]llm.ModelInfo, error)
	}); ok {
		if listed, err := lister.ListModels(ctx); err == nil {
			models = listed
		}
	}

	if len(models) == 0 {
		providerName := s.cfgRef.DefaultProvider
		if providerCfg := s.cfgRef.GetActiveProviderConfig(); providerCfg != nil {
			if providerCfg.Model != "" {
				models = append(models, llm.ModelInfo{ID: providerCfg.Model})
			}
		}
		if curated, ok := llm.ProviderModels[providerName]; ok {
			for _, id := range curated {
				models = append(models, llm.ModelInfo{ID: id})
			}
		}
	}

	seen := map[string]bool{}
	items := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		items = append(items, map[string]any{
			"id":      m.ID,
			"object":  "model",
			"created": m.Created,
			"owned_by": func() string {
				if m.OwnedBy != "" {
					return m.OwnedBy
				}
				return "term-llm"
			}(),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		idi, _ := items[i]["id"].(string)
		idj, _ := items[j]["id"].(string)
		return idi < idj
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   items,
	})
}

func (s *serveServer) getModelsProvider() (llm.Provider, error) {
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()

	if s.modelsProvider != nil {
		return s.modelsProvider, nil
	}
	provider, err := llm.NewProvider(s.cfgRef)
	if err != nil {
		return nil, err
	}
	s.modelsProvider = provider
	return provider, nil
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	var req responsesCreateRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

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
	runtime, stateful, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Enforce chaining from the latest response only. Stale response IDs that
	// map to a valid session but don't match the runtime's last response would
	// produce incorrect branching (the context wouldn't match what the client
	// expects from that response).
	if req.PreviousResponseID != "" {
		lastRespID := runtime.getLastResponseID()
		if lastRespID != "" && req.PreviousResponseID != lastRespID {
			writeOpenAIError(w, http.StatusConflict, "conflict_error",
				fmt.Sprintf("previous_response_id %q is stale; latest is %q", req.PreviousResponseID, lastRespID))
			if !stateful {
				runtime.Close()
			}
			return
		}
	}
	if !stateful {
		defer runtime.Close()
	}

	searchFromTools, requestedTools := parseRequestedTools(req.Tools)
	search := runtime.search || searchFromTools
	toolChoice := parseToolChoice(req.ToolChoice)
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}

	llmReq := llm.Request{
		SessionID:           sessionID,
		Model:               strings.TrimSpace(req.Model),
		Tools:               runtime.selectTools(requestedTools),
		ToolChoice:          toolChoice,
		ParallelToolCalls:   parallel,
		Search:              search,
		ForceExternalSearch: runtime.forceExternalSearch,
		MaxTurns:            runtime.maxTurns,
		Debug:               runtime.debug,
		DebugRaw:            runtime.debugRaw,
	}

	if req.MaxOutputTokens > 0 {
		llmReq.MaxOutputTokens = req.MaxOutputTokens
	}
	if req.Temperature != nil {
		llmReq.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		llmReq.TopP = *req.TopP
	}

	if req.Stream {
		s.streamResponses(ctx, w, runtime, stateful, replaceHistory, inputMessages, llmReq, sessionID)
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

	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}

	respID := "resp_" + randomSuffix()
	s.registerResponseID(runtime, respID, sessionID)

	writeJSON(w, http.StatusOK, responsesFinalResponse(result, model, respID))
}

// sseKeepalive starts a background goroutine that writes an SSE comment ping
// to w every interval while streaming is active. This prevents intermediate
// proxies (e.g. nginx with a short send_timeout) from closing the connection
// during silent periods — e.g. when the LLM is in extended thinking mode or
// the API is slow to emit tokens.
//
// The returned mu must wrap all writes to w inside the RunWithEvents callback.
// Call stop() immediately after RunWithEvents returns; it blocks until the
// goroutine has exited so subsequent final writes to w are safe without a lock.
func sseKeepalive(w http.ResponseWriter, flusher http.Flusher, interval time.Duration) (mu *sync.Mutex, stop func()) {
	mu = &sync.Mutex{}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mu.Lock()
				_, _ = io.WriteString(w, ": ping\n\n")
				flusher.Flush()
				mu.Unlock()
			case <-done:
				return
			}
		}
	}()
	return mu, func() {
		close(done)
		wg.Wait()
	}
}

func (s *serveServer) streamResponses(ctx context.Context, w http.ResponseWriter, runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	setSSEHeaders(w)
	respID := "resp_" + randomSuffix()
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	_ = writeSSEEvent(w, "response.created", map[string]any{
		"response": map[string]any{
			"id":      respID,
			"object":  "response",
			"created": created,
			"model":   model,
			"status":  "in_progress",
		},
	})
	flusher.Flush()

	pingMu, stopPing := sseKeepalive(w, flusher, 20*time.Second)

	outputIndex := 0
	toolsSeen := false
	result, err := runtime.RunWithEvents(ctx, stateful, replaceHistory, inputMessages, llmReq, func(ev llm.Event) error {
		pingMu.Lock()
		defer pingMu.Unlock()
		switch ev.Type {
		case llm.EventTextDelta:
			if toolsSeen {
				if err := writeSSEEvent(w, "response.output_text.new_segment", map[string]any{
					"output_index": outputIndex,
				}); err != nil {
					return err
				}
				toolsSeen = false
			}
			return writeSSEEvent(w, "response.output_text.delta", map[string]any{
				"output_index": outputIndex,
				"delta":        ev.Text,
			})
		case llm.EventToolCall:
			if ev.Tool == nil {
				return nil
			}
			toolsSeen = true
			item := map[string]any{
				"id":        "fc_" + ev.Tool.ID,
				"type":      "function_call",
				"call_id":   ev.Tool.ID,
				"name":      ev.Tool.Name,
				"arguments": string(ev.Tool.Arguments),
			}
			if err := writeSSEEvent(w, "response.output_item.added", map[string]any{"output_index": outputIndex, "item": item}); err != nil {
				return err
			}
			if err := writeSSEEvent(w, "response.function_call_arguments.delta", map[string]any{"output_index": outputIndex, "delta": string(ev.Tool.Arguments)}); err != nil {
				return err
			}
			if err := writeSSEEvent(w, "response.output_item.done", map[string]any{"output_index": outputIndex, "item": item}); err != nil {
				return err
			}
			outputIndex++
		case llm.EventToolExecStart:
			_ = writeSSEEvent(w, "response.tool_exec.start", map[string]any{
				"call_id":   ev.ToolCallID,
				"tool_name": ev.ToolName,
				"tool_info": ev.ToolInfo,
			})
		case llm.EventToolExecEnd:
			payload := map[string]any{
				"call_id":   ev.ToolCallID,
				"tool_name": ev.ToolName,
				"success":   ev.ToolSuccess,
			}
			if len(ev.ToolImages) > 0 {
				imageURLs := make([]string, 0, len(ev.ToolImages))
				for _, imgPath := range ev.ToolImages {
					imageURLs = append(imageURLs, "/images/"+filepath.Base(imgPath))
				}
				payload["images"] = imageURLs
			}
			_ = writeSSEEvent(w, "response.tool_exec.end", payload)
		case llm.EventHeartbeat:
			_ = writeSSEEvent(w, "response.heartbeat", map[string]any{
				"call_id":   ev.ToolCallID,
				"tool_name": ev.ToolName,
			})
		case llm.EventInterjection:
			_ = writeSSEEvent(w, "response.interjection", map[string]any{
				"text": ev.Text,
			})
		}
		flusher.Flush()
		return nil
	})
	stopPing() // wait for keepalive goroutine before any final writes

	if err != nil {
		errType := "invalid_request_error"
		if errors.Is(err, errServeSessionBusy) {
			errType = "conflict_error"
		}
		_ = writeSSEEvent(w, "response.failed", map[string]any{
			"error": map[string]any{"message": err.Error(), "type": errType},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	s.registerResponseID(runtime, respID, sessionID)

	_ = writeSSEEvent(w, "response.completed", map[string]any{
		"response": map[string]any{
			"id":      respID,
			"object":  "response",
			"created": created,
			"model":   model,
			"status":  "completed",
			"usage": map[string]any{
				"input_tokens":  result.Usage.InputTokens,
				"output_tokens": result.Usage.OutputTokens,
				"total_tokens":  result.Usage.InputTokens + result.Usage.OutputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": result.Usage.CachedInputTokens,
				},
			},
			"session_usage": map[string]any{
				"input_tokens":  result.SessionUsage.InputTokens,
				"output_tokens": result.SessionUsage.OutputTokens,
				"total_tokens":  result.SessionUsage.InputTokens + result.SessionUsage.OutputTokens,
				"input_tokens_details": map[string]any{
					"cached_tokens": result.SessionUsage.CachedInputTokens,
				},
			},
		},
	})
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	var req chatCompletionsRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
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
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if !stateful {
		defer runtime.Close()
	}

	search := runtime.search
	requestedTools := parseChatRequestedToolNames(req.Tools)
	toolChoice := parseToolChoice(req.ToolChoice)
	parallel := true
	if req.ParallelToolCalls != nil {
		parallel = *req.ParallelToolCalls
	}

	llmReq := llm.Request{
		SessionID:           sessionID,
		Model:               strings.TrimSpace(req.Model),
		Tools:               runtime.selectTools(requestedTools),
		ToolChoice:          toolChoice,
		ParallelToolCalls:   parallel,
		Search:              search,
		ForceExternalSearch: runtime.forceExternalSearch,
		MaxTurns:            runtime.maxTurns,
		Debug:               runtime.debug,
		DebugRaw:            runtime.debugRaw,
	}
	if req.MaxTokens > 0 {
		llmReq.MaxOutputTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		llmReq.Temperature = *req.Temperature
	}
	if req.TopP != nil {
		llmReq.TopP = *req.TopP
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
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
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
	respID := "chatcmpl_" + sessionOrRandomID(sessionID)
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	pingMu, stopPing := sseKeepalive(w, flusher, 20*time.Second)

	first := true
	toolCallSeen := false
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
			writeErr = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": 0,
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
		if errors.Is(err, errServeSessionBusy) {
			_ = writeChatStreamChunk(w, map[string]any{
				"id":      respID,
				"object":  "chat.completion.chunk",
				"created": created,
				"model":   model,
				"choices": []map[string]any{{"index": 0, "finish_reason": "error", "delta": map[string]any{}}},
				"error":   map[string]any{"message": err.Error(), "type": "conflict_error"},
			})
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		_ = writeChatStreamChunk(w, map[string]any{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "error", "delta": map[string]any{}}},
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
		_ = writeChatStreamChunk(w, map[string]any{
			"id":      respID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{},
			"usage": map[string]any{
				"prompt_tokens":     result.Usage.InputTokens,
				"completion_tokens": result.Usage.OutputTokens,
				"total_tokens":      result.Usage.InputTokens + result.Usage.OutputTokens,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": result.Usage.CachedInputTokens,
				},
			},
		})
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// registerResponseID stores a response ID on the runtime and server-wide map,
// pruning old IDs that exceed the per-session cap.
func (s *serveServer) registerResponseID(rt *serveRuntime, respID, sessionID string) {
	pruned := rt.addResponseID(respID)
	s.responseToSession.Store(respID, sessionID)
	for _, old := range pruned {
		s.responseToSession.Delete(old)
	}
}

func (s *serveServer) runtimeForRequest(ctx context.Context, sessionID string) (*serveRuntime, bool, error) {
	if sessionID == "" {
		// Ephemeral stateless runtime (fresh per request for isolation)
		rt, err := s.sessionMgr.factory(ctx)
		if err != nil {
			return nil, false, err
		}
		return rt, false, nil
	}
	// Stateful sessions should outlive a single HTTP request context.
	rt, err := s.sessionMgr.GetOrCreate(context.Background(), sessionID)
	if err != nil {
		return nil, false, err
	}
	return rt, true, nil
}
