package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	response        string
	synthetic       bool
	usage           llm.Usage
	lastError       string
}

type sideQuestionView struct {
	Running    bool                 `json:"running"`
	Question   string               `json:"question,omitempty"`
	Response   string               `json:"response,omitempty"`
	Synthetic  bool                 `json:"synthetic,omitempty"`
	Usage      llm.Usage            `json:"usage"`
	Error      string               `json:"error,omitempty"`
	Generation uint64               `json:"generation"`
	History    []sidequestion.Entry `json:"history"`
}

func (rt *serveRuntime) sideQuestionContext() []llm.Message {
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
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.context = rt.sideQuestionContext()
	rt.sideQuestion.providerKey = rt.providerKey
	rt.sideQuestion.model = rt.defaultModel
	rt.sideQuestion.mu.Unlock()
}

func (rt *serveRuntime) updateSideQuestionConfig(req llm.Request) {
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = rt.defaultModel
	}
	effort := normalizeReasoningEffort(req.ReasoningEffort)
	model, effort = normalizeProviderModelEffort(rt.providerKey, model, effort)
	mode := ""
	if req.Responses != nil {
		mode = strings.TrimSpace(req.Responses.ReasoningMode)
	}
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.context = rt.sideQuestionContext()
	rt.sideQuestion.providerKey = rt.providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.reasoningEffort = effort
	rt.sideQuestion.reasoningMode = mode
	rt.sideQuestion.mu.Unlock()
}

func (rt *serveRuntime) initializeSideQuestionSnapshot(messages []llm.Message) {
	rt.sideQuestion.mu.Lock()
	defer rt.sideQuestion.mu.Unlock()
	if rt.sideQuestion.snapshotReady {
		return
	}
	rt.sideQuestion.mainSnapshot = sidequestion.PrepareContextSnapshot(messages)
	rt.sideQuestion.context = rt.sideQuestionContext()
	rt.sideQuestion.providerKey = rt.providerKey
	rt.sideQuestion.model = rt.defaultModel
	rt.sideQuestion.snapshotReady = true
}

func (rt *serveRuntime) refreshSideQuestionSnapshot(messages []llm.Message) {
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.mainSnapshot = sidequestion.PrepareContextSnapshot(messages)
	rt.sideQuestion.context = rt.sideQuestionContext()
	rt.sideQuestion.providerKey = rt.providerKey
	rt.sideQuestion.model = rt.defaultModel
	rt.sideQuestion.snapshotReady = true
	rt.sideQuestion.mu.Unlock()
}

func (sq *sideQuestionRuntime) view() sideQuestionView {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return sideQuestionView{
		Running: sq.running, Question: sq.question, Response: sq.response,
		Synthetic: sq.synthetic, Usage: sq.usage, Error: sq.lastError,
		Generation: sq.generation, History: append([]sidequestion.Entry(nil), sq.history...),
	}
}

const sideQuestionStopTimeout = 250 * time.Millisecond

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
	cancel, done := sq.cancel, sq.done
	sq.generation++
	sq.running = false
	sq.cancel = nil
	sq.response = ""
	sq.lastError = ""
	sq.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = waitForSideQuestion(done, sideQuestionStopTimeout)
}

func (sq *sideQuestionRuntime) clearHistory() {
	sq.mu.Lock()
	sq.history = nil
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
	sq.generation++
	generation := sq.generation
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	events := make(chan sideQuestionEventMsg, 64)
	snapshot := sidequestion.PrepareContextSnapshot(sq.mainSnapshot)
	snapshot = append(sidequestion.PrepareContextSnapshot(sq.context), snapshot...)
	history := append([]sidequestion.Entry(nil), sq.history...)
	sq.running = true
	sq.cancel = cancel
	sq.done = done
	sq.question = question
	sq.response = ""
	sq.synthetic = false
	sq.usage = llm.Usage{}
	sq.lastError = ""
	sq.mu.Unlock()

	req := llm.Request{
		Model: model, ReasoningEffort: reasoningEffort,
		Messages:  sidequestion.BuildMessages(snapshot, history, question),
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
					sq.response += event.Text
				case llm.EventAttemptDiscard:
					sq.response = ""
				}
			}
			sq.mu.Unlock()
			if len(events) < cap(events)-1 {
				events <- sideQuestionEventMsg{Generation: generation, Event: event}
			}
		})

		sq.mu.Lock()
		current := generation == sq.generation
		if current {
			sq.running = false
			sq.cancel = nil
			if errors.Is(runErr, context.Canceled) {
				sq.response = ""
			} else if runErr != nil {
				sq.lastError = runErr.Error()
			} else {
				sq.response = result.Response
				sq.synthetic = result.Synthetic
				sq.usage = result.Usage
				if !result.Synthetic && strings.TrimSpace(result.Response) != "" {
					sq.history = sidequestion.AppendHistory(sq.history, sidequestion.Entry{
						Question: question, Response: result.Response, CreatedAt: time.Now(), Usage: result.Usage,
					})
				}
			}
		}
		sq.mu.Unlock()
		if current {
			events <- sideQuestionEventMsg{Generation: generation, Result: &result, Err: runErr}
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
	storedMessages, err := s.store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load session history: %w", err)
	}
	history := make([]llm.Message, 0, len(storedMessages))
	for _, message := range storedMessages {
		history = append(history, message.ToLLMMessage())
	}
	rt.sideQuestion.mu.Lock()
	snapshotReady := rt.sideQuestion.snapshotReady
	rt.sideQuestion.mu.Unlock()
	rt.mu.Lock()
	rt.sessionMeta = meta
	if !snapshotReady {
		rt.history = copyLLMMessageSlice(history)
		rt.historyPersisted = true
	}
	rt.mu.Unlock()
	rt.initializeSideQuestionSnapshot(history)
	rt.updateSideQuestionConfig(llm.Request{
		Model: model, ReasoningEffort: effort,
		Responses: &llm.ResponsesOptions{ReasoningMode: mode},
	})
	return rt, nil
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
		rt, err := s.runtimeForSideQuestion(r.Context(), sessionID)
		if err != nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
			return
		}
		writeJSON(w, http.StatusOK, rt.sideQuestion.view())
		return
	}
	if len(parts) == 3 && r.Method == http.MethodDelete {
		rt, err := s.runtimeForSideQuestion(r.Context(), sessionID)
		if err != nil {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
			return
		}
		switch parts[2] {
		case "active":
			rt.sideQuestion.cancelActive()
		case "history":
			rt.sideQuestion.clearHistory()
		default:
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
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "already running") {
			status = http.StatusConflict
		}
		writeOpenAIError(w, status, "conflict_error", err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	for msg := range events {
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
			return
		}
		flusher.Flush()
	}
}
