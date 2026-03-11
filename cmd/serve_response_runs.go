package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

type responseRunEvent struct {
	Sequence int64
	Event    string
	Data     []byte
}

type responseRunRecoveryTool struct {
	ID        string
	Name      string
	Arguments string
	Status    string
	Created   int64
}

type responseRunRecoveryMessage struct {
	ID       string
	Role     string
	Content  string
	Created  int64
	Tools    []responseRunRecoveryTool
	Expanded bool
	Status   string
	Usage    map[string]any
}

type responseRunSubscribeResult struct {
	replay           []responseRunEvent
	ch               <-chan responseRunEvent
	snapshotRequired bool
	minReplayAfter   int64
}

type responseRun struct {
	mu                 sync.Mutex
	id                 string
	sessionID          string
	previousResponseID string
	model              string
	created            int64
	status             string
	errorType          string
	errorMessage       string
	usage              llm.Usage
	sessionUsage       llm.Usage
	lastSequenceNumber int64
	// Retain the raw event stream until the run expires so reconnecting clients can replay by sequence number.
	events             []responseRunEvent
	minReplayAfter     int64
	maxRetainedEvents  int
	recoveryMessages   []responseRunRecoveryMessage
	nextMessageOrdinal int64
	currentAssistant   int
	currentToolGroup   int
	compactionEnabled  bool
	subscribers        map[int]chan responseRunEvent
	nextSubscriberID   int
	cancel             context.CancelFunc
}

type startResponseRunOptions struct {
	previousResponseID string
	uiSession          bool
}

func newResponseRun(respID, sessionID, previousResponseID, model string, created int64, cancel context.CancelFunc) *responseRun {
	return &responseRun{
		id:                 respID,
		sessionID:          sessionID,
		previousResponseID: previousResponseID,
		model:              model,
		created:            created,
		status:             "in_progress",
		maxRetainedEvents:  defaultResponseRunReplayLimit,
		currentAssistant:   -1,
		currentToolGroup:   -1,
		compactionEnabled:  true,
		subscribers:        make(map[int]chan responseRunEvent),
		cancel:             cancel,
	}
}

func (r *responseRun) appendEvent(event string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendEventLocked(event, payload, false)
}

func (r *responseRun) complete(payload map[string]any, usage llm.Usage, sessionUsage llm.Usage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = "completed"
	r.errorType = ""
	r.errorMessage = ""
	r.usage = usage
	r.sessionUsage = sessionUsage
	return r.appendEventLocked("response.completed", payload, true)
}

func (r *responseRun) fail(payload map[string]any, errType, errMessage string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hadSubscribers := len(r.subscribers) > 0
	r.status = "failed"
	r.errorType = errType
	r.errorMessage = errMessage
	return hadSubscribers, r.appendEventLocked("response.failed", payload, true)
}

func (r *responseRun) appendEventLocked(event string, payload map[string]any, terminal bool) error {
	if payload == nil {
		payload = map[string]any{}
	}
	r.lastSequenceNumber++
	payload["sequence_number"] = r.lastSequenceNumber

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	r.applyRecoveryEventLocked(event, payload)

	stored := responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    event,
		Data:     data,
	}
	r.events = append(r.events, stored)
	r.compactEventsLocked()

	for id, ch := range r.subscribers {
		select {
		case ch <- stored:
		default:
			log.Printf("response run %s subscriber fell behind at sequence %d; closing stream", r.id, stored.Sequence)
			close(ch)
			delete(r.subscribers, id)
		}
	}

	if terminal {
		for id, ch := range r.subscribers {
			close(ch)
			delete(r.subscribers, id)
		}
	}

	return nil
}

func (r *responseRun) compactEventsLocked() {
	if !r.compactionEnabled || r.maxRetainedEvents <= 0 || len(r.events) <= r.maxRetainedEvents {
		return
	}

	dropCount := len(r.events) - r.maxRetainedEvents
	if dropCount <= 0 {
		return
	}

	nextReplayAfter := r.events[dropCount].Sequence - 1
	if nextReplayAfter > r.minReplayAfter {
		r.minReplayAfter = nextReplayAfter
	}

	trimmed := make([]responseRunEvent, len(r.events)-dropCount)
	copy(trimmed, r.events[dropCount:])
	r.events = trimmed
}

func (r *responseRun) applyRecoveryEventLocked(event string, payload map[string]any) {
	switch event {
	case "response.ask_user.prompt", "response.approval.prompt", "response.interjection":
		r.compactionEnabled = false
	}

	switch event {
	case "response.output_text.delta":
		delta := stringValue(payload["delta"])
		if delta == "" {
			return
		}
		r.closeToolGroupLocked()
		idx := r.ensureAssistantMessageLocked()
		r.recoveryMessages[idx].Content += delta
	case "response.output_text.new_segment":
		r.closeToolGroupLocked()
		r.currentAssistant = -1
		r.ensureAssistantMessageLocked()
	case "response.output_item.added":
		item := mapValue(payload["item"])
		if stringValue(item["type"]) != "function_call" {
			return
		}
		tool := responseRunRecoveryTool{
			ID:        stringValue(item["call_id"]),
			Name:      stringValue(item["name"]),
			Arguments: stringValue(item["arguments"]),
			Status:    "running",
			Created:   time.Now().UnixMilli(),
		}
		if tool.ID == "" {
			tool.ID = fmt.Sprintf("%s_tool_%d", r.id, len(r.recoveryMessages)+1)
		}
		if r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
			r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
				ID:       r.nextRecoveryMessageIDLocked("tool_group"),
				Role:     "tool-group",
				Created:  time.Now().UnixMilli(),
				Tools:    []responseRunRecoveryTool{tool},
				Expanded: false,
				Status:   "running",
			})
			r.currentToolGroup = len(r.recoveryMessages) - 1
		} else {
			group := &r.recoveryMessages[r.currentToolGroup]
			group.Tools = append(group.Tools, tool)
			group.Status = "running"
		}
		r.currentAssistant = -1
	case "response.output_item.done":
		item := mapValue(payload["item"])
		if stringValue(item["type"]) != "function_call" || r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
			return
		}
		callID := stringValue(item["call_id"])
		name := stringValue(item["name"])
		arguments := stringValue(item["arguments"])
		group := &r.recoveryMessages[r.currentToolGroup]
		for i := range group.Tools {
			if callID != "" && group.Tools[i].ID == callID {
				group.Tools[i].Arguments = arguments
				return
			}
			if callID == "" && name != "" && group.Tools[i].Name == name && group.Tools[i].Status == "running" {
				group.Tools[i].Arguments = arguments
				return
			}
		}
	case "response.tool_exec.end":
		if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
			group := &r.recoveryMessages[r.currentToolGroup]
			callID := stringValue(payload["call_id"])
			for i := range group.Tools {
				if callID == "" || group.Tools[i].ID == callID {
					group.Tools[i].Status = "done"
					if callID != "" {
						break
					}
				}
			}
			allDone := len(group.Tools) > 0
			for _, tool := range group.Tools {
				if tool.Status != "done" {
					allDone = false
					break
				}
			}
			if allDone {
				group.Status = "done"
			}
		}
		images := stringSliceValue(payload["images"])
		if len(images) == 0 {
			return
		}
		idx := r.ensureAssistantMessageLocked()
		for _, url := range images {
			r.recoveryMessages[idx].Content += fmt.Sprintf("\n\n![Generated Image](%s)\n", url)
		}
	case "response.completed":
		r.closeToolGroupLocked()
		response := mapValue(payload["response"])
		usage := mapValue(response["usage"])
		if len(usage) == 0 {
			return
		}
		for i := len(r.recoveryMessages) - 1; i >= 0; i-- {
			if r.recoveryMessages[i].Role == "assistant" {
				r.recoveryMessages[i].Usage = cloneJSONMap(usage)
				return
			}
		}
	case "response.failed":
		r.closeToolGroupLocked()
		errPayload := mapValue(payload["error"])
		message := stringValue(errPayload["message"])
		if message == "" {
			return
		}
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:      r.nextRecoveryMessageIDLocked("error"),
			Role:    "error",
			Content: message,
			Created: time.Now().UnixMilli(),
		})
		r.currentAssistant = -1
	}
}

func (r *responseRun) nextRecoveryMessageIDLocked(kind string) string {
	r.nextMessageOrdinal++
	return fmt.Sprintf("%s_%s_%d", r.id, kind, r.nextMessageOrdinal)
}

func (r *responseRun) ensureAssistantMessageLocked() int {
	if r.currentAssistant >= 0 && r.currentAssistant < len(r.recoveryMessages) {
		return r.currentAssistant
	}
	r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
		ID:      r.nextRecoveryMessageIDLocked("assistant"),
		Role:    "assistant",
		Created: time.Now().UnixMilli(),
	})
	r.currentAssistant = len(r.recoveryMessages) - 1
	return r.currentAssistant
}

func (r *responseRun) closeToolGroupLocked() {
	if r.currentToolGroup < 0 || r.currentToolGroup >= len(r.recoveryMessages) {
		return
	}
	group := &r.recoveryMessages[r.currentToolGroup]
	if group.Role != "tool-group" {
		r.currentToolGroup = -1
		return
	}
	for i := range group.Tools {
		group.Tools[i].Status = "done"
	}
	group.Status = "done"
	r.currentToolGroup = -1
}

func (r *responseRun) subscribe(after int64) responseRunSubscribeResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	if after < r.minReplayAfter {
		return responseRunSubscribeResult{
			snapshotRequired: true,
			minReplayAfter:   r.minReplayAfter,
		}
	}

	replay := make([]responseRunEvent, 0, len(r.events))
	for _, ev := range r.events {
		if ev.Sequence > after {
			replay = append(replay, ev)
		}
	}

	if r.status != "in_progress" {
		return responseRunSubscribeResult{replay: replay}
	}

	id := r.nextSubscriberID
	r.nextSubscriberID++
	ch := make(chan responseRunEvent, defaultResponseRunSubscriberBuffer)
	r.subscribers[id] = ch
	return responseRunSubscribeResult{replay: replay, ch: ch}
}

func (r *responseRun) unsubscribe(ch <-chan responseRunEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, existing := range r.subscribers {
		if existing == ch {
			// Terminal/buffer-overflow paths own channel closing.
			// Explicit unsubscribe only detaches the subscriber to avoid
			// coupling normal teardown to a specific close ordering.
			delete(r.subscribers, id)
			return
		}
	}
}

func (r *responseRun) snapshot() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	payload := map[string]any{
		"id":                   r.id,
		"object":               "response",
		"created":              r.created,
		"model":                r.model,
		"status":               r.status,
		"session_id":           r.sessionID,
		"previous_response_id": r.previousResponseID,
		"last_sequence_number": r.lastSequenceNumber,
	}
	if r.status == "completed" {
		payload["usage"] = usagePayload(r.usage)
		payload["session_usage"] = usagePayload(r.sessionUsage)
	}
	if r.errorMessage != "" {
		payload["error"] = map[string]any{
			"type":    r.errorType,
			"message": r.errorMessage,
		}
	}
	payload["recovery"] = r.recoveryPayloadLocked()
	return payload
}

func (r *responseRun) recoveryPayloadLocked() map[string]any {
	recovery := map[string]any{
		"sequence_number":  r.lastSequenceNumber,
		"min_replay_after": r.minReplayAfter,
	}
	if len(r.recoveryMessages) == 0 {
		return recovery
	}

	messages := make([]map[string]any, 0, len(r.recoveryMessages))
	for _, msg := range r.recoveryMessages {
		entry := map[string]any{
			"id":      msg.ID,
			"role":    msg.Role,
			"created": msg.Created,
		}
		if msg.Content != "" {
			entry["content"] = msg.Content
		}
		if msg.Status != "" {
			entry["status"] = msg.Status
		}
		if msg.Expanded {
			entry["expanded"] = msg.Expanded
		}
		if len(msg.Tools) > 0 {
			toolsPayload := make([]map[string]any, 0, len(msg.Tools))
			for _, tool := range msg.Tools {
				toolEntry := map[string]any{
					"id":      tool.ID,
					"name":    tool.Name,
					"status":  tool.Status,
					"created": tool.Created,
				}
				if tool.Arguments != "" {
					toolEntry["arguments"] = tool.Arguments
				}
				toolsPayload = append(toolsPayload, toolEntry)
			}
			entry["tools"] = toolsPayload
		}
		if len(msg.Usage) > 0 {
			entry["usage"] = cloneJSONMap(msg.Usage)
		}
		messages = append(messages, entry)
	}
	recovery["messages"] = messages
	return recovery
}

func (r *responseRun) cancelRun() bool {
	r.mu.Lock()
	if r.status != "in_progress" || r.cancel == nil {
		r.mu.Unlock()
		return false
	}
	cancel := r.cancel
	r.cancel = nil
	r.mu.Unlock()

	cancel()
	return true
}

func (r *responseRun) disableCompaction() {
	r.mu.Lock()
	r.compactionEnabled = false
	r.mu.Unlock()
}

type responseRunManager struct {
	mu                sync.Mutex
	runs              map[string]*responseRun
	activeBySession   map[string]string
	cleanupTimers     map[string]*time.Timer
	terminalRetention time.Duration
	closed            bool
}

const (
	defaultResponseRunRetention        = 5 * time.Minute
	defaultResponseRunReplayLimit      = 256
	defaultResponseRunSubscriberBuffer = 64
	defaultResponseRunTimeout          = 15 * time.Minute
)

func newServeResponseRunManager() *responseRunManager {
	return newServeResponseRunManagerWithRetention(defaultResponseRunRetention)
}

func newServeResponseRunManagerWithRetention(retention time.Duration) *responseRunManager {
	return &responseRunManager{
		runs:              make(map[string]*responseRun),
		activeBySession:   make(map[string]string),
		cleanupTimers:     make(map[string]*time.Timer),
		terminalRetention: retention,
	}
}

func (s *serveServer) ensureResponseRuns() *responseRunManager {
	s.responseRunsOnce.Do(func() {
		if s.responseRuns == nil {
			s.responseRuns = newServeResponseRunManager()
		}
	})
	return s.responseRuns
}

func (m *responseRunManager) create(run *responseRun) error {
	if run == nil || strings.TrimSpace(run.id) == "" {
		return fmt.Errorf("response run id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.runs[run.id]; exists {
		return fmt.Errorf("response run %q already exists", run.id)
	}
	m.runs[run.id] = run
	return nil
}

func (m *responseRunManager) delete(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timer, ok := m.cleanupTimers[id]; ok {
		timer.Stop()
		delete(m.cleanupTimers, id)
	}
	delete(m.runs, id)
	for sessionID, activeID := range m.activeBySession {
		if activeID == id {
			delete(m.activeBySession, sessionID)
		}
	}
}

func (m *responseRunManager) scheduleCleanup(id string) {
	if strings.TrimSpace(id) == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.runs[id]; !ok {
		return
	}
	if timer, ok := m.cleanupTimers[id]; ok {
		timer.Stop()
		delete(m.cleanupTimers, id)
	}

	if m.closed || m.terminalRetention <= 0 {
		delete(m.runs, id)
		for sessionID, activeID := range m.activeBySession {
			if activeID == id {
				delete(m.activeBySession, sessionID)
			}
		}
		return
	}

	m.cleanupTimers[id] = time.AfterFunc(m.terminalRetention, func() {
		m.delete(id)
	})
}

func (m *responseRunManager) get(id string) (*responseRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[id]
	return run, ok
}

func (m *responseRunManager) setActiveRun(sessionID, runID string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeBySession[sessionID] = runID
}

func (m *responseRunManager) clearActiveRun(sessionID, runID string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeBySession[sessionID] == runID {
		delete(m.activeBySession, sessionID)
	}
}

func (m *responseRunManager) activeRunID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeBySession[sessionID]
}

func (m *responseRunManager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	runs := make([]*responseRun, 0, len(m.runs))
	for _, run := range m.runs {
		runs = append(runs, run)
	}
	for id, timer := range m.cleanupTimers {
		timer.Stop()
		delete(m.cleanupTimers, id)
	}
	m.mu.Unlock()

	for _, run := range runs {
		_ = run.cancelRun()
	}
}

func usagePayload(usage llm.Usage) map[string]any {
	return map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  usage.InputTokens + usage.CachedInputTokens + usage.CacheWriteTokens + usage.OutputTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens":      usage.CachedInputTokens,
			"cache_write_tokens": usage.CacheWriteTokens,
		},
	}
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func mapValue(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func stringSliceValue(v any) []string {
	switch values := v.(type) {
	case []string:
		out := make([]string, len(values))
		copy(out, values)
		return out
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func cloneJSONMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(src))
	for key, value := range src {
		cloned[key] = cloneJSONValue(value)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = cloneJSONValue(typed[i])
		}
		return out
	default:
		return typed
	}
}

func writeStoredResponseEvent(w io.Writer, ev responseRunEvent) error {
	if _, err := fmt.Fprintf(w, "id: %d\n", ev.Sequence); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", ev.Event); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", ev.Data)
	return err
}

type responseRunStreamState struct {
	outputIndex int
	toolsSeen   bool
}

func (s *serveServer) appendResponseRunEvent(runtime *serveRuntime, run *responseRun, state *responseRunStreamState, ev llm.Event) error {
	switch ev.Type {
	case llm.EventTextDelta:
		if state.toolsSeen {
			if err := run.appendEvent("response.output_text.new_segment", map[string]any{
				"output_index": state.outputIndex,
			}); err != nil {
				return err
			}
			state.toolsSeen = false
		}
		return run.appendEvent("response.output_text.delta", map[string]any{
			"output_index": state.outputIndex,
			"delta":        ev.Text,
		})
	case llm.EventToolCall:
		if ev.Tool == nil {
			return nil
		}
		state.toolsSeen = true
		item := map[string]any{
			"id":        "fc_" + ev.Tool.ID,
			"type":      "function_call",
			"call_id":   ev.Tool.ID,
			"name":      ev.Tool.Name,
			"arguments": string(ev.Tool.Arguments),
		}
		if err := run.appendEvent("response.output_item.added", map[string]any{
			"output_index": state.outputIndex,
			"item":         item,
		}); err != nil {
			return err
		}
		if err := run.appendEvent("response.function_call_arguments.delta", map[string]any{
			"output_index": state.outputIndex,
			"delta":        string(ev.Tool.Arguments),
		}); err != nil {
			return err
		}
		if err := run.appendEvent("response.output_item.done", map[string]any{
			"output_index": state.outputIndex,
			"item":         item,
		}); err != nil {
			return err
		}
		state.outputIndex++
		return nil
	case llm.EventToolExecStart:
		if ev.ToolName == tools.AskUserToolName {
			if prompt, err := runtime.prepareAskUserFromToolArgs(ev.ToolCallID, ev.ToolArgs); err == nil {
				if err := run.appendEvent("response.ask_user.prompt", map[string]any{
					"call_id":    prompt.CallID,
					"questions":  prompt.Questions,
					"created_at": prompt.CreatedAt,
				}); err != nil {
					return err
				}
			}
		}
		return run.appendEvent("response.tool_exec.start", map[string]any{
			"call_id":        ev.ToolCallID,
			"tool_name":      ev.ToolName,
			"tool_info":      ev.ToolInfo,
			"tool_arguments": string(ev.ToolArgs),
		})
	case llm.EventToolExecEnd:
		if ev.ToolName == tools.AskUserToolName {
			runtime.clearPendingAskUser(ev.ToolCallID)
		}
		payload := map[string]any{
			"call_id":   ev.ToolCallID,
			"tool_name": ev.ToolName,
			"success":   ev.ToolSuccess,
		}
		if len(ev.ToolImages) > 0 {
			imageURLs := make([]string, 0, len(ev.ToolImages))
			for _, imgPath := range ev.ToolImages {
				if served, ok := s.ensureImageServeable(imgPath); ok {
					imageURLs = append(imageURLs, s.cfg.imagesRoute()+filepath.Base(served))
				}
			}
			if len(imageURLs) > 0 {
				payload["images"] = imageURLs
			}
		}
		return run.appendEvent("response.tool_exec.end", payload)
	case llm.EventHeartbeat:
		return run.appendEvent("response.heartbeat", map[string]any{
			"call_id":   ev.ToolCallID,
			"tool_name": ev.ToolName,
		})
	case llm.EventInterjection:
		return run.appendEvent("response.interjection", map[string]any{
			"text": ev.Text,
		})
	default:
		return nil
	}
}

func (s *serveServer) handleResponseByID(w http.ResponseWriter, r *http.Request) {
	mgr := s.ensureResponseRuns()

	path := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	runID := parts[0]
	run, ok := mgr.get(runID)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "response not found")
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, run.snapshot())
		return
	}

	if len(parts) == 2 && parts[1] == "events" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		after, err := parseNonNegativeIntQuery(r, "after", 0)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		s.streamResponseRunEvents(r.Context(), w, run, int64(after))
		return
	}

	if len(parts) == 2 && parts[1] == "cancel" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		if !run.cancelRun() {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "response is not running")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":     runID,
			"object": "response.cancel",
			"status": "cancelling",
		})
		return
	}

	http.NotFound(w, r)
}

func (s *serveServer) streamResponseRunEvents(ctx context.Context, w http.ResponseWriter, run *responseRun, after int64) {
	subscription := run.subscribe(after)
	if subscription.snapshotRequired {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"type":    "conflict_error",
				"message": "response replay no longer available; fetch the response snapshot and resume from its sequence number",
			},
			"snapshot_required": true,
			"min_replay_after":  subscription.minReplayAfter,
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming not supported")
		return
	}

	setSSEHeaders(w)
	replay := subscription.replay
	ch := subscription.ch

	pingMu, stopPing := sseKeepalive(w, flusher, 20*time.Second)
	defer stopPing()
	if ch != nil {
		defer run.unsubscribe(ch)
	}

	writeEvent := func(ev responseRunEvent) error {
		pingMu.Lock()
		defer pingMu.Unlock()
		if err := writeStoredResponseEvent(w, ev); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	for _, ev := range replay {
		if err := writeEvent(ev); err != nil {
			return
		}
	}

	if ch == nil {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if err := writeEvent(ev); err != nil {
				return
			}
		}
	}
}

func (s *serveServer) startResponseRun(runtime *serveRuntime, stateful bool, replaceHistory bool, inputMessages []llm.Message, llmReq llm.Request, sessionID string, options startResponseRunOptions) (*responseRun, error) {
	mgr := s.ensureResponseRuns()

	respID := "resp_" + randomSuffix()
	model := llmReq.Model
	if model == "" {
		model = runtime.defaultModel
	}
	created := time.Now().Unix()

	runCtx, cancel := context.WithTimeout(context.Background(), defaultResponseRunTimeout)
	run := newResponseRun(respID, sessionID, options.previousResponseID, model, created, cancel)
	if err := mgr.create(run); err != nil {
		cancel()
		return nil, err
	}
	mgr.setActiveRun(sessionID, respID)

	s.registerResponseID(runtime, respID, sessionID)
	if options.uiSession {
		runtime.clearLastUIRunError()
	}

	if err := run.appendEvent("response.created", map[string]any{
		"response": map[string]any{
			"id":      respID,
			"object":  "response",
			"created": created,
			"model":   model,
			"status":  "in_progress",
		},
	}); err != nil {
		cancel()
		mgr.clearActiveRun(sessionID, respID)
		mgr.delete(respID)
		return nil, err
	}

	go func() {
		defer func() {
			mgr.clearActiveRun(sessionID, respID)
			mgr.scheduleCleanup(respID)
		}()
		if !stateful {
			defer runtime.Close()
		}

		// Wire approval event callback so PromptUIFunc can emit SSE events
		runtime.approvalMu.Lock()
		runtime.approvalEventFunc = func(event string, data map[string]any) error {
			return run.appendEvent(event, data)
		}
		runtime.approvalCtx = runCtx
		runtime.approvalMu.Unlock()
		defer func() {
			runtime.approvalMu.Lock()
			runtime.approvalEventFunc = nil
			runtime.approvalCtx = nil
			runtime.approvalMu.Unlock()
		}()

		streamState := &responseRunStreamState{}
		result, err := runtime.RunWithEvents(runCtx, stateful, replaceHistory, inputMessages, llmReq, func(ev llm.Event) error {
			return s.appendResponseRunEvent(runtime, run, streamState, ev)
		})
		if err != nil {
			errType := "invalid_request_error"
			if errors.Is(err, errServeSessionBusy) {
				errType = "conflict_error"
			}
			hadSubscribers, failErr := run.fail(map[string]any{
				"error": map[string]any{
					"message": err.Error(),
					"type":    errType,
				},
			}, errType, err.Error())
			if options.uiSession {
				switch {
				case hadSubscribers:
					runtime.clearLastUIRunError()
				case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
					runtime.clearLastUIRunError()
				default:
					runtime.setLastUIRunError(err.Error())
				}
			}
			if failErr != nil {
				log.Printf("response run %s failed to append terminal event: %v", respID, failErr)
			}
			if failErr != nil && options.uiSession {
				runtime.setLastUIRunError(err.Error())
			}
			return
		}

		if options.uiSession {
			runtime.clearLastUIRunError()
		}
		if err := run.complete(map[string]any{
			"response": map[string]any{
				"id":            respID,
				"object":        "response",
				"created":       created,
				"model":         model,
				"status":        "completed",
				"usage":         usagePayload(result.Usage),
				"session_usage": usagePayload(result.SessionUsage),
			},
		}, result.Usage, result.SessionUsage); err != nil {
			log.Printf("response run %s failed to append completion event: %v", respID, err)
		}
	}()

	return run, nil
}
