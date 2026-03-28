package cmd

import (
	"context"
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
	systemPrompt         string
	history              []llm.Message
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
	defaultModel         string
	lastUsedUnixNano     atomic.Int64
	activeInterrupt      *runtimeInterruptState
	lastResponseID       string
	responseIDs          []string
	cumulativeUsage      llm.Usage
	pendingAskUsers      map[string]*servePendingAskUser
	pendingApprovals     map[string]*servePendingApproval
	approvalEventFunc    func(event string, data map[string]any) error
	approvalCtx          context.Context
	lastUIRunError       string
	platform             string
	platformMessages     agents.PlatformMessagesConfig
	lastInjectedPlatform string
}

type runtimeInterruptState struct {
	cancel      context.CancelFunc
	done        chan struct{}
	currentTask string
	toolsRun    []string
	proseLen    int
	activeTool  string
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

func (rt *serveRuntime) Close() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.interruptMu.Lock()
	if rt.activeInterrupt != nil && rt.activeInterrupt.cancel != nil {
		rt.activeInterrupt.cancel()
	}
	rt.interruptMu.Unlock()
	rt.clearPendingAskUsers()
	rt.clearPendingApprovals()
	if rt.mcpManager != nil {
		rt.mcpManager.StopAll()
		rt.mcpManager = nil
	}
	if cleaner, ok := rt.provider.(interface{ CleanupMCP() }); ok {
		cleaner.CleanupMCP()
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
	case llm.EventToolExecStart:
		rt.activeInterrupt.activeTool = ev.ToolName
		if ev.ToolName != "" {
			rt.activeInterrupt.toolsRun = append(rt.activeInterrupt.toolsRun, ev.ToolName)
		}
	case llm.EventToolExecEnd:
		rt.activeInterrupt.activeTool = ""
	}
}

func (rt *serveRuntime) Interrupt(ctx context.Context, msg string, fastProvider llm.Provider) (llm.InterruptAction, error) {
	rt.interruptMu.Lock()
	state := rt.activeInterrupt
	if state == nil {
		rt.interruptMu.Unlock()
		return llm.InterruptInterject, fmt.Errorf("session has no active stream")
	}
	cancel := state.cancel
	activity := llm.InterruptActivity{
		CurrentTask: state.currentTask,
		ToolsRun:    append([]string(nil), state.toolsRun...),
		ProseLen:    state.proseLen,
		ActiveTool:  state.activeTool,
	}
	rt.interruptMu.Unlock()
	action := llm.ClassifyInterrupt(ctx, fastProvider, msg, activity)
	switch action {
	case llm.InterruptCancel:
		if cancel != nil {
			cancel()
		}
	case llm.InterruptInterject:
		rt.engine.Interject(msg)
	}
	return action, nil
}

func (rt *serveRuntime) ensurePersistedSession(ctx context.Context, sessionID string, inputMessages []llm.Message) bool {
	if rt.store == nil || sessionID == "" {
		return false
	}
	if rt.sessionMeta != nil && rt.sessionMeta.ID == sessionID {
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
		ID:        sessionID,
		Provider:  providerName,
		Model:     modelName,
		Mode:      session.ModeChat,
		Origin:    session.OriginWeb,
		Agent:     rt.agentName,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Search:    rt.search,
		Tools:     rt.toolsSetting,
		MCP:       rt.mcpSetting,
		Status:    session.StatusActive,
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
		if len(rt.history) == 0 {
			msgs, loadErr := rt.store.GetMessages(ctx, sessionID, 0, 0)
			if loadErr == nil && len(msgs) > 0 {
				llmMsgs := make([]llm.Message, 0, len(msgs))
				for _, m := range msgs {
					llmMsgs = append(llmMsgs, m.ToLLMMessage())
				}
				rt.history = llmMsgs
			}
		}
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

func (rt *serveRuntime) persistSnapshot(ctx context.Context, sessionID string, snapshot []llm.Message) {
	if rt.store == nil || sessionID == "" {
		return
	}
	messages := make([]session.Message, 0, len(snapshot))
	for _, msg := range snapshot {
		if msg.Role == "" {
			continue
		}
		sessionMsg := session.NewMessage(sessionID, msg, -1)
		messages = append(messages, *sessionMsg)
	}
	if err := rt.store.ReplaceMessages(ctx, sessionID, messages); err != nil {
		log.Printf("[serve] session ReplaceMessages failed for %s: %v", sessionID, err)
		return
	}
	if rt.sessionMeta != nil && rt.sessionMeta.Summary == "" {
		for _, msg := range snapshot {
			if msg.Role != llm.RoleUser {
				continue
			}
			if text := session.NewMessage(sessionID, msg, -1).TextContent; text != "" {
				rt.sessionMeta.Summary = session.TruncateSummary(text)
				if updateErr := rt.store.Update(ctx, rt.sessionMeta); updateErr != nil {
					log.Printf("[serve] session Update failed for %s: %v", sessionID, updateErr)
				}
				break
			}
		}
	}
	if setErr := rt.store.SetCurrent(ctx, sessionID); setErr != nil {
		log.Printf("[serve] session SetCurrent failed for %s: %v", sessionID, setErr)
	}
	if statusErr := rt.store.UpdateStatus(ctx, sessionID, session.StatusActive); statusErr != nil {
		log.Printf("[serve] session UpdateStatus(active) failed for %s: %v", sessionID, statusErr)
	}
}

func (rt *serveRuntime) persistStatus(ctx context.Context, sessionID string, status session.SessionStatus) {
	if rt.store == nil || sessionID == "" {
		return
	}
	if err := rt.store.UpdateStatus(ctx, sessionID, status); err != nil {
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

type serveRunResult struct {
	Text         strings.Builder
	ToolCalls    []llm.ToolCall
	Usage        llm.Usage
	SessionUsage llm.Usage
}

var errServeSessionBusy = errors.New("session is busy processing another request")

func (rt *serveRuntime) Run(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request) (serveRunResult, error) {
	return rt.run(ctx, stateful, replaceHistory, inputMessages, req, nil)
}

func (rt *serveRuntime) RunWithEvents(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onEvent func(llm.Event) error) (serveRunResult, error) {
	return rt.run(ctx, stateful, replaceHistory, inputMessages, req, onEvent)
}

func (rt *serveRuntime) run(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onEvent func(llm.Event) error) (serveRunResult, error) {
	if !rt.mu.TryLock() {
		return serveRunResult{}, errServeSessionBusy
	}
	defer rt.mu.Unlock()
	rt.Touch()
	persisted := rt.ensurePersistedSession(ctx, req.SessionID, inputMessages)
	if persisted {
		rt.persistStatus(ctx, req.SessionID, session.StatusActive)
	}

	if !stateful {
		rt.engine.ResetConversation()
		rt.history = nil
	}

	baseHistory := make([]llm.Message, len(rt.history))
	copy(baseHistory, rt.history)
	if replaceHistory {
		baseHistory = nil
		rt.engine.ResetConversation()
		rt.cumulativeUsage = llm.Usage{}
		rt.lastInjectedPlatform = ""
	}

	if devText := rt.platformMessages.For(rt.platform); devText != "" && rt.lastInjectedPlatform != rt.platform {
		devMsg := llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: devText}}}
		inputMessages = append([]llm.Message{devMsg}, inputMessages...)
		rt.lastInjectedPlatform = rt.platform
	}

	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()
	runCtx = tools.ContextWithAskUserUIFunc(runCtx, rt.awaitAskUser)

	intState := &runtimeInterruptState{
		cancel:      runCancel,
		done:        make(chan struct{}),
		currentTask: lastUserText(inputMessages),
	}
	rt.setActiveInterrupt(intState)
	defer func() {
		close(intState.done)
		rt.clearActiveInterrupt(intState)
	}()

	messages := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+1)
	if rt.systemPrompt != "" && !containsSystemMessage(baseHistory) && !containsSystemMessage(inputMessages) {
		messages = append(messages, llm.SystemText(rt.systemPrompt))
	}
	messages = append(messages, baseHistory...)
	messages = append(messages, inputMessages...)

	req.Messages = messages

	var produced []llm.Message
	persistProducedSnapshot := func(persistCtx context.Context) {
		snapshot := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+len(produced))
		snapshot = append(snapshot, baseHistory...)
		snapshot = append(snapshot, inputMessages...)
		snapshot = append(snapshot, produced...)
		if stateful {
			rt.history = snapshot
		}
		if persisted {
			rt.persistSnapshot(persistCtx, req.SessionID, snapshot)
		}
	}
	// ResponseCompletedCallback receives the assistant message (with tool call parts)
	// immediately after streaming, BEFORE tool execution. Without this, tool calls
	// are missing from persisted sessions because TurnCompletedCallback only receives
	// tool results.
	rt.engine.SetResponseCompletedCallback(func(cbCtx context.Context, _ int, assistantMsg llm.Message, _ llm.TurnMetrics) error {
		produced = append(produced, assistantMsg)
		persistProducedSnapshot(cbCtx)
		return nil
	})
	defer rt.engine.SetResponseCompletedCallback(nil)
	// TurnCompletedCallback receives tool results after execution, or the final
	// assistant message when no tools are used (ResponseCompletedCallback never fires).
	rt.engine.SetTurnCompletedCallback(func(cbCtx context.Context, _ int, msgs []llm.Message, _ llm.TurnMetrics) error {
		produced = append(produced, msgs...)
		persistProducedSnapshot(cbCtx)
		return nil
	})
	defer rt.engine.SetTurnCompletedCallback(nil)

	stream, err := rt.engine.Stream(runCtx, req)
	if err != nil {
		if persisted {
			rt.persistStatus(ctx, req.SessionID, statusForRunError(err))
		}
		return serveRunResult{}, err
	}
	defer stream.Close()

	result := serveRunResult{}
	for {
		ev, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			if persisted {
				rt.persistStatus(ctx, req.SessionID, statusForRunError(recvErr))
			}
			return serveRunResult{}, recvErr
		}

		if onEvent != nil {
			if err := onEvent(ev); err != nil {
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
		case llm.EventToolCall:
			if ev.Tool != nil {
				result.ToolCalls = append(result.ToolCalls, *ev.Tool)
			}
		case llm.EventUsage:
			if ev.Use != nil {
				result.Usage.InputTokens += ev.Use.InputTokens
				result.Usage.OutputTokens += ev.Use.OutputTokens
				result.Usage.CachedInputTokens += ev.Use.CachedInputTokens
				result.Usage.CacheWriteTokens += ev.Use.CacheWriteTokens
			}
		case llm.EventError:
			if ev.Err != nil {
				if persisted {
					rt.persistStatus(ctx, req.SessionID, statusForRunError(ev.Err))
				}
				return serveRunResult{}, ev.Err
			}
		}
	}

	// Accumulate cumulative session-level usage
	rt.cumulativeUsage.InputTokens += result.Usage.InputTokens
	rt.cumulativeUsage.OutputTokens += result.Usage.OutputTokens
	rt.cumulativeUsage.CachedInputTokens += result.Usage.CachedInputTokens
	rt.cumulativeUsage.CacheWriteTokens += result.Usage.CacheWriteTokens
	result.SessionUsage = rt.cumulativeUsage

	newHistory := make([]llm.Message, 0, len(baseHistory)+len(inputMessages)+len(produced)+1)
	newHistory = append(newHistory, baseHistory...)
	newHistory = append(newHistory, inputMessages...)
	newHistory = append(newHistory, produced...)
	if len(produced) == 0 && result.Text.Len() > 0 {
		newHistory = append(newHistory, llm.AssistantText(result.Text.String()))
	}
	if stateful {
		rt.history = newHistory
	}
	if persisted {
		rt.persistSnapshot(ctx, req.SessionID, newHistory)
	}

	return result, nil
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
