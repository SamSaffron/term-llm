package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sidequestion"
)

type sideQuestionRuntime struct {
	mu              sync.Mutex
	running         bool
	generation      uint64
	cancel          context.CancelFunc
	done            chan struct{}
	history         []sidequestion.Entry
	mainSnapshot    []llm.Message
	snapshotReady   bool
	context         []llm.Message
	providerKey     string
	model           string
	reasoningEffort string
	reasoningMode   string
	question        string
	response        []byte
	synthetic       bool
	usage           llm.Usage
	totalUsage      llm.Usage
	requestCount    int
	lastError       string
}

type sideQuestionView struct {
	Running    bool                 `json:"running"`
	Question   string               `json:"question,omitempty"`
	Response   string               `json:"response,omitempty"`
	Synthetic  bool                 `json:"synthetic,omitempty"`
	Usage      llm.Usage            `json:"usage"`
	TotalUsage llm.Usage            `json:"total_usage"`
	Requests   int                  `json:"requests"`
	Error      string               `json:"error,omitempty"`
	Generation uint64               `json:"generation"`
	History    []sidequestion.Entry `json:"history"`
}

// sideQuestionContextLocked snapshots runtime-owned context. Callers must hold
// rt.mu after the runtime has been published; construction-time callers are safe
// before publication.
func (rt *serveRuntime) sideQuestionContextLocked() []llm.Message {
	contextMessages := make([]llm.Message, 0, 3)
	if systemPrompt := strings.TrimSpace(rt.systemPrompt); systemPrompt != "" {
		contextMessages = append(contextMessages, llm.Message{Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: systemPrompt}}})
	}
	if platformPrompt := strings.TrimSpace(rt.platformMessages.For(rt.platform)); platformPrompt != "" {
		contextMessages = append(contextMessages, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: platformPrompt}}})
	}
	if rt.sessionMeta != nil && strings.TrimSpace(rt.sessionMeta.CWD) != "" {
		contextMessages = append(contextMessages, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "Current working directory (context only; do not access it): " + strings.TrimSpace(rt.sessionMeta.CWD)}}})
	}
	return contextMessages
}

func (rt *serveRuntime) configureSideQuestionContext() {
	contextMessages := rt.sideQuestionContextLocked()
	providerKey, model := rt.providerKey, rt.defaultModel
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.context = contextMessages
	rt.sideQuestion.providerKey = providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.mu.Unlock()
}

func (rt *serveRuntime) updateSideQuestionConfig(req llm.Request) {
	contextMessages := rt.sideQuestionContextLocked()
	providerKey := rt.providerKey
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = rt.defaultModel
	}
	effort := normalizeReasoningEffort(req.ReasoningEffort)
	model, effort = normalizeProviderModelEffort(providerKey, model, effort)
	mode := ""
	if req.Responses != nil {
		mode = strings.TrimSpace(req.Responses.ReasoningMode)
	}
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.context = contextMessages
	rt.sideQuestion.providerKey = providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.reasoningEffort = effort
	rt.sideQuestion.reasoningMode = mode
	rt.sideQuestion.mu.Unlock()
}

func (rt *serveRuntime) initializeSideQuestionSnapshot(messages []llm.Message) {
	snapshot := append([]llm.Message(nil), messages...)
	contextMessages := rt.sideQuestionContextLocked()
	providerKey, model := rt.providerKey, rt.defaultModel
	rt.sideQuestion.mu.Lock()
	defer rt.sideQuestion.mu.Unlock()
	if rt.sideQuestion.snapshotReady {
		return
	}
	rt.sideQuestion.mainSnapshot = snapshot
	rt.sideQuestion.context = contextMessages
	rt.sideQuestion.providerKey = providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.snapshotReady = true
}

func (rt *serveRuntime) refreshSideQuestionSnapshot(messages []llm.Message) {
	snapshot := append([]llm.Message(nil), messages...)
	contextMessages := rt.sideQuestionContextLocked()
	providerKey, model := rt.providerKey, rt.defaultModel
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.mainSnapshot = snapshot
	rt.sideQuestion.context = contextMessages
	rt.sideQuestion.providerKey = providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.snapshotReady = true
	rt.sideQuestion.mu.Unlock()
}

type sideQuestionStateBackup struct {
	history         []sidequestion.Entry
	mainSnapshot    []llm.Message
	snapshotReady   bool
	context         []llm.Message
	providerKey     string
	model           string
	reasoningEffort string
	reasoningMode   string
	question        string
	response        string
	synthetic       bool
	usage           llm.Usage
	lastError       string
}

func (sq *sideQuestionRuntime) backup() sideQuestionStateBackup {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return sideQuestionStateBackup{
		history: append([]sidequestion.Entry(nil), sq.history...), mainSnapshot: sidequestion.CloneMessages(sq.mainSnapshot),
		snapshotReady: sq.snapshotReady, context: sidequestion.CloneMessages(sq.context), providerKey: sq.providerKey,
		model: sq.model, reasoningEffort: sq.reasoningEffort, reasoningMode: sq.reasoningMode,
		question: sq.question, response: string(sq.response), synthetic: sq.synthetic, usage: sq.usage, lastError: sq.lastError,
	}
}

func (sq *sideQuestionRuntime) restore(backup sideQuestionStateBackup) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	sq.history = append([]sidequestion.Entry(nil), backup.history...)
	sq.mainSnapshot = sidequestion.CloneMessages(backup.mainSnapshot)
	sq.snapshotReady = backup.snapshotReady
	sq.context = sidequestion.CloneMessages(backup.context)
	sq.providerKey, sq.model = backup.providerKey, backup.model
	sq.reasoningEffort, sq.reasoningMode = backup.reasoningEffort, backup.reasoningMode
	sq.question = backup.question
	sq.response = append(sq.response[:0], backup.response...)
	sq.synthetic, sq.usage, sq.lastError = backup.synthetic, backup.usage, backup.lastError
}

func (sq *sideQuestionRuntime) view() sideQuestionView {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return sideQuestionView{
		Running: sq.running, Question: sq.question, Response: string(sq.response),
		Synthetic: sq.synthetic, Usage: sq.usage, TotalUsage: sq.totalUsage, Requests: sq.requestCount, Error: sq.lastError,
		Generation: sq.generation, History: append([]sidequestion.Entry(nil), sq.history...),
	}
}

func waitForSideQuestion(done <-chan struct{}, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (sq *sideQuestionRuntime) cancelActive() {
	sq.mu.Lock()
	cancel := sq.cancel
	sq.generation++
	sq.running = false
	sq.cancel = nil
	sq.response = nil
	sq.lastError = ""
	sq.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (sq *sideQuestionRuntime) clearHistory() {
	sq.cancelActive()
	sq.mu.Lock()
	sq.history = nil
	sq.question = ""
	sq.response = nil
	sq.synthetic = false
	sq.usage = llm.Usage{}
	sq.lastError = ""
	sq.mu.Unlock()
}

func (sq *sideQuestionRuntime) close(ctx context.Context) {
	sq.mu.Lock()
	cancel, done := sq.cancel, sq.done
	sq.generation++
	sq.running = false
	sq.cancel = nil
	sq.history = nil
	sq.mainSnapshot = nil
	sq.snapshotReady = false
	sq.context = nil
	sq.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
}

type sideQuestionStart struct {
	Question string `json:"question"`
}

func appendMissingSideContext(snapshot, contextMessages []llm.Message) []llm.Message {
	canonicalPrefix := len(snapshot) >= len(contextMessages)
	for i, candidate := range contextMessages {
		if i >= len(snapshot) || !equivalentMessage(snapshot[i], candidate) {
			canonicalPrefix = false
			break
		}
	}
	if canonicalPrefix {
		return sidequestion.CloneMessages(snapshot)
	}

	// Runtime-owned system/developer context is a request prefix. If any piece is
	// missing, rebuild the whole prefix in canonical order and remove matching
	// copies from the transcript rather than appending privileged roles after it.
	messages := sidequestion.CloneMessages(contextMessages)
	for _, existing := range snapshot {
		duplicate := false
		for _, candidate := range contextMessages {
			if equivalentMessage(existing, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			messages = append(messages, sidequestion.CloneMessages([]llm.Message{existing})...)
		}
	}
	return messages
}

func equivalentMessage(a, b llm.Message) bool {
	return a.Role == b.Role && llm.MessageText(a) == llm.MessageText(b)
}

func (rt *serveRuntime) startSideQuestion(input sideQuestionStart) (<-chan sideQuestionEventMsg, error) {
	question := strings.TrimSpace(input.Question)
	if question == "" {
		return nil, errors.New("question is required")
	}
	if rt.sideProviderFactory == nil {
		return nil, errors.New("side questions are unavailable")
	}
	sq := &rt.sideQuestion
	sq.mu.Lock()
	if rt.compacting.Load() {
		sq.mu.Unlock()
		return nil, errors.New("Cannot ask a side question while conversation context is being compressed")
	}
	if sq.running {
		sq.mu.Unlock()
		return nil, errors.New("A side question is already running")
	}
	if sq.done != nil {
		select {
		case <-sq.done:
			sq.done = nil
		default:
			sq.mu.Unlock()
			return nil, errors.New("The previous side question is still stopping")
		}
	}
	providerKey := sq.providerKey
	model := sq.model
	reasoningEffort := sq.reasoningEffort
	reasoningMode := sq.reasoningMode
	sq.mu.Unlock()
	provider, err := rt.sideProviderFactory(providerKey, model)
	if err != nil {
		return nil, err
	}

	sq.mu.Lock()
	if sq.running {
		sq.mu.Unlock()
		if cleaner, ok := provider.(llm.ProviderCleaner); ok {
			cleaner.CleanupMCP()
		}
		return nil, errors.New("A side question is already running")
	}
	stateGeneration := sq.generation
	snapshot := appendMissingSideContext(sq.mainSnapshot, sq.context)
	history := append([]sidequestion.Entry(nil), sq.history...)
	sq.mu.Unlock()

	inputLimit := 0
	if rt.engine != nil {
		inputLimit = rt.engine.InputLimit()
	}
	messages, err := sidequestion.BuildMessages(snapshot, history, question, providerKey, model, inputLimit)
	if err != nil {
		if cleaner, ok := provider.(llm.ProviderCleaner); ok {
			cleaner.CleanupMCP()
		}
		return nil, err
	}

	sq.mu.Lock()
	if sq.running || sq.generation != stateGeneration {
		sq.mu.Unlock()
		if cleaner, ok := provider.(llm.ProviderCleaner); ok {
			cleaner.CleanupMCP()
		}
		return nil, errors.New("Side question state changed while preparing the request")
	}
	sq.generation++
	generation := sq.generation
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	events := make(chan sideQuestionEventMsg, 64)
	sq.running = true
	sq.cancel = cancel
	sq.done = done
	sq.question = question
	sq.response = nil
	sq.synthetic = false
	sq.usage = llm.Usage{}
	sq.lastError = ""
	sq.mu.Unlock()

	req := llm.Request{
		Model: model, ReasoningEffort: reasoningEffort,
		Messages:  messages,
		Responses: &llm.ResponsesOptions{ReasoningMode: reasoningMode},
	}
	go func() {
		defer close(done)
		defer close(events)
		defer func() {
			if cleaner, ok := provider.(llm.ProviderCleaner); ok {
				cleaner.CleanupMCP()
			}
		}()
		result, runErr := sidequestion.Run(ctx, provider, req, func(event llm.Event) {
			sq.mu.Lock()
			if generation == sq.generation {
				switch event.Type {
				case llm.EventTextDelta:
					sq.response = append(sq.response, event.Text...)
				case llm.EventAttemptDiscard:
					sq.response = sq.response[:0]
				}
			}
			sq.mu.Unlock()
			if len(events) < cap(events)-1 {
				select {
				case events <- sideQuestionEventMsg{Generation: generation, Event: event}:
				default:
				}
			}
		})

		sq.mu.Lock()
		sq.totalUsage.Add(result.Usage)
		sq.requestCount++
		current := generation == sq.generation
		if current {
			sq.running = false
			sq.cancel = nil
			sq.usage = result.Usage
			if errors.Is(runErr, context.Canceled) {
				sq.response = nil
			} else if runErr != nil {
				sq.lastError = runErr.Error()
			} else {
				sq.synthetic = result.Synthetic
				if !result.Synthetic && strings.TrimSpace(result.Response) != "" {
					sq.history = sidequestion.AppendHistory(sq.history, sidequestion.Entry{
						Question: question, Response: result.Response, CreatedAt: time.Now(), Usage: result.Usage,
					})
					sq.question = ""
					sq.response = nil
				} else {
					sq.response = append(sq.response[:0], result.Response...)
				}
			}
		}
		sq.mu.Unlock()
		if current {
			select {
			case events <- sideQuestionEventMsg{Generation: generation, Result: &result, Err: runErr}:
			default:
			}
		}
	}()
	return events, nil
}

type sideQuestionEventMsg struct {
	Generation uint64
	Event      llm.Event
	Result     *sidequestion.Result
	Err        error
}

func (s *serveServer) runtimeForSideQuestion(ctx context.Context, sessionID string) (*serveRuntime, error) {
	rt, inMemory := s.sessionMgr.Get(sessionID)
	if s.store == nil {
		if !inMemory {
			return nil, session.ErrNotFound
		}
		return rt, nil
	}
	meta, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, session.ErrNotFound
	}
	if inMemory {
		rt.sideQuestion.mu.Lock()
		ready := rt.sideQuestion.snapshotReady
		rt.sideQuestion.mu.Unlock()
		if ready {
			return rt, nil
		}
	}
	providerKey := strings.TrimSpace(meta.ProviderKey)
	if providerKey == "" {
		providerKey = resolveSessionProviderKey(s.cfgRef, meta)
	}
	model, effort := normalizeProviderModelEffort(providerKey, meta.Model, meta.ReasoningEffort)
	mode, _, err := validateResponseReasoningMode(providerKey, model, meta.ReasoningMode, strings.TrimSpace(meta.ReasoningMode) != "")
	if err != nil {
		return nil, err
	}
	if !inMemory {
		rt, _, err = s.runtimeForProviderModelRequest(ctx, sessionID, providerKey, model)
		if err != nil {
			return nil, err
		}
	}
	storedMessages, err := session.LoadActiveMessages(ctx, s.store, meta)
	if err != nil {
		return nil, fmt.Errorf("load active session history: %w", err)
	}
	history := make([]llm.Message, 0, len(storedMessages))
	for _, message := range storedMessages {
		history = append(history, message.ToLLMMessage())
	}
	rt.mu.Lock()
	rt.sessionMeta = meta
	rt.sideQuestion.mu.Lock()
	snapshotReady := rt.sideQuestion.snapshotReady
	rt.sideQuestion.mu.Unlock()
	if !snapshotReady {
		rt.history = copyLLMMessageSlice(history)
		rt.historyPersisted = true
		rt.restorePlatformInjectionStateFromHistory()
	}
	rt.initializeSideQuestionSnapshot(history)
	rt.updateSideQuestionConfig(llm.Request{
		Model: model, ReasoningEffort: effort,
		Responses: &llm.ResponsesOptions{ReasoningMode: mode},
	})
	rt.mu.Unlock()
	return rt, nil
}

func (s *serveServer) sideQuestionViewForSession(ctx context.Context, sessionID string) (sideQuestionView, error) {
	if rt, ok := s.sessionMgr.Get(sessionID); ok {
		return rt.sideQuestion.view(), nil
	}
	if s.store == nil {
		return sideQuestionView{}, session.ErrNotFound
	}
	meta, err := s.store.Get(ctx, sessionID)
	if err != nil {
		return sideQuestionView{}, err
	}
	if meta == nil {
		return sideQuestionView{}, session.ErrNotFound
	}
	return sideQuestionView{History: []sidequestion.Entry{}}, nil
}

func (s *serveServer) handleSideQuestion(w http.ResponseWriter, r *http.Request) {
	const marker = "/api/sessions/"
	path := strings.TrimPrefix(r.URL.Path, marker)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[1] != "side-question" {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "not found")
		return
	}
	sessionID := strings.TrimSpace(parts[0])
	if sessionID == "" || s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}

	if len(parts) == 2 && r.Method == http.MethodGet {
		view, err := s.sideQuestionViewForSession(r.Context(), sessionID)
		if err != nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
			return
		}
		writeJSON(w, http.StatusOK, view)
		return
	}
	if len(parts) == 3 && r.Method == http.MethodDelete {
		rt, inMemory := s.sessionMgr.Get(sessionID)
		if !inMemory {
			if _, err := s.sideQuestionViewForSession(r.Context(), sessionID); err != nil {
				writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
				return
			}
		} else {
			switch parts[2] {
			case "active":
				rt.sideQuestion.cancelActive()
			case "history":
				rt.sideQuestion.clearHistory()
			default:
				writeOpenAIError(w, http.StatusNotFound, "not_found_error", "not found")
				return
			}
		}
		if parts[2] != "active" && parts[2] != "history" {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	var input sideQuestionStart
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || strings.TrimSpace(input.Question) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "question is required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming unsupported")
		return
	}
	rt, err := s.runtimeForSideQuestion(r.Context(), sessionID)
	if err != nil {
		status := http.StatusNotFound
		errorType := "not_found_error"
		if !errors.Is(err, session.ErrNotFound) {
			status = http.StatusBadRequest
			errorType = "invalid_request_error"
		}
		writeOpenAIError(w, status, errorType, err.Error())
		return
	}
	events, err := rt.startSideQuestion(input)
	if err != nil {
		status, errorType := http.StatusInternalServerError, "server_error"
		message := strings.ToLower(err.Error())
		switch {
		case strings.Contains(message, "already running"), strings.Contains(message, "still stopping"), strings.Contains(message, "state changed"):
			status, errorType = http.StatusConflict, "conflict_error"
		case strings.Contains(message, "unavailable"):
			status = http.StatusServiceUnavailable
		}
		writeOpenAIError(w, status, errorType, err.Error())
		return
	}
	nextMessage := func() (sideQuestionEventMsg, bool) {
		select {
		case <-r.Context().Done():
			rt.sideQuestion.cancelActive()
			return sideQuestionEventMsg{}, false
		case msg, open := <-events:
			return msg, open
		}
	}
	msg, open := nextMessage()
	if !open {
		return
	}
	w.Header().Set("x-side-generation", strconv.FormatUint(msg.Generation, 10))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for {
		payload := map[string]any{"generation": msg.Generation}
		switch {
		case msg.Result != nil || msg.Err != nil:
			payload["type"] = "done"
			payload["result"] = msg.Result
			if msg.Err != nil {
				payload["error"] = msg.Err.Error()
			}
		default:
			payload["type"] = string(msg.Event.Type)
			payload["text"] = msg.Event.Text
			if msg.Event.Use != nil {
				payload["usage"] = msg.Event.Use
			}
		}
		data, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			rt.sideQuestion.cancelActive()
			return
		}
		flusher.Flush()
		msg, open = nextMessage()
		if !open {
			return
		}
	}
}
