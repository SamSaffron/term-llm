package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type serveRuntime struct {
	mu                   sync.Mutex
	goalMu               sync.Mutex
	interruptMu          sync.Mutex
	responseMu           sync.Mutex // guards lastResponseID and responseIDs
	askUserMu            sync.Mutex
	approvalMu           sync.Mutex
	uiStateMu            sync.Mutex
	provider             llm.Provider
	providerKey          string
	engine               *llm.Engine
	toolMgr              *tools.ToolManager
	mcpManager           *mcp.Manager
	store                session.Store
	goalStore            session.Store
	syntheticUserCB      func(context.Context, llm.Message) error
	systemPrompt         string
	history              []llm.Message
	historyPersisted     bool // history matches the persisted active transcript and can safely append next turn
	search               bool
	toolsSetting         string
	mcpSetting           string
	agentName            string
	sessionMeta          *session.Session
	forceExternalSearch  bool
	maxTurns             int
	toolMap              map[string]string
	debug                bool
	debugRaw             bool
	autoCompact          bool
	skipProviderCleanup  bool
	defaultModel         string
	yoloMode             bool
	lastUsedUnixNano     atomic.Int64
	activeInterrupt      *runtimeInterruptState
	interjectionCalls    map[string]*runtimeInterjectionCall
	lastResponseID       string
	responseIDs          []string
	cumulativeUsage      llm.Usage
	pendingAskUsers      map[string]*servePendingAskUser
	askUserFunc          func(context.Context, []tools.AskUserQuestion) ([]tools.AskUserAnswer, error)
	assistantSnapshotCB  llm.AssistantSnapshotCallback
	responseCompletedCB  llm.ResponseCompletedCallback
	turnCompletedCB      llm.TurnCompletedCallback
	compactionCB         llm.CompactionCallback
	pendingApprovals     map[string]*servePendingApproval
	approvalEventFunc    func(event string, data map[string]any) error
	approvalCtx          context.Context
	lastUIRunError       string
	platform             string
	platformMessages     agents.PlatformMessagesConfig
	lastInjectedPlatform string
}

type runtimeInterruptState struct {
	cancel          context.CancelFunc
	done            chan struct{}
	currentTask     string
	toolsRun        []string
	proseLen        int
	activeTool      string
	model           string
	reasoningEffort string
}

type runtimeInterjectionCall struct {
	done        chan struct{}
	fingerprint string
	action      llm.InterruptAction
	completedAt time.Time
}

const runtimeInterjectionCallTTL = time.Minute

func (rt *serveRuntime) emitGuardianReview(event tools.GuardianEvent) {
	message := strings.TrimSpace(event.Message)
	if message == "" {
		return
	}
	rt.approvalMu.Lock()
	eventFunc := rt.approvalEventFunc
	rt.approvalMu.Unlock()
	if eventFunc != nil {
		payload := map[string]any{
			"message":      message,
			"tool_call_id": event.ToolCallID,
			"outcome":      event.Outcome,
			"command":      event.Command,
			"workdir":      event.WorkDir,
		}
		if err := eventFunc("response.guardian.review", payload); err != nil {
			log.Printf("[serve] guardian review event failed: %v", err)
		}
		return
	}
	log.Printf("[serve] %s", message)
}

func (rt *serveRuntime) Touch() {
	rt.lastUsedUnixNano.Store(time.Now().UnixNano())
}

func (rt *serveRuntime) LastUsed() time.Time {
	unixNano := rt.lastUsedUnixNano.Load()
	if unixNano == 0 {
		return time.Time{}
	}
	return time.Unix(0, unixNano)
}

func (rt *serveRuntime) providerStateKey() string {
	if rt == nil {
		return ""
	}
	if key := strings.TrimSpace(rt.providerKey); key != "" {
		return key
	}
	if rt.provider == nil {
		return ""
	}
	if cred := strings.TrimSpace(rt.provider.Credential()); cred != "" {
		return cred
	}
	return strings.TrimSpace(rt.provider.Name())
}

func (rt *serveRuntime) restoreProviderState(ctx context.Context, sessionID string) {
	if rt == nil || rt.store == nil || rt.provider == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	importer, ok := rt.provider.(llm.ProviderStateImporter)
	if !ok {
		return
	}
	stateStore, ok := rt.store.(session.ProviderStateStore)
	if !ok {
		return
	}
	providerKey := rt.providerStateKey()
	if providerKey == "" {
		return
	}
	state, err := stateStore.LoadProviderState(ctx, sessionID, providerKey)
	if err != nil {
		log.Printf("[serve] load provider state failed for %s/%s: %v", sessionID, providerKey, err)
		return
	}
	if len(state) == 0 {
		return
	}
	if err := importer.ImportProviderState(state); err != nil {
		log.Printf("[serve] import provider state failed for %s/%s: %v", sessionID, providerKey, err)
	}
}

func (rt *serveRuntime) persistProviderState(ctx context.Context, sessionID string) {
	if rt == nil || rt.store == nil || rt.provider == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	stateStore, ok := rt.store.(session.ProviderStateStore)
	if !ok {
		return
	}
	providerKey := rt.providerStateKey()
	if providerKey == "" {
		return
	}
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	exporter, ok := rt.provider.(llm.ProviderStateExporter)
	if !ok {
		if err := stateStore.DeleteProviderState(dbCtx, sessionID, providerKey); err != nil {
			log.Printf("[serve] delete provider state failed for %s/%s: %v", sessionID, providerKey, err)
		}
		return
	}
	state, ok := exporter.ExportProviderState()
	if !ok || len(state) == 0 {
		if err := stateStore.DeleteProviderState(dbCtx, sessionID, providerKey); err != nil {
			log.Printf("[serve] delete provider state failed for %s/%s: %v", sessionID, providerKey, err)
		}
		return
	}
	if err := stateStore.SaveProviderState(dbCtx, sessionID, providerKey, state); err != nil {
		log.Printf("[serve] save provider state failed for %s/%s: %v", sessionID, providerKey, err)
	}
}

func (rt *serveRuntime) SessionNumber() int64 {
	if rt == nil {
		return 0
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.sessionMeta != nil {
		return rt.sessionMeta.Number
	}
	return 0
}

func (rt *serveRuntime) configureContextManagementForRequest(req llm.Request) {
	if rt.engine == nil || rt.provider == nil {
		return
	}

	providerForLimits := strings.TrimSpace(rt.providerKey)
	if providerForLimits == "" {
		providerForLimits = strings.TrimSpace(rt.provider.Name())
	}

	modelForLimits := strings.TrimSpace(req.Model)
	if modelForLimits == "" {
		modelForLimits = strings.TrimSpace(rt.defaultModel)
	}

	rt.engine.ConfigureContextManagement(rt.provider, providerForLimits, modelForLimits, rt.autoCompact)
}

func (rt *serveRuntime) Close() {
	rt.CloseContext(context.Background())
}

func (rt *serveRuntime) CloseContext(ctx context.Context) {
	rt.interruptMu.Lock()
	state := rt.activeInterrupt
	rt.interruptMu.Unlock()
	if state != nil && state.cancel != nil {
		state.cancel()
	}

	if !rt.lockForClose(ctx) {
		return
	}
	if ctx == nil || ctx.Done() == nil {
		defer rt.mu.Unlock()
		rt.closeLocked()
		return
	}

	// Cleanup hooks are third-party code and may block independently of the
	// active run. Keep ownership of rt.mu in the cleanup goroutine so a bounded
	// shutdown can return without permitting concurrent runtime reuse.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rt.mu.Unlock()
		rt.closeLocked()
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// lockForClose waits for the runtime mutex, but lets CloseContext abandon the
// wait when a provider/tool run keeps holding rt.mu after cancellation.
func (rt *serveRuntime) lockForClose(ctx context.Context) bool {
	if ctx == nil {
		rt.mu.Lock()
		return true
	}
	if rt.mu.TryLock() {
		return true
	}
	done := ctx.Done()
	if done == nil {
		rt.mu.Lock()
		return true
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return false
		case <-ticker.C:
			if rt.mu.TryLock() {
				return true
			}
		}
	}
}

func (rt *serveRuntime) closeLocked() {
	rt.clearPendingAskUsers()
	rt.clearPendingApprovals()
	if rt.mcpManager != nil {
		rt.mcpManager.StopAll()
		rt.mcpManager = nil
	}
	if !rt.skipProviderCleanup {
		if cleaner, ok := rt.provider.(interface{ CleanupMCP() }); ok {
			cleaner.CleanupMCP()
		}
	}
}

func (rt *serveRuntime) setActiveInterrupt(state *runtimeInterruptState) {
	rt.interruptMu.Lock()
	rt.activeInterrupt = state
	rt.interruptMu.Unlock()
}

func (rt *serveRuntime) clearActiveInterrupt(state *runtimeInterruptState) {
	rt.interruptMu.Lock()
	if rt.activeInterrupt == state {
		rt.activeInterrupt = nil
	}
	rt.interruptMu.Unlock()
}

func (rt *serveRuntime) updateInterruptFromEvent(ev llm.Event) {
	rt.interruptMu.Lock()
	defer rt.interruptMu.Unlock()
	if rt.activeInterrupt == nil {
		return
	}
	switch ev.Type {
	case llm.EventTextDelta:
		rt.activeInterrupt.proseLen += len(ev.Text)
	case llm.EventAttemptDiscard:
		rt.activeInterrupt.proseLen = 0
	case llm.EventToolExecStart:
		rt.activeInterrupt.activeTool = ev.ToolName
		if ev.ToolName != "" {
			rt.activeInterrupt.toolsRun = append(rt.activeInterrupt.toolsRun, ev.ToolName)
		}
	case llm.EventToolExecEnd:
		rt.activeInterrupt.activeTool = ""
	case llm.EventModelSwitch:
		model := strings.TrimSpace(ev.Model)
		if model == "" {
			model = strings.TrimSpace(ev.Text)
		}
		effort := strings.TrimSpace(ev.ReasoningEffort)
		model, effort = normalizeProviderModelEffort(runtimeProviderKey(rt), model, effort)
		if model != "" {
			rt.activeInterrupt.model = model
		}
		rt.activeInterrupt.reasoningEffort = effort
	}
}

func (rt *serveRuntime) Interrupt(ctx context.Context, msg string, fastProvider llm.Provider) (llm.InterruptAction, error) {
	action, _, err := rt.InterruptMessage(ctx, llm.UserText(msg), msg, "", fastProvider, false)
	return action, err
}

func (rt *serveRuntime) QueueActiveRunRuntimeSwitch(model, reasoningEffort string) error {
	if rt == nil || rt.engine == nil {
		return fmt.Errorf("session has no active stream")
	}
	model = strings.TrimSpace(model)
	reasoningEffort = strings.TrimSpace(reasoningEffort)
	rt.interruptMu.Lock()
	defer rt.interruptMu.Unlock()
	if rt.activeInterrupt == nil {
		return fmt.Errorf("session has no active stream")
	}
	activeModel := strings.TrimSpace(rt.activeInterrupt.model)
	if activeModel == "" {
		activeModel = strings.TrimSpace(rt.defaultModel)
	}
	if model == "" {
		model = activeModel
	}
	if activeModel != "" && model != activeModel {
		return fmt.Errorf("runtime effort switch can only target active model %q", activeModel)
	}
	rt.engine.QueueRequestRuntimeSwitch(model, reasoningEffort)
	return nil
}

func interjectionFingerprint(msg llm.Message, displayText string, autoContinue bool) (string, error) {
	parts := append([]llm.Part(nil), msg.Parts...)
	for i := range parts {
		// Parsing inline attachments can materialize them at a fresh temporary path
		// on each transport retry. The content fields are the stable identity.
		parts[i].ImagePath = ""
		parts[i].FilePath = ""
	}
	payload, err := json.Marshal(struct {
		Parts        []llm.Part `json:"parts"`
		DisplayText  string     `json:"display_text"`
		AutoContinue bool       `json:"auto_continue"`
	}{parts, displayText, autoContinue})
	if err != nil {
		return "", fmt.Errorf("encode interjection idempotency payload: %w", err)
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%x", sum), nil
}

func (rt *serveRuntime) InterruptMessage(ctx context.Context, msg llm.Message, displayText string, interjectionID string, fastProvider llm.Provider, autoContinue bool) (llm.InterruptAction, bool, error) {
	interjectionID = strings.TrimSpace(interjectionID)
	fingerprint := ""
	if interjectionID != "" {
		var err error
		fingerprint, err = interjectionFingerprint(msg, displayText, autoContinue)
		if err != nil {
			return llm.InterruptInterject, false, err
		}
	}

	rt.interruptMu.Lock()
	now := time.Now()
	for id, existing := range rt.interjectionCalls {
		if !existing.completedAt.IsZero() && now.Sub(existing.completedAt) > runtimeInterjectionCallTTL {
			delete(rt.interjectionCalls, id)
		}
	}
	var call *runtimeInterjectionCall
	if interjectionID != "" {
		if rt.interjectionCalls == nil {
			rt.interjectionCalls = make(map[string]*runtimeInterjectionCall)
		}
		if existing := rt.interjectionCalls[interjectionID]; existing != nil {
			if existing.fingerprint != fingerprint {
				rt.interruptMu.Unlock()
				return llm.InterruptInterject, false, fmt.Errorf("interjection id %q was already used for different content", interjectionID)
			}
			rt.interruptMu.Unlock()
			select {
			case <-existing.done:
				return existing.action, true, nil
			case <-ctx.Done():
				return llm.InterruptInterject, true, ctx.Err()
			}
		}
		call = &runtimeInterjectionCall{done: make(chan struct{}), fingerprint: fingerprint}
		rt.interjectionCalls[interjectionID] = call
	}
	state := rt.activeInterrupt
	if state == nil {
		if call != nil {
			delete(rt.interjectionCalls, interjectionID)
		}
		rt.interruptMu.Unlock()
		return llm.InterruptInterject, false, fmt.Errorf("session has no active stream")
	}
	cancel := state.cancel
	activity := llm.InterruptActivity{
		CurrentTask: state.currentTask,
		ToolsRun:    append([]string(nil), state.toolsRun...),
		ProseLen:    state.proseLen,
		ActiveTool:  state.activeTool,
	}
	rt.interruptMu.Unlock()
	classifyText := strings.TrimSpace(displayText)
	if classifyText == "" {
		classifyText = strings.TrimSpace(llm.MessageText(msg))
	}
	if summary := llm.MessageAttachmentSummary(msg); summary != "" {
		if classifyText != "" {
			classifyText += " "
		}
		classifyText += summary
	}
	classifyCtx := ctx
	classifyCancel := func() {}
	if interjectionID != "" {
		// Stay below the web client's 5-second mutation first-frame timeout so a
		// healthy classifier normally responds before transport fallback begins.
		classifyCtx, classifyCancel = context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
	}
	action := llm.ClassifyInterrupt(classifyCtx, fastProvider, classifyText, activity)
	classifyCancel()
	switch action {
	case llm.InterruptCancel:
		if cancel != nil {
			cancel()
		}
	case llm.InterruptInterject:
		rt.engine.QueueInterjection(llm.QueuedInterjection{ID: interjectionID, Message: msg, DisplayText: displayText, AutoContinue: autoContinue})
	}
	if call != nil {
		rt.interruptMu.Lock()
		call.action = action
		call.completedAt = time.Now()
		close(call.done)
		rt.interruptMu.Unlock()
	}
	return action, false, nil
}

// ensureSessionInStore creates the session record in the database if it doesn't
// exist yet and returns the assigned session number. Unlike ensurePersistedSession,
// this does NOT mutate runtime state (sessionMeta, history), so it is safe to call
// without holding rt.mu.
func (rt *serveRuntime) ensureSessionInStore(ctx context.Context, sessionID string, inputMessages []llm.Message) int64 {
	if rt.store == nil || sessionID == "" {
		return 0
	}
	// Fast path: runtime already hydrated under rt.mu by a prior run.
	rt.mu.Lock()
	if meta := rt.sessionMeta; meta != nil && meta.ID == sessionID {
		number := meta.Number
		rt.mu.Unlock()
		return number
	}
	rt.mu.Unlock()
	// Check DB for existing session.
	if existing, err := rt.store.Get(ctx, sessionID); err == nil && existing != nil {
		return existing.Number
	}
	// Build and insert a new session record.
	providerName := "unknown"
	if rt.provider != nil {
		if name := strings.TrimSpace(rt.provider.Name()); name != "" {
			providerName = name
		}
	}
	modelName := strings.TrimSpace(rt.defaultModel)
	if modelName == "" {
		modelName = "unknown"
	}
	sess := &session.Session{
		ID:          sessionID,
		Provider:    providerName,
		ProviderKey: strings.TrimSpace(rt.providerKey),
		Model:       modelName,
		Mode:        session.ModeChat,
		Origin:      session.OriginWeb,
		Agent:       rt.agentName,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Search:      rt.search,
		Tools:       rt.toolsSetting,
		MCP:         rt.mcpSetting,
		Status:      session.StatusActive,
	}
	if cwd, err := os.Getwd(); err == nil {
		sess.CWD = cwd
	}
	for _, msg := range inputMessages {
		if msg.Role != llm.RoleUser {
			continue
		}
		if text := session.NewMessage(sessionID, msg, -1).TextContent; text != "" {
			sess.Summary = session.TruncateSummary(text)
			break
		}
	}
	if err := rt.store.Create(ctx, sess); err != nil {
		if existing, getErr := rt.store.Get(ctx, sessionID); getErr == nil && existing != nil {
			return existing.Number
		}
		log.Printf("[serve] session Create failed for %s: %v", sessionID, err)
		return 0
	}
	return sess.Number
}

func (rt *serveRuntime) restorePersistedHistory(ctx context.Context, sess *session.Session) bool {
	if sess == nil || len(rt.history) > 0 || rt.historyPersisted {
		return true
	}
	msgs, err := session.LoadActiveMessages(ctx, rt.store, sess)
	if err != nil {
		log.Printf("[serve] session history restore failed for %s: %v", sess.ID, err)
		return false
	}
	llmMsgs := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		llmMsgs = append(llmMsgs, m.ToLLMMessage())
	}
	rt.history = llmMsgs
	rt.historyPersisted = true
	return true
}

func (rt *serveRuntime) ensurePersistedSession(ctx context.Context, sessionID string, inputMessages []llm.Message) bool {
	if rt.store == nil || sessionID == "" {
		return false
	}
	if rt.sessionMeta != nil && rt.sessionMeta.ID == sessionID {
		// Metadata-only setup (for example restoring a worktree BaseDir or
		// updating reasoning settings) can populate sessionMeta before the first
		// post-restart run. Never treat metadata as proof that the transcript is
		// hydrated: persisting an empty runtime snapshot would truncate the stored
		// conversation to the new input.
		if !rt.restorePersistedHistory(ctx, rt.sessionMeta) {
			return false
		}
		rt.restorePlatformInjectionStateFromHistory()
		return true
	}

	// Check DB first — ensureSessionInStore may have already created the record.
	if existing, err := rt.store.Get(ctx, sessionID); err == nil && existing != nil {
		rt.sessionMeta = existing
		if !rt.restorePersistedHistory(ctx, existing) {
			return false
		}
		rt.restorePlatformInjectionStateFromHistory()
		return true
	}

	providerName := "unknown"
	if rt.provider != nil {
		if name := strings.TrimSpace(rt.provider.Name()); name != "" {
			providerName = name
		}
	}
	modelName := strings.TrimSpace(rt.defaultModel)
	if modelName == "" {
		modelName = "unknown"
	}

	sess := &session.Session{
		ID:          sessionID,
		Provider:    providerName,
		ProviderKey: strings.TrimSpace(rt.providerKey),
		Model:       modelName,
		Mode:        session.ModeChat,
		Origin:      session.OriginWeb,
		Agent:       rt.agentName,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
		Search:      rt.search,
		Tools:       rt.toolsSetting,
		MCP:         rt.mcpSetting,
		Status:      session.StatusActive,
	}
	if cwd, err := os.Getwd(); err == nil {
		sess.CWD = cwd
	}
	for _, msg := range inputMessages {
		if msg.Role != llm.RoleUser {
			continue
		}
		if text := session.NewMessage(sessionID, msg, -1).TextContent; text != "" {
			sess.Summary = session.TruncateSummary(text)
			break
		}
	}

	if err := rt.store.Create(ctx, sess); err != nil {
		existing, getErr := rt.store.Get(ctx, sessionID)
		if getErr != nil || existing == nil {
			log.Printf("[serve] session Create failed for %s: %v", sessionID, err)
			return false
		}
		rt.sessionMeta = existing
		if !rt.restorePersistedHistory(ctx, existing) {
			return false
		}
		rt.restorePlatformInjectionStateFromHistory()
		if setErr := rt.store.SetCurrent(ctx, sessionID); setErr != nil {
			log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
		}
		return true
	}

	rt.sessionMeta = sess
	if setErr := rt.store.SetCurrent(ctx, sessionID); setErr != nil {
		log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
	}
	return true
}

func inlinePersistContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, timeout)
}

func (rt *serveRuntime) persistSnapshot(ctx context.Context, sessionID string, snapshot []llm.Message) bool {
	if rt.store == nil || sessionID == "" {
		return false
	}
	dbCtx, cancel := inlinePersistContext(ctx, 10*time.Second)
	defer cancel()

	messages := make([]session.Message, 0, len(snapshot))
	for _, msg := range snapshot {
		if msg.Role == "" {
			continue
		}
		sessionMsg := session.NewMessage(sessionID, msg, -1)
		messages = append(messages, *sessionMsg)
	}
	if err := rt.store.ReplaceMessages(dbCtx, sessionID, messages); err != nil {
		log.Printf("[serve] session ReplaceMessages failed for %s: %v", sessionID, err)
		return false
	}
	userTurns := countUserMessages(snapshot)
	if rt.sessionMeta != nil {
		updated := *rt.sessionMeta
		needsUpdate := false
		if updated.UserTurns != userTurns {
			updated.UserTurns = userTurns
			needsUpdate = true
		}
		if updated.Summary == "" {
			for _, msg := range snapshot {
				if msg.Role != llm.RoleUser {
					continue
				}
				if text := session.NewMessage(sessionID, msg, -1).TextContent; text != "" {
					updated.Summary = session.TruncateSummary(text)
					needsUpdate = true
					break
				}
			}
		}
		if needsUpdate {
			if goalStore := rt.goalStateStore(); goalStore != nil && strings.TrimSpace(sessionID) != "" {
				if refreshed, refreshErr := goalStore.Get(dbCtx, sessionID); refreshErr == nil && refreshed != nil {
					updated.Goal = refreshed.Goal.Clone()
				}
			}
			if updateErr := rt.store.Update(dbCtx, &updated); updateErr != nil {
				log.Printf("[serve] session Update failed for %s: %v", sessionID, updateErr)
			} else {
				*rt.sessionMeta = updated
			}
		}
	}
	if setErr := rt.store.SetCurrent(dbCtx, sessionID); setErr != nil {
		log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
	}
	if statusErr := rt.store.UpdateStatus(dbCtx, sessionID, session.StatusActive); statusErr != nil {
		log.Printf("[serve] session UpdateStatus(active) failed for %s: %v", sessionID, statusErr)
	}
	return true
}

type compactedMessageReplacer interface {
	ReplaceCompactedMessages(ctx context.Context, sessionID string, messages []session.Message) error
}

func (rt *serveRuntime) persistCompactedSnapshot(ctx context.Context, sessionID string, snapshot []llm.Message) bool {
	if rt.store == nil || sessionID == "" {
		return false
	}
	replacer, ok := rt.store.(compactedMessageReplacer)
	if !ok {
		log.Printf("[serve] session ReplaceCompactedMessages unsupported for %s", sessionID)
		return false
	}
	dbCtx, cancel := inlinePersistContext(ctx, 10*time.Second)
	defer cancel()

	messages := make([]session.Message, 0, len(snapshot))
	for _, msg := range snapshot {
		if msg.Role == "" {
			continue
		}
		sessionMsg := session.NewMessage(sessionID, msg, -1)
		messages = append(messages, *sessionMsg)
	}
	if err := replacer.ReplaceCompactedMessages(dbCtx, sessionID, messages); err != nil {
		log.Printf("[serve] session ReplaceCompactedMessages failed for %s: %v", sessionID, err)
		return false
	}
	if rt.sessionMeta != nil {
		if refreshed, err := rt.store.Get(dbCtx, sessionID); err == nil && refreshed != nil {
			rt.sessionMeta = refreshed
		}
	}
	if setErr := rt.store.SetCurrent(dbCtx, sessionID); setErr != nil {
		log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
	}
	if statusErr := rt.store.UpdateStatus(dbCtx, sessionID, session.StatusActive); statusErr != nil {
		log.Printf("[serve] session UpdateStatus(active) failed for %s: %v", sessionID, statusErr)
	}
	return true
}

// appendMessages incrementally adds new messages to the DB using AddMessage.
// Unlike persistSnapshot (which does a full DELETE+INSERT replace), this only
// appends new messages, making each callback commit a small atomic write that
// survives kill -9. Returns the number of messages successfully written so
// callers can track progress accurately.
func (rt *serveRuntime) appendMessages(ctx context.Context, sessionID string, messages []llm.Message, turnIndex int) int {
	if rt.store == nil || sessionID == "" || len(messages) == 0 {
		return 0
	}
	dbCtx, cancel := inlinePersistContext(ctx, 10*time.Second)
	defer cancel()
	written := 0
	for _, msg := range messages {
		if msg.Role == "" {
			written++ // skip but count as consumed
			continue
		}
		sessionMsg := session.NewMessage(sessionID, msg, -1)
		sessionMsg.TurnIndex = turnIndex
		if err := rt.store.AddMessage(dbCtx, sessionID, sessionMsg); err != nil {
			log.Printf("[serve] session AddMessage failed for %s: %v", sessionID, err)
			return written
		}
		if msg.Role == llm.RoleUser {
			if err := rt.store.IncrementUserTurns(dbCtx, sessionID); err != nil {
				log.Printf("[serve] session IncrementUserTurns failed for %s: %v", sessionID, err)
			} else if rt.sessionMeta != nil {
				rt.sessionMeta.UserTurns++
			}
		}
		written++
	}
	return written
}

func (rt *serveRuntime) persistTurnAccounting(ctx context.Context, persisted bool, sessionID string, messages []llm.Message, metrics llm.TurnMetrics) {
	if !persisted || rt.store == nil || rt.sessionMeta == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	if !isModelTurnCompletion(messages, metrics) {
		return
	}
	if err := rt.store.UpdateMetrics(ctx, sessionID, 1, metrics.ToolCalls, metrics.InputTokens, metrics.OutputTokens, metrics.CachedInputTokens, metrics.CacheWriteTokens); err != nil {
		log.Printf("[serve] session UpdateMetrics failed for %s: %v", sessionID, err)
	} else {
		rt.sessionMeta.LLMTurns++
		rt.sessionMeta.ToolCalls += metrics.ToolCalls
		rt.sessionMeta.InputTokens += metrics.InputTokens
		rt.sessionMeta.OutputTokens += metrics.OutputTokens
		rt.sessionMeta.CachedInputTokens += metrics.CachedInputTokens
		rt.sessionMeta.CacheWriteTokens += metrics.CacheWriteTokens
	}
	if rt.engine == nil {
		return
	}
	total, count := rt.engine.ContextEstimateBaseline()
	if total <= 0 {
		return
	}
	if err := rt.store.UpdateContextEstimate(ctx, sessionID, total, count); err != nil {
		log.Printf("[serve] session UpdateContextEstimate failed for %s: %v", sessionID, err)
		return
	}
	rt.sessionMeta.LastTotalTokens = total
	rt.sessionMeta.LastMessageCount = count
}

func isModelTurnCompletion(messages []llm.Message, metrics llm.TurnMetrics) bool {
	if metrics.InputTokens != 0 || metrics.OutputTokens != 0 || metrics.CachedInputTokens != 0 || metrics.CacheWriteTokens != 0 || metrics.ToolCalls != 0 {
		return true
	}
	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleAssistant, llm.RoleTool:
			return true
		}
	}
	return false
}

func (rt *serveRuntime) persistStatus(ctx context.Context, sessionID string, status session.SessionStatus) {
	if rt.store == nil || sessionID == "" {
		return
	}
	// Use a cancel-proof context for final status writes so they succeed even
	// when the run context is cancelled (e.g. ^C or client disconnect), while
	// preserving any context values (tracing, logging).
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := rt.store.UpdateStatus(dbCtx, sessionID, status); err != nil {
		log.Printf("[serve] session UpdateStatus(%s) failed for %s: %v", status, sessionID, err)
	}
}

// maxResponseIDs is the maximum number of response IDs tracked per session.
// Only the latest is needed for chaining validation; a small buffer guards
// against in-flight races. Older IDs are pruned from the server-wide map.
const maxResponseIDs = 16

func (rt *serveRuntime) selectTools(requested map[string]bool) []llm.ToolSpec {
	all := rt.engine.Tools().AllSpecs()
	if len(requested) == 0 {
		return all
	}
	// Resolve client tool names through toolMap so that a request for
	// "WebSearch" with toolMap["WebSearch"]="web_search" matches the
	// server tool "web_search".
	resolved := make(map[string]bool, len(requested))
	for name := range requested {
		if rt.toolMap != nil {
			if mapped, ok := rt.toolMap[name]; ok {
				resolved[mapped] = true
				continue
			}
		}
		resolved[name] = true
	}
	out := make([]llm.ToolSpec, 0, len(all))
	for _, spec := range all {
		if resolved[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}

// getLastResponseID returns the last response ID for chaining validation.
func (rt *serveRuntime) getLastResponseID() string {
	rt.responseMu.Lock()
	defer rt.responseMu.Unlock()
	return rt.lastResponseID
}

// addResponseID records a new response ID and returns any pruned IDs that
// should be removed from the server-wide map.
func (rt *serveRuntime) addResponseID(respID string) []string {
	rt.responseMu.Lock()
	defer rt.responseMu.Unlock()
	rt.lastResponseID = respID
	rt.responseIDs = append(rt.responseIDs, respID)
	if len(rt.responseIDs) <= maxResponseIDs {
		return nil
	}
	excess := len(rt.responseIDs) - maxResponseIDs
	pruned := make([]string, excess)
	copy(pruned, rt.responseIDs[:excess])
	rt.responseIDs = rt.responseIDs[excess:]
	return pruned
}

// getResponseIDs returns a snapshot of tracked response IDs.
func (rt *serveRuntime) getResponseIDs() []string {
	rt.responseMu.Lock()
	defer rt.responseMu.Unlock()
	return append([]string(nil), rt.responseIDs...)
}

// snapshotHistory returns a copy of the current history when the runtime is idle.
// If a run is already in progress, it returns nil so callers can fall back to
// persisted session history or report busy via the normal run path.
func (rt *serveRuntime) snapshotHistory() []llm.Message {
	if rt == nil || !rt.mu.TryLock() {
		return nil
	}
	defer rt.mu.Unlock()
	history := make([]llm.Message, len(rt.history))
	copy(history, rt.history)
	return history
}

type serveRunResult struct {
	Text         strings.Builder
	ToolCalls    []llm.ToolCall
	Usage        llm.Usage
	SessionUsage llm.Usage
}

var (
	errServeSessionBusy         = errors.New("session is busy processing another request")
	errServeSessionLimitReached = errors.New("session limit reached: all sessions are busy")
)

func (rt *serveRuntime) Run(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request) (serveRunResult, error) {
	return rt.runWithGoal(ctx, stateful, replaceHistory, inputMessages, req, nil, nil)
}

func (rt *serveRuntime) RunWithEvents(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onEvent func(llm.Event) error) (serveRunResult, error) {
	return rt.runWithGoal(ctx, stateful, replaceHistory, inputMessages, req, nil, onEvent)
}

func (rt *serveRuntime) RunWithEventsAndStart(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onStart func(), onEvent func(llm.Event) error) (serveRunResult, error) {
	return rt.runWithGoal(ctx, stateful, replaceHistory, inputMessages, req, onStart, onEvent)
}

func (rt *serveRuntime) runOnce(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onStart func(), onEvent func(llm.Event) error) (serveRunResult, error) {
	if !rt.mu.TryLock() {
		return serveRunResult{}, errServeSessionBusy
	}
	defer rt.mu.Unlock()
	if onStart != nil {
		onStart()
	}
	rt.Touch()
	persisted := rt.ensurePersistedSession(ctx, req.SessionID, inputMessages)
	if persisted {
		rt.persistStatus(ctx, req.SessionID, session.StatusActive)
	}

	if !stateful {
		rt.engine.ResetConversation()
		rt.history = nil
		rt.historyPersisted = false
	}
	if stateful && !replaceHistory && hasUserMessage(inputMessages) {
		// A cancelled/interrupted run can leave the persisted transcript ending in
		// a user message with no assistant reply. Drop that orphan before appending
		// the next submitted user turn; the snapshot persistence path below rewrites
		// the store so providers never see consecutive user turns. If the UI retries
		// the same prompt, this also avoids answering it twice.
		rt.dropTrailingUserHistory()
	}

	baseHistory := make([]llm.Message, len(rt.history))
	copy(baseHistory, rt.history)
	replacingExistingHistory := replaceHistory && len(baseHistory) > 0
	replaceHistoryBackup := baseHistory
	replaceUsageBackup := rt.cumulativeUsage
	replacePlatformBackup := rt.lastInjectedPlatform
	replacePersistedBackup := rt.historyPersisted
	if replaceHistory {
		baseHistory = nil
		rt.history = nil
		rt.engine.ResetConversation()
		rt.cumulativeUsage = llm.Usage{}
		rt.lastInjectedPlatform = ""
		rt.historyPersisted = false
	}
	if persisted && stateful && !replaceHistory {
		rt.restoreProviderState(ctx, req.SessionID)
	}
	turnIndex := countUserMessages(baseHistory)

	var injectedPlatform string
	if devText := rt.platformMessages.For(rt.platform); devText != "" && rt.lastInjectedPlatform != rt.platform {
		devMsg := llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: devText}}}
		inputMessages = append([]llm.Message{devMsg}, inputMessages...)
		injectedPlatform = rt.platform
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	askUserFunc := rt.askUserFunc
	if askUserFunc == nil {
		switch rt.platform {
		case "", "web":
			askUserFunc = rt.awaitAskUser
		case "telegram", "jobs":
			platform := rt.platform
			askUserFunc = func(context.Context, []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
				return nil, fmt.Errorf("ask_user is not available on %s sessions", platform)
			}
		}
	}
	if askUserFunc != nil {
		runCtx = tools.ContextWithAskUserUIFunc(runCtx, askUserFunc)
	}
	if rt.platform == "web" && strings.TrimSpace(req.SessionID) != "" {
		runCtx = tools.ContextWithQueueAgentOrigin(runCtx, tools.QueueAgentOriginContext{
			Origin:    tools.QueueAgentOriginWeb,
			SessionID: req.SessionID,
		})
	}

	activeModel := strings.TrimSpace(req.Model)
	activeEffort := strings.TrimSpace(req.ReasoningEffort)
	if activeModel == "" {
		activeModel = strings.TrimSpace(rt.defaultModel)
	}
	activeModel, activeEffort = normalizeProviderModelEffort(runtimeProviderKey(rt), activeModel, activeEffort)
	intState := &runtimeInterruptState{
		cancel:          runCancel,
		done:            make(chan struct{}),
		currentTask:     lastUserText(inputMessages),
		model:           activeModel,
		reasoningEffort: activeEffort,
	}
	rt.setActiveInterrupt(intState)
	defer func() {
		close(intState.done)
		rt.clearActiveInterrupt(intState)
		if rt.engine != nil {
			rt.engine.ClearPendingRequestModelSwitch()
		}
	}()

	messages := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+1)
	systemPromptInjected := rt.systemPrompt != "" && !containsSystemMessage(baseHistory) && !containsSystemMessage(inputMessages)
	if systemPromptInjected {
		messages = append(messages, llm.SystemText(rt.systemPrompt))
	}
	messages = append(messages, baseHistory...)
	messages = append(messages, inputMessages...)

	req.Messages = messages
	// The runtime's restored session/worktree binding is authoritative for both
	// local tools and local CLI providers. Keep caller-supplied values only when
	// this runtime has no explicit base directory.
	if rt.toolMgr != nil {
		if baseDir := strings.TrimSpace(rt.toolMgr.BaseDir()); baseDir != "" {
			req.WorkingDir = baseDir
		}
	}

	// Re-set each run in case the request selects a different model than the
	// runtime default, matching the TUI's per-turn context management behavior.
	rt.configureContextManagementForRequest(req)

	var produced []llm.Message
	var producedMu sync.Mutex
	var lastAppendedIdx int // tracks how many produced messages have been incrementally persisted
	assistantSnapshotDirty := false
	assistantSnapshotNeedsReconcile := false

	// Persist-as-we-go: snapshot callback fires per streamed tool call. The
	// pending row lives at produced[pendingAssistantIdx] and is upserted in
	// place via UpdateMessage once it has been added — so repeated snapshots
	// replace a single logical row instead of appending duplicates. After a
	// turn completes (turn callback for async tools, or first upsert for
	// text-only turns) both fields reset so the next turn starts fresh.
	pendingAssistantIdx := -1
	var pendingAssistantMsgID int64
	pendingAssistantTextPersisted := false
	persistPlatformInjectionLocked := func() {
		if injectedPlatform != "" && (stateful || persisted) {
			rt.lastInjectedPlatform = injectedPlatform
		}
	}
	buildSnapshotLocked := func() []llm.Message {
		snapshot := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+len(produced)+1)
		if systemPromptInjected {
			snapshot = append(snapshot, llm.SystemText(rt.systemPrompt))
		}
		snapshot = append(snapshot, baseHistory...)
		snapshot = append(snapshot, inputMessages...)
		snapshot = append(snapshot, produced...)
		return snapshot
	}

	compactedActiveHistory := false
	persistProducedSnapshot := func(persistCtx context.Context) {
		producedMu.Lock()
		defer producedMu.Unlock()

		snapshot := buildSnapshotLocked()
		useCompactedSnapshot := compactedActiveHistory
		if stateful {
			rt.history = snapshot
			rt.historyPersisted = false
		}
		if persisted {
			if useCompactedSnapshot {
				rt.historyPersisted = rt.persistCompactedSnapshot(persistCtx, req.SessionID, snapshot)
			} else {
				rt.historyPersisted = rt.persistSnapshot(persistCtx, req.SessionID, snapshot)
			}
		}
		persistPlatformInjectionLocked()
	}

	appendOnlyPersisted := persisted && !replaceHistory && rt.historyPersisted
	initialPersisted := false
	initialMessages := make([]llm.Message, 0, len(inputMessages)+1)
	if systemPromptInjected {
		initialMessages = append(initialMessages, llm.SystemText(rt.systemPrompt))
	}
	initialMessages = append(initialMessages, inputMessages...)
	initialAppendedIdx := 0
	appendOnlyCaughtUpLocked := func() bool {
		return appendOnlyPersisted &&
			initialPersisted &&
			initialAppendedIdx >= len(initialMessages) &&
			lastAppendedIdx >= len(produced) &&
			!assistantSnapshotDirty &&
			!assistantSnapshotNeedsReconcile
	}
	appendInitialInputLocked := func(persistCtx context.Context) bool {
		if !appendOnlyPersisted || initialAppendedIdx >= len(initialMessages) {
			initialPersisted = true
			return true
		}
		written := rt.appendMessages(persistCtx, req.SessionID, initialMessages[initialAppendedIdx:], turnIndex)
		initialAppendedIdx += written
		if initialAppendedIdx < len(initialMessages) {
			appendOnlyPersisted = false
			rt.historyPersisted = false
			return false
		}
		initialPersisted = true
		return true
	}

	// Make the submitted user turn durable before waiting for the provider's
	// first streaming callback. The web UI can navigate away and reload the
	// session while the model is still thinking; if we only persist on the first
	// assistant/tool event, the just-submitted prompt temporarily disappears.
	// For replace-history runs with existing history, keep the old transcript
	// until the run produces an event so an early provider failure cannot wipe it.
	if persisted && (!replaceHistory || !replacingExistingHistory) {
		if appendOnlyPersisted {
			appendInitialInputLocked(ctx)
		} else {
			initialSnapshot := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+1)
			if systemPromptInjected {
				initialSnapshot = append(initialSnapshot, llm.SystemText(rt.systemPrompt))
			}
			initialSnapshot = append(initialSnapshot, baseHistory...)
			initialSnapshot = append(initialSnapshot, inputMessages...)
			initialPersisted = rt.persistSnapshot(ctx, req.SessionID, initialSnapshot)
		}
	}

	// updateStateAndAppend updates in-memory history and incrementally appends
	// only the NEW produced messages to the DB. Each append is a small atomic
	// SQLite INSERT that survives kill -9, so at most one turn can be lost.
	// On the first callback, it eagerly persists baseHistory + inputMessages and
	// only starts incremental appends after that snapshot succeeds.
	updateStateAndAppendLocked := func(persistCtx context.Context) {
		if stateful {
			rt.history = buildSnapshotLocked()
			rt.historyPersisted = false
		}
		if persisted {
			if appendOnlyPersisted {
				if !appendInitialInputLocked(persistCtx) {
					persistPlatformInjectionLocked()
					return
				}
				if lastAppendedIdx < len(produced) {
					written := rt.appendMessages(persistCtx, req.SessionID, produced[lastAppendedIdx:], turnIndex)
					lastAppendedIdx += written
					if lastAppendedIdx < len(produced) {
						appendOnlyPersisted = false
						rt.historyPersisted = false
					}
				}
				persistPlatformInjectionLocked()
				return
			}
			// On the first callback, persist the full base snapshot so
			// incremental AddMessage calls have the correct starting state.
			if !initialPersisted {
				initialSnapshot := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+1)
				if systemPromptInjected {
					initialSnapshot = append(initialSnapshot, llm.SystemText(rt.systemPrompt))
				}
				initialSnapshot = append(initialSnapshot, baseHistory...)
				initialSnapshot = append(initialSnapshot, inputMessages...)
				initialPersisted = rt.persistSnapshot(persistCtx, req.SessionID, initialSnapshot)
			}
			if initialPersisted && lastAppendedIdx < len(produced) {
				written := rt.appendMessages(persistCtx, req.SessionID, produced[lastAppendedIdx:], turnIndex)
				lastAppendedIdx += written
			}
		}
		persistPlatformInjectionLocked()
	}
	var compactionUsage llm.Usage
	var compactionUsageMu sync.Mutex

	// Keep runtime-owned history in sync with engine compaction. The engine only
	// replaces its in-flight request; without this callback serve/web would later
	// rebuild snapshots from stale baseHistory/inputMessages/produced and
	// resurrect the pre-compaction context.
	rt.engine.SetCompactionCallback(func(cbCtx context.Context, result *llm.CompactionResult) error {
		producedMu.Lock()
		defer producedMu.Unlock()
		if result == nil {
			return nil
		}
		handledByPlatform := rt.compactionCB != nil
		if handledByPlatform {
			if err := rt.compactionCB(cbCtx, result); err != nil {
				return err
			}
		}
		var compacted []llm.Message
		if handledByPlatform {
			compacted = append(compacted, result.NewMessages...)
		} else {
			updated, _, refreshed, err := session.ApplyCompaction(cbCtx, rt.store, rt.sessionMeta, nil, result)
			if err != nil {
				return err
			}
			if refreshed != nil {
				rt.sessionMeta = refreshed
			}
			compacted = make([]llm.Message, 0, len(updated))
			for _, msg := range updated {
				compacted = append(compacted, msg.ToLLMMessage())
			}
		}
		if !result.Usage.IsZero() {
			compactionUsageMu.Lock()
			compactionUsage.Add(result.Usage)
			compactionUsageMu.Unlock()
		}
		if !result.Usage.BillableCountersZero() && rt.store != nil && rt.sessionMeta != nil {
			if err := rt.store.UpdateMetrics(cbCtx, rt.sessionMeta.ID, 0, 0, result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.CachedInputTokens, result.Usage.CacheWriteTokens); err == nil {
				rt.sessionMeta.InputTokens += result.Usage.InputTokens
				rt.sessionMeta.OutputTokens += result.Usage.OutputTokens
				rt.sessionMeta.CachedInputTokens += result.Usage.CachedInputTokens
				rt.sessionMeta.CacheWriteTokens += result.Usage.CacheWriteTokens
			}
		}
		if len(compacted) == 0 {
			compacted = append(compacted, result.NewMessages...)
		}
		baseHistory = compacted
		compactedActiveHistory = true
		inputMessages = nil
		produced = nil
		lastAppendedIdx = 0
		initialPersisted = persisted
		initialAppendedIdx = len(initialMessages)
		systemPromptInjected = false
		pendingAssistantIdx = -1
		pendingAssistantMsgID = 0
		pendingAssistantTextPersisted = false
		assistantSnapshotDirty = false
		assistantSnapshotNeedsReconcile = false
		if stateful {
			rt.history = append([]llm.Message(nil), compacted...)
			rt.historyPersisted = persisted
		}
		rt.engine.SetContextEstimateBaseline(0, 0)
		persistPlatformInjectionLocked()
		return nil
	})
	defer rt.engine.SetCompactionCallback(nil)

	// upsertPendingAssistantLocked writes (or rewrites) the in-progress
	// assistant row for the current turn. First call inserts, subsequent calls
	// update the same row; on ErrNotFound it re-inserts. Must hold producedMu.
	upsertPendingAssistantLocked := func(persistCtx context.Context, assistantMsg llm.Message, finalizeText bool) {
		firstInsert := pendingAssistantIdx < 0
		if firstInsert {
			pendingAssistantIdx = len(produced)
			produced = append(produced, assistantMsg)
			pendingAssistantTextPersisted = false
		} else {
			produced[pendingAssistantIdx] = assistantMsg
		}
		assistantSnapshotDirty = true
		if stateful {
			rt.history = buildSnapshotLocked()
			rt.historyPersisted = false
		}
		if !persisted {
			persistPlatformInjectionLocked()
			return
		}
		if appendOnlyPersisted {
			if !appendInitialInputLocked(persistCtx) {
				persistPlatformInjectionLocked()
				return
			}
		} else if !initialPersisted {
			initialSnapshot := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+1)
			if systemPromptInjected {
				initialSnapshot = append(initialSnapshot, llm.SystemText(rt.systemPrompt))
			}
			initialSnapshot = append(initialSnapshot, baseHistory...)
			initialSnapshot = append(initialSnapshot, inputMessages...)
			initialPersisted = rt.persistSnapshot(persistCtx, req.SessionID, initialSnapshot)
			if !initialPersisted {
				persistPlatformInjectionLocked()
				return
			}
		}
		dbCtx, cancel := inlinePersistContext(persistCtx, 10*time.Second)
		defer cancel()
		sessionMsg := session.NewMessage(req.SessionID, assistantMsg, -1)
		sessionMsg.TurnIndex = turnIndex
		if pendingAssistantMsgID != 0 {
			sessionMsg.ID = pendingAssistantMsgID
			err := session.UpdateStreamingMessage(dbCtx, rt.store, req.SessionID, sessionMsg, finalizeText)
			if err == nil {
				assistantSnapshotDirty = false
				if finalizeText {
					pendingAssistantTextPersisted = true
				}
				persistPlatformInjectionLocked()
				return
			}
			if !errors.Is(err, session.ErrNotFound) {
				assistantSnapshotNeedsReconcile = true
				appendOnlyPersisted = false
				rt.historyPersisted = false
				log.Printf("[serve] session UpdateMessage failed for %s: %v", req.SessionID, err)
				persistPlatformInjectionLocked()
				return
			}
			// Row missing (e.g., compaction). Fall through to re-insert.
			pendingAssistantMsgID = 0
			pendingAssistantTextPersisted = false
			sessionMsg = session.NewMessage(req.SessionID, assistantMsg, -1)
			sessionMsg.TurnIndex = turnIndex
		}
		if err := rt.store.AddMessage(dbCtx, req.SessionID, sessionMsg); err != nil {
			assistantSnapshotNeedsReconcile = true
			appendOnlyPersisted = false
			rt.historyPersisted = false
			log.Printf("[serve] session AddMessage failed for %s: %v", req.SessionID, err)
			persistPlatformInjectionLocked()
			return
		}
		pendingAssistantMsgID = sessionMsg.ID
		pendingAssistantTextPersisted = finalizeText
		assistantSnapshotDirty = false
		// Reserve produced[pendingAssistantIdx] so plain append-path writes
		// skip it on subsequent callbacks.
		if pendingAssistantIdx+1 > lastAppendedIdx {
			lastAppendedIdx = pendingAssistantIdx + 1
		}
		persistPlatformInjectionLocked()
	}

	// Snapshot fires before each EventToolCall so partial content survives a
	// consumer cancellation mid-turn.
	rt.engine.SetAssistantSnapshotCallback(func(cbCtx context.Context, callbackTurnIndex int, assistantMsg llm.Message) error {
		producedMu.Lock()
		defer producedMu.Unlock()
		upsertPendingAssistantLocked(cbCtx, assistantMsg, false)
		if rt.assistantSnapshotCB != nil {
			return rt.assistantSnapshotCB(cbCtx, callbackTurnIndex, assistantMsg)
		}
		return nil
	})
	defer rt.engine.SetAssistantSnapshotCallback(nil)

	rt.engine.SetResponseCompletedCallback(func(cbCtx context.Context, callbackTurnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
		producedMu.Lock()
		defer producedMu.Unlock()
		upsertPendingAssistantLocked(cbCtx, assistantMsg, true)
		if rt.responseCompletedCB != nil {
			return rt.responseCompletedCB(cbCtx, callbackTurnIndex, assistantMsg, metrics)
		}
		return nil
	})
	defer rt.engine.SetResponseCompletedCallback(nil)

	// Turn callback: upsert the assistant row if present as first element, then
	// plain-append the rest (tool results or interjections). Reset pending at
	// end of turn.
	rt.engine.SetTurnCompletedCallback(func(cbCtx context.Context, callbackTurnIndex int, msgs []llm.Message, metrics llm.TurnMetrics) error {
		func() {
			producedMu.Lock()
			defer producedMu.Unlock()
			appendStart := 0
			if len(msgs) > 0 && msgs[0].Role == llm.RoleAssistant {
				upsertPendingAssistantLocked(cbCtx, msgs[0], !pendingAssistantTextPersisted)
				appendStart = 1
			}
			if appendStart < len(msgs) {
				produced = append(produced, msgs[appendStart:]...)
				updateStateAndAppendLocked(cbCtx)
			}
			pendingAssistantIdx = -1
			pendingAssistantMsgID = 0
			pendingAssistantTextPersisted = false
		}()

		rt.persistTurnAccounting(cbCtx, persisted, req.SessionID, msgs, metrics)
		if rt.turnCompletedCB != nil {
			return rt.turnCompletedCB(cbCtx, callbackTurnIndex, msgs, metrics)
		}
		return nil
	})
	defer rt.engine.SetTurnCompletedCallback(nil)

	// Safety net: on error exits, persist a full snapshot so the final DB
	// state is consistent. If streamed text was shown before any callback fired,
	// synthesize an assistant message from result.Text so the partial reply is
	// not dropped. Successful runs stay on the incremental path unless a
	// fallback snapshot is still needed to reconcile missed writes.
	result := serveRunResult{}
	var runErr error
	defer func() {
		if runErr == nil {
			return
		}
		if !persisted {
			if replaceHistory {
				rt.history = replaceHistoryBackup
				rt.cumulativeUsage = replaceUsageBackup
				rt.lastInjectedPlatform = replacePlatformBackup
				rt.historyPersisted = replacePersistedBackup
			}
			return
		}
		producedMu.Lock()
		if len(produced) == 0 && result.Text.Len() > 0 {
			produced = append(produced, llm.AssistantText(result.Text.String()))
		}
		hasProduced := len(produced) > 0
		producedMu.Unlock()
		if !hasProduced {
			if replaceHistory && !initialPersisted {
				rt.history = replaceHistoryBackup
				rt.cumulativeUsage = replaceUsageBackup
				rt.lastInjectedPlatform = replacePlatformBackup
				rt.historyPersisted = replacePersistedBackup
			}
			return
		}
		deferCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if appendOnlyPersisted {
			producedMu.Lock()
			updateStateAndAppendLocked(deferCtx)
			caughtUp := appendOnlyCaughtUpLocked()
			producedMu.Unlock()
			if caughtUp {
				rt.historyPersisted = true
				return
			}
		}
		persistProducedSnapshot(deferCtx)
	}()

	stream, err := rt.engine.Stream(runCtx, req)
	if err != nil {
		runErr = err
		if persisted {
			rt.persistStatus(ctx, req.SessionID, statusForRunError(err))
		}
		return serveRunResult{}, err
	}
	defer stream.Close()

	for {
		ev, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			runErr = recvErr
			if persisted {
				rt.persistStatus(ctx, req.SessionID, statusForRunError(recvErr))
			}
			return serveRunResult{}, recvErr
		}

		if onEvent != nil {
			if err := onEvent(ev); err != nil {
				runErr = err
				if persisted {
					rt.persistStatus(ctx, req.SessionID, statusForRunError(err))
				}
				return serveRunResult{}, err
			}
		}
		rt.updateInterruptFromEvent(ev)

		switch ev.Type {
		case llm.EventTextDelta:
			result.Text.WriteString(ev.Text)
		case llm.EventAttemptDiscard:
			result.Text.Reset()
			result.Usage = llm.Usage{}
		case llm.EventToolCall:
			if ev.Tool != nil {
				result.ToolCalls = append(result.ToolCalls, *ev.Tool)
			}
		case llm.EventUsage:
			if ev.Use != nil {
				result.Usage.Add(*ev.Use)
			}
		case llm.EventError:
			if ev.Err != nil {
				runErr = ev.Err
				if persisted {
					rt.persistStatus(ctx, req.SessionID, statusForRunError(ev.Err))
				}
				return serveRunResult{}, ev.Err
			}
		}
	}

	// Do not drain residual queued interjections here. If the run ended without a
	// tool boundary, queued interjections were never submitted to the provider and
	// must remain cancellable/pending for UI recovery or explicit follow-up.

	// Accumulate cumulative session-level usage, including helper calls used for
	// any auto-compaction that happened during this run.
	compactionUsageMu.Lock()
	rt.cumulativeUsage.Add(compactionUsage)
	compactionUsageMu.Unlock()
	rt.cumulativeUsage.Add(result.Usage)
	result.SessionUsage = rt.cumulativeUsage

	var newHistory []llm.Message
	var needFinalSnapshot bool
	var needCompactedSnapshot bool
	producedMu.Lock()
	newHistory = buildSnapshotLocked()
	synthesizedAssistant := len(produced) == 0 && result.Text.Len() > 0
	if synthesizedAssistant {
		assistantMsg := llm.AssistantText(result.Text.String())
		newHistory = append(newHistory, assistantMsg)
		if appendOnlyPersisted {
			produced = append(produced, assistantMsg)
		}
	}
	if stateful {
		rt.history = newHistory
		rt.historyPersisted = false
	}
	needFinalSnapshot = false
	if persisted {
		if appendOnlyPersisted {
			if (assistantSnapshotDirty || assistantSnapshotNeedsReconcile) && pendingAssistantIdx >= 0 && pendingAssistantIdx < len(produced) {
				upsertPendingAssistantLocked(ctx, produced[pendingAssistantIdx], true)
			}
			updateStateAndAppendLocked(ctx)
			needFinalSnapshot = !appendOnlyCaughtUpLocked()
		} else {
			needFinalSnapshot = !initialPersisted || lastAppendedIdx < len(produced) || assistantSnapshotDirty || assistantSnapshotNeedsReconcile || synthesizedAssistant
		}
		needCompactedSnapshot = compactedActiveHistory
	}
	persistPlatformInjectionLocked()
	producedMu.Unlock()
	if needFinalSnapshot {
		if needCompactedSnapshot {
			rt.historyPersisted = rt.persistCompactedSnapshot(ctx, req.SessionID, newHistory)
		} else {
			rt.historyPersisted = rt.persistSnapshot(ctx, req.SessionID, newHistory)
		}
	} else if persisted {
		rt.historyPersisted = true
	}

	if persisted && stateful {
		rt.persistProviderState(ctx, req.SessionID)
	}

	return result, nil
}

func (rt *serveRuntime) restorePlatformInjectionStateFromHistory() {
	if rt == nil || rt.lastInjectedPlatform != "" {
		return
	}
	platform := strings.TrimSpace(rt.platform)
	if platform == "" {
		return
	}
	devText := strings.TrimSpace(rt.platformMessages.For(platform))
	if devText == "" {
		return
	}
	for _, msg := range rt.history {
		if msg.Role == llm.RoleDeveloper && strings.TrimSpace(llm.MessageText(msg)) == devText {
			rt.lastInjectedPlatform = platform
			return
		}
	}
}

func (rt *serveRuntime) dropTrailingUserHistory() {
	if rt == nil || len(rt.history) == 0 {
		return
	}
	trimmedLen := len(rt.history)
	for trimmedLen > 0 && rt.history[trimmedLen-1].Role == llm.RoleUser {
		trimmedLen--
	}
	if trimmedLen == len(rt.history) {
		return
	}
	trimmed := make([]llm.Message, trimmedLen)
	copy(trimmed, rt.history[:trimmedLen])
	rt.history = trimmed
	// The persisted transcript still contains the unanswered user turn(s). Force
	// the next persistence step down the snapshot path so the store is reconciled
	// before the provider response streams.
	rt.historyPersisted = false
}

func hasUserMessage(messages []llm.Message) bool {
	for _, msg := range messages {
		if msg.Role == llm.RoleUser {
			return true
		}
	}
	return false
}

func statusForRunError(err error) session.SessionStatus {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return session.StatusInterrupted
	}
	return session.StatusError
}

func containsSystemMessage(messages []llm.Message) bool {
	for _, msg := range messages {
		if msg.Role == llm.RoleSystem {
			return true
		}
	}
	return false
}

// isServerExecutedTool returns true if a tool call will be executed by the
// server's engine (registered tool or mapped via toolMap). Such calls should
// not be forwarded to API clients as tool_use blocks because the server
// handles them internally.
func (rt *serveRuntime) isServerExecutedTool(name string) bool {
	lookupName := name
	if rt.toolMap != nil {
		if mapped, ok := rt.toolMap[name]; ok {
			lookupName = mapped
		}
	}
	_, ok := rt.engine.Tools().Get(lookupName)
	return ok
}

func countUserMessages(messages []llm.Message) int {
	count := 0
	for _, msg := range messages {
		if msg.Role == llm.RoleUser {
			count++
		}
	}
	return count
}

func lastUserText(messages []llm.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != llm.RoleUser {
			continue
		}
		var parts []string
		for _, p := range msg.Parts {
			if p.Type == llm.PartText && strings.TrimSpace(p.Text) != "" {
				parts = append(parts, strings.TrimSpace(p.Text))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return ""
}
