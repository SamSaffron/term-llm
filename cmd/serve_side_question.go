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
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/runboundary"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/sidequestion"
)

type sideQuestionRuntime struct {
	mu         sync.Mutex
	running    bool
	generation uint64
	cancel     context.CancelFunc
	done       chan struct{}
	history    []sidequestion.Entry
	// anchor is the main boundary a side question branches from: its messages and
	// the provider state captured with them, as one value.
	anchor          sidequestion.Anchor
	lane            *sidequestion.Lane
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
	// New provider/model identity must never end up attached to a lane branched
	// under the old one. The anchor keeps the identity its state belongs to, so
	// the seam refuses on a mismatch, but the lane's own session is dropped here.
	identityChanged := rt.sideQuestion.providerKey != providerKey || rt.sideQuestion.model != model
	rt.sideQuestion.context = contextMessages
	rt.sideQuestion.providerKey = providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.reasoningEffort = effort
	rt.sideQuestion.reasoningMode = mode
	lane := rt.sideQuestion.lane
	if identityChanged {
		rt.sideQuestion.lane = nil
	}
	rt.sideQuestion.mu.Unlock()
	if identityChanged {
		lane.Close()
	}
}

// sideQuestionProviderIdentity is the part of the provider context that is
// always safe to publish: who the provider is and where its sessions live. It
// carries no transport state, so it can be republished while a stream is in
// flight.
//
// The working directory matters even on the replay path: Claude Code keys its
// sessions and project configuration by directory, so dropping it would run a
// side question somewhere else entirely.
func (rt *serveRuntime) sideQuestionProviderIdentity() runboundary.ProviderContext {
	identity := runboundary.ProviderContext{ProviderKey: rt.providerKey, Model: rt.defaultModel}
	if rt.toolMgr != nil {
		identity.WorkingDir = strings.TrimSpace(rt.toolMgr.BaseDir())
	}
	return identity
}

// sideQuestionReplayBoundary is a boundary that can only be replayed from: the
// messages and the provider identity they belong to, with no transport state.
// Transcript rewrites publish this, because the live session no longer matches
// what the transcript says — but the replay still has to run in the session's
// project directory.
func (rt *serveRuntime) sideQuestionReplayBoundary(messages []llm.Message) sidequestion.Anchor {
	return sidequestion.Anchor{
		Messages: append([]llm.Message(nil), messages...),
		Provider: rt.sideQuestionProviderIdentity(),
	}
}

// captureSideQuestionProvider adds the live provider's exported transport state
// to that identity. It is the web equivalent of the TUI's capture at the run
// boundary, and must only be called when the provider is not streaming.
func (rt *serveRuntime) captureSideQuestionProvider() runboundary.ProviderContext {
	captured := rt.sideQuestionProviderIdentity()
	if exporter, ok := rt.provider.(llm.ProviderStateExporter); ok {
		if state, exported := exporter.ExportProviderState(); exported {
			captured.State = state
		}
	}
	return captured
}

// sideQuestionBoundary builds the boundary a side question branches from:
// messages, the provider state captured with them, and the persisted row a
// promoted branch would be created at — one value, assembled at one instant.
// durableRowID is zero when no row identity is available, which is not the same
// as row zero.
func (rt *serveRuntime) sideQuestionBoundary(messages []llm.Message, durableRowID int64, durable bool) sidequestion.Anchor {
	return sidequestion.Anchor{
		Messages:        append([]llm.Message(nil), messages...),
		Provider:        rt.captureSideQuestionProvider(),
		DurableAnchorID: durableRowID,
		Durable:         durable && durableRowID > 0,
	}
}

func (rt *serveRuntime) initializeSideQuestionSnapshot(anchor sidequestion.Anchor) {
	contextMessages := rt.sideQuestionContextLocked()
	providerKey, model := rt.providerKey, rt.defaultModel
	rt.sideQuestion.mu.Lock()
	defer rt.sideQuestion.mu.Unlock()
	if rt.sideQuestion.snapshotReady {
		return
	}
	rt.sideQuestion.anchor = anchor
	rt.sideQuestion.context = contextMessages
	rt.sideQuestion.providerKey = providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.snapshotReady = true
}

// refreshSideQuestionSnapshot advances the side-question anchor. Messages,
// provider state and branch point move together, so state can never be refreshed
// without the messages it belongs to. Pass an anchor with no provider state when
// the runtime cannot vouch that the live provider session matches these
// messages; that only costs the branch path, never correctness.
func (rt *serveRuntime) refreshSideQuestionSnapshot(anchor sidequestion.Anchor) {
	contextMessages := rt.sideQuestionContextLocked()
	providerKey, model := rt.providerKey, rt.defaultModel
	rt.sideQuestion.mu.Lock()
	// Only the path that persisted a row knows a branch point. Everything else
	// advances messages and provider state and leaves the last known row alone,
	// the way the TUI's durable boundary only ever advances. Invalidation clears
	// it along with the rest of the anchor.
	if anchor.DurableAnchorID <= 0 {
		anchor.DurableAnchorID = rt.sideQuestion.anchor.DurableAnchorID
		anchor.Durable = rt.sideQuestion.anchor.Durable
	}
	rt.sideQuestion.anchor = anchor
	rt.sideQuestion.context = contextMessages
	rt.sideQuestion.providerKey = providerKey
	rt.sideQuestion.model = model
	rt.sideQuestion.snapshotReady = true
	rt.sideQuestion.mu.Unlock()
}

// advanceSideQuestionTranscript republishes the in-flight transcript so a
// question asked mid-run sees the work done so far.
//
// Turn completion is not frequent enough on its own: providers that run their
// whole tool loop inside one Stream complete a single engine turn per run, so a
// boundary that only advances there stays at the run's starting point for the
// run's entire duration.
//
// It publishes provider identity but no transport state: the provider is
// streaming, so its resume boundary must not be read here, and a mid-run
// question takes the bounded replay path anyway. Identity has to stay, because
// dropping the working directory would run the replay in the wrong project
// directory and make an established lane look like it had moved. The last known
// branch point is left alone; it still points at a persisted row.
func (rt *serveRuntime) advanceSideQuestionTranscript(messages []llm.Message) {
	snapshot := append([]llm.Message(nil), messages...)
	identity := rt.sideQuestionProviderIdentity()
	rt.sideQuestion.mu.Lock()
	rt.sideQuestion.anchor.Messages = snapshot
	rt.sideQuestion.anchor.Provider = identity
	rt.sideQuestion.snapshotReady = true
	rt.sideQuestion.mu.Unlock()
}

// invalidateSideQuestionSnapshot drops the anchor and the lane branched from it
// after an operation that rewrote or replaced the transcript. Dropping the state
// without the lane would leave a branch pointing at a transcript that no longer
// exists.
//
// A running turn is cancelled first: Close hands an in-flight turn's provider to
// that turn's own completion, so leaving it running would let the next question
// start a second turn against a session the old one is still streaming.
func (rt *serveRuntime) invalidateSideQuestionSnapshot() {
	rt.sideQuestion.cancelActive()
	identity := rt.sideQuestionProviderIdentity()
	rt.sideQuestion.mu.Lock()
	// Provider identity is not part of what a transcript rewrite invalidates, and
	// a replay asked before the next refresh still needs the project directory.
	rt.sideQuestion.anchor = sidequestion.Anchor{Provider: identity}
	rt.sideQuestion.snapshotReady = true
	lane := rt.sideQuestion.lane
	rt.sideQuestion.lane = nil
	rt.sideQuestion.mu.Unlock()
	lane.Close()
}

type sideQuestionStateBackup struct {
	history         []sidequestion.Entry
	anchor          sidequestion.Anchor
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

// backup round-trips the whole anchor rather than its messages alone, so a
// rollback cannot restore a snapshot without the provider state it belongs to.
// The lane is deliberately not part of it: a live provider branch is a lifecycle
// object, not restorable data, so a rollback drops it and the next question
// re-branches.
func (sq *sideQuestionRuntime) backup() sideQuestionStateBackup {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return sideQuestionStateBackup{
		history: append([]sidequestion.Entry(nil), sq.history...), anchor: sq.anchor.Clone(),
		snapshotReady: sq.snapshotReady, context: sidequestion.CloneMessages(sq.context), providerKey: sq.providerKey,
		model: sq.model, reasoningEffort: sq.reasoningEffort, reasoningMode: sq.reasoningMode,
		question: sq.question, response: string(sq.response), synthetic: sq.synthetic, usage: sq.usage, lastError: sq.lastError,
	}
}

func (sq *sideQuestionRuntime) restore(backup sideQuestionStateBackup) {
	sq.mu.Lock()
	sq.history = append([]sidequestion.Entry(nil), backup.history...)
	sq.anchor = backup.anchor.Clone()
	sq.snapshotReady = backup.snapshotReady
	sq.context = sidequestion.CloneMessages(backup.context)
	sq.providerKey, sq.model = backup.providerKey, backup.model
	sq.reasoningEffort, sq.reasoningMode = backup.reasoningEffort, backup.reasoningMode
	sq.question = backup.question
	sq.response = []byte(backup.response)
	sq.synthetic, sq.usage, sq.lastError = backup.synthetic, backup.usage, backup.lastError
	lane := sq.lane
	sq.lane = nil
	sq.mu.Unlock()
	lane.Close()
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
	lane := sq.lane
	sq.lane = nil
	sq.mu.Unlock()
	lane.Close()
}

func (sq *sideQuestionRuntime) close(ctx context.Context) {
	sq.mu.Lock()
	cancel, done := sq.cancel, sq.done
	sq.generation++
	sq.running = false
	sq.cancel = nil
	sq.history = nil
	sq.question = ""
	sq.response = nil
	sq.anchor = sidequestion.Anchor{}
	sq.snapshotReady = false
	sq.context = nil
	lane := sq.lane
	sq.lane = nil
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
	// Close the lane after the running turn has settled so its provider is not
	// released while a stream is still using it.
	lane.Close()
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
	workCtx, release, reloadErr := restart.Default.Root(context.Background())
	if reloadErr != nil {
		return nil, reloadErr
	}
	defer release()
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
	stateGeneration := sq.generation
	anchor := sq.anchor.Clone()
	anchor.Messages = appendMissingSideContext(anchor.Messages, sq.context)
	history := append([]sidequestion.Entry(nil), sq.history...)
	if sq.lane == nil {
		sq.lane = &sidequestion.Lane{}
	}
	lane := sq.lane
	sq.mu.Unlock()

	inputLimit := 0
	if rt.engine != nil {
		inputLimit = rt.engine.InputLimit()
	}
	turn, err := lane.PrepareTurn(sidequestion.TurnRequest{
		Question:        question,
		Anchor:          anchor,
		History:         history,
		ProviderKey:     providerKey,
		Model:           model,
		ReasoningEffort: reasoningEffort,
		ReasoningMode:   reasoningMode,
		InputLimit:      inputLimit,
		// Branching a Claude Code session the runtime still holds mid-turn has not
		// been proven safe, so lane creation waits for a settled boundary.
		AllowFork: !rt.hasActiveRun(),
		// Re-checked immediately before the branch streams: a request can arrive
		// and start a main run between preparing the branch and launching it.
		ForkStillAllowed: func() bool { return !rt.hasActiveRun() },
		ForkSource:       rt.provider,
		NewProvider:      rt.sideProviderFactory,
	})
	if err != nil {
		return nil, err
	}

	sq.mu.Lock()
	if sq.running || sq.generation != stateGeneration || sq.lane != lane {
		sq.mu.Unlock()
		turn.Abandon()
		return nil, errors.New("Side question state changed while preparing the request")
	}
	sq.generation++
	generation := sq.generation
	ctx, cancel := context.WithCancel(workCtx)
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

	_ = restart.Default.Go(ctx, func(ctx context.Context) {
		defer close(done)
		defer close(events)
		// The lane owns its provider across turns and releases it itself: after a
		// replay turn, after a failed lane turn, and on teardown.
		result, runErr := turn.Run(ctx, func(event llm.Event) {
			sq.mu.Lock()
			if generation == sq.generation {
				switch event.Type {
				case llm.EventTextDelta:
					sq.response = append(sq.response, event.Text...)
				case llm.EventAttemptDiscard:
					// Release an abandoned large attempt rather than retaining its
					// capacity for a potentially much smaller retry.
					sq.response = nil
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

		rt.recordHelperStats("side_question", model, result.Usage)
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
					// This is a replacement, not an append. Use an exact detached
					// buffer so a prior streamed response cannot pin excess capacity.
					sq.response = []byte(result.Response)
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
	})
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
	rt.initializeSideQuestionSnapshot(rt.sideQuestionBoundary(history, 0, false))
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
	sessionID, err := s.resolveSessionPathID(r.Context(), strings.TrimSpace(parts[0]))
	if err != nil || sessionID == "" || s.sessionMgr == nil {
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
	if _, ok := w.(http.Flusher); !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming unsupported")
		return
	}
	w = newStreamingResponseWriter(w, serveStreamWriteTimeout)
	flusher := w.(http.Flusher)
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
	releaseAdmission, admissionErr := s.sessionMgr.pinCurrentRuntime(sessionID, rt)
	if admissionErr != nil {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", admissionErr.Error())
		return
	}
	events, err := rt.startSideQuestion(input)
	releaseAdmission()
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
