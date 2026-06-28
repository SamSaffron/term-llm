package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	Images    []string
}

type responseRunRecoveryMessage struct {
	ID             string
	Role           string
	Content        []byte
	Created        int64
	Tools          []responseRunRecoveryTool
	Attachments    []map[string]any
	Expanded       bool
	Status         string
	Usage          map[string]any
	InterruptState string
}

type responseRunRecoveryEvent struct {
	Event   string
	Payload map[string]any
}

type responseRunSubscribeResult struct {
	id               int
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
	reasoningEffort    string
	reasoningEffortSet bool
	created            int64
	status             string
	errorType          string
	errorMessage       string
	usage              llm.Usage
	sessionUsage       llm.Usage
	lastSequenceNumber int64
	// events[eventStart:] is the retained replay window; dropped prefix slots
	// are zeroed and reclaimed in batches to avoid per-token slice copies.
	events             []responseRunEvent
	eventStart         int
	minReplayAfter     int64
	maxRetainedEvents  int
	recoveryMessages   []responseRunRecoveryMessage
	recoveryEvents     []responseRunRecoveryEvent
	nextMessageOrdinal int64
	currentAssistant   int
	currentToolGroup   int
	compactionEnabled  bool
	subscribers        map[int]chan responseRunEvent
	subscriberWarned   map[int]bool // tracks whether 75% buffer warning was logged
	subscriberDropped  map[int]bool // tracks subscribers dropped after their live buffer overflowed
	nextSubscriberID   int
	cancel             context.CancelFunc
	cancelRequested    bool
}

type startResponseRunOptions struct {
	previousResponseID        string
	uiSession                 bool
	resetResponseIDsOnSuccess bool
	modelSwap                 *responseModelSwapExecution
	idempotencyKey            string
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
		subscriberWarned:   make(map[int]bool),
		subscriberDropped:  make(map[int]bool),
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
	if r.cancelRequested {
		r.status = "cancelled"
		r.errorType = ""
		r.errorMessage = ""
		r.cancel = nil
		r.cancelRequested = false
		if response := mapValue(payload["response"]); len(response) > 0 {
			response["status"] = "cancelled"
			delete(response, "usage")
			delete(response, "session_usage")
		}
		return r.appendEventLocked("response.cancelled", payload, true)
	}
	r.status = "completed"
	r.errorType = ""
	r.errorMessage = ""
	r.cancel = nil
	r.cancelRequested = false
	r.usage = usage
	r.sessionUsage = sessionUsage
	return r.appendEventLocked("response.completed", payload, true)
}

func (r *responseRun) finishCancelled(payload map[string]any) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.cancelRequested {
		return false, nil
	}
	r.status = "cancelled"
	r.errorType = ""
	r.errorMessage = ""
	r.cancel = nil
	r.cancelRequested = false
	return true, r.appendEventLocked("response.cancelled", payload, true)
}

func (r *responseRun) fail(payload map[string]any, errType, errMessage string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hadSubscribers := len(r.subscribers) > 0
	r.status = "failed"
	r.errorType = errType
	r.errorMessage = errMessage
	r.cancel = nil
	r.cancelRequested = false
	return hadSubscribers, r.appendEventLocked("response.failed", payload, true)
}

func (r *responseRun) applyRuntimeMetadataLocked(event string, payload map[string]any) {
	var source map[string]any
	switch event {
	case "response.created", "response.completed", "response.cancelled":
		source = mapValue(payload["response"])
	case "response.model_switch":
		source = payload
	default:
		return
	}
	if len(source) == 0 {
		return
	}
	if model := stringValue(source["model"]); model != "" {
		r.model = model
	}
	if _, ok := source["reasoning_effort"]; ok {
		r.reasoningEffort = stringValue(source["reasoning_effort"])
		r.reasoningEffortSet = true
	}
}

func (r *responseRun) appendEventLocked(event string, payload map[string]any, terminal bool) error {
	if payload == nil {
		payload = map[string]any{}
	}
	r.lastSequenceNumber++
	payload["sequence_number"] = r.lastSequenceNumber
	r.applyRuntimeMetadataLocked(event, payload)

	data, err := json.Marshal(payload)
	if err != nil {
		r.lastSequenceNumber--
		delete(payload, "sequence_number")
		return err
	}

	r.applyRecoveryEventLocked(event, payload)
	r.storeEventLocked(responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    event,
		Data:     data,
	}, terminal)
	return nil
}

// storeEventLocked appends stored to r.events, compacts the buffer, and fans
// out to all live subscribers. Must be called with r.mu held.
func (r *responseRun) storeEventLocked(stored responseRunEvent, terminal bool) {
	r.events = append(r.events, stored)
	r.compactEventsLocked()

	// Fan out to subscribers under the lock to guarantee event ordering.
	// Non-blocking send: the 256-event buffer provides ample headroom.
	// A subscriber that can't accept is truly stalled and gets dropped immediately.
	for id, ch := range r.subscribers {
		select {
		case ch <- stored:
			fill := len(ch)
			threshold := cap(ch) * 3 / 4
			if fill > threshold && !r.subscriberWarned[id] {
				log.Printf("response run %s subscriber %d buffer at %d/%d", r.id, id, fill, cap(ch))
				r.subscriberWarned[id] = true
			} else if fill <= threshold/2 && r.subscriberWarned[id] {
				r.subscriberWarned[id] = false
			}
		default:
			log.Printf("response run %s subscriber fell behind at sequence %d; closing stream", r.id, stored.Sequence)
			r.subscriberDropped[id] = true
			close(ch)
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
		}
	}

	if terminal {
		for id, ch := range r.subscribers {
			close(ch)
			delete(r.subscribers, id)
			delete(r.subscriberWarned, id)
		}
	}
}

func encodeTextDeltaPayload(outputIndex int, delta string, sequenceNumber int64) ([]byte, error) {
	data := make([]byte, 0, 80+len(delta))
	data = append(data, `{"output_index":`...)
	data = strconv.AppendInt(data, int64(outputIndex), 10)
	data = append(data, `,"delta":`...)
	if utf8.ValidString(delta) {
		data = appendJSONString(data, delta)
	} else {
		encoded, err := json.Marshal(delta)
		if err != nil {
			return nil, err
		}
		data = append(data, encoded...)
	}
	data = append(data, `,"sequence_number":`...)
	data = strconv.AppendInt(data, sequenceNumber, 10)
	data = append(data, '}')
	return data, nil
}

// appendTextDeltaEvent is a fast path for response.output_text.delta that avoids
// allocating a map[string]any or a typed payload on every streamed token.
func (r *responseRun) appendTextDeltaEvent(outputIndex int, delta string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSequenceNumber++

	data, err := encodeTextDeltaPayload(outputIndex, delta, r.lastSequenceNumber)
	if err != nil {
		r.lastSequenceNumber--
		return err
	}

	if delta != "" {
		r.closeToolGroupLocked()
		idx := r.ensureAssistantMessageLocked()
		r.recoveryMessages[idx].Content = append(r.recoveryMessages[idx].Content, delta...)
	}

	r.storeEventLocked(responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    "response.output_text.delta",
		Data:     data,
	}, false)
	return nil
}

func (r *responseRun) compactEventsLocked() {
	if !r.compactionEnabled || r.maxRetainedEvents <= 0 {
		return
	}

	activeLen := len(r.events) - r.eventStart
	if activeLen <= r.maxRetainedEvents {
		return
	}

	dropCount := activeLen - r.maxRetainedEvents
	firstKept := r.eventStart + dropCount

	nextReplayAfter := r.events[firstKept].Sequence - 1
	if nextReplayAfter > r.minReplayAfter {
		r.minReplayAfter = nextReplayAfter
	}

	for i := r.eventStart; i < firstKept; i++ {
		r.events[i] = responseRunEvent{}
	}
	r.eventStart = firstKept
	r.compactEventStorageLocked()
}

// compactEventStorageLocked reclaims the dropped prefix in batches so steady
// streaming appends avoid copying the replay window on every token while still
// keeping the backing array bounded to roughly twice maxRetainedEvents.
func (r *responseRun) compactEventStorageLocked() {
	if r.eventStart == 0 {
		return
	}
	if r.maxRetainedEvents > 0 && r.eventStart < r.maxRetainedEvents {
		return
	}

	activeLen := len(r.events) - r.eventStart
	copy(r.events, r.events[r.eventStart:])
	tail := r.events[activeLen:]
	for i := range tail {
		tail[i] = responseRunEvent{}
	}
	r.events = r.events[:activeLen]
	r.eventStart = 0
}

func (r *responseRun) activeEventsLocked() []responseRunEvent {
	return r.events[r.eventStart:]
}

func (r *responseRun) applyRecoveryEventLocked(event string, payload map[string]any) {
	switch event {
	case "response.ask_user.prompt", "response.approval.prompt":
		r.recoveryEvents = append(r.recoveryEvents, responseRunRecoveryEvent{
			Event:   event,
			Payload: cloneJSONMap(payload),
		})
	case "response.interjection":
		text := stringValue(payload["text"])
		if text == "" {
			return
		}
		r.closeToolGroupLocked()
		r.currentAssistant = -1
		id := stringValue(payload["interjection_id"])
		if id == "" {
			id = r.nextRecoveryMessageIDLocked("user")
		}
		r.recoveryMessages = append(r.recoveryMessages, responseRunRecoveryMessage{
			ID:             id,
			Role:           "user",
			Content:        []byte(text),
			Created:        time.Now().UnixMilli(),
			Attachments:    attachmentsFromPayload(payload["attachments"]),
			InterruptState: "interject",
		})
		return
	}

	switch event {
	case "response.output_text.delta":
		delta := stringValue(payload["delta"])
		if delta == "" {
			return
		}
		r.closeToolGroupLocked()
		idx := r.ensureAssistantMessageLocked()
		r.recoveryMessages[idx].Content = append(r.recoveryMessages[idx].Content, delta...)
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
		images := stringSliceValue(payload["images"])
		if r.currentToolGroup >= 0 && r.currentToolGroup < len(r.recoveryMessages) {
			group := &r.recoveryMessages[r.currentToolGroup]
			callID := stringValue(payload["call_id"])
			for i := range group.Tools {
				if callID == "" || group.Tools[i].ID == callID {
					group.Tools[i].Status = "done"
					group.Tools[i].Images = appendUniqueStrings(group.Tools[i].Images, images...)
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
	case "response.cancelled":
		r.closeToolGroupLocked()
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
			Content: []byte(message),
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

	replayEvents := r.activeEventsLocked()
	replay := make([]responseRunEvent, 0, len(replayEvents))
	for _, ev := range replayEvents {
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
	return responseRunSubscribeResult{id: id, replay: replay, ch: ch}
}

func (r *responseRun) subscriberWasDropped(id int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.subscriberDropped[id] {
		return false
	}
	delete(r.subscriberDropped, id)
	return true
}

func (r *responseRun) droppedSubscriberTerminalEvent() (responseRunEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	payload := map[string]any{
		"error": map[string]any{
			"type":    "stream_buffer_overflow",
			"message": "response event stream subscriber fell behind; reconnect using the recovery payload to resume",
		},
		"sequence_number":  r.lastSequenceNumber,
		"min_replay_after": r.minReplayAfter,
		"recovery":         r.recoveryPayloadLocked(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return responseRunEvent{}, err
	}
	return responseRunEvent{
		Sequence: r.lastSequenceNumber,
		Event:    "response.stream_error",
		Data:     data,
	}, nil
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
			delete(r.subscriberWarned, id)
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
	if r.reasoningEffortSet {
		payload["reasoning_effort"] = r.reasoningEffort
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
	if len(r.recoveryMessages) == 0 && len(r.recoveryEvents) == 0 {
		return recovery
	}

	messages := make([]map[string]any, 0, len(r.recoveryMessages))
	for _, msg := range r.recoveryMessages {
		entry := map[string]any{
			"id":      msg.ID,
			"role":    msg.Role,
			"created": msg.Created,
		}
		if len(msg.Content) > 0 {
			entry["content"] = string(msg.Content)
		}
		if msg.Status != "" {
			entry["status"] = msg.Status
		}
		if msg.InterruptState != "" {
			entry["interruptState"] = msg.InterruptState
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
				if len(tool.Images) > 0 {
					images := make([]string, len(tool.Images))
					copy(images, tool.Images)
					toolEntry["images"] = images
				}
				toolsPayload = append(toolsPayload, toolEntry)
			}
			entry["tools"] = toolsPayload
		}
		if len(msg.Attachments) > 0 {
			atts := make([]map[string]any, 0, len(msg.Attachments))
			for _, att := range msg.Attachments {
				atts = append(atts, cloneJSONMap(att))
			}
			entry["attachments"] = atts
		}
		if len(msg.Usage) > 0 {
			entry["usage"] = cloneJSONMap(msg.Usage)
		}
		messages = append(messages, entry)
	}
	recovery["messages"] = messages
	if len(r.recoveryEvents) > 0 {
		events := make([]map[string]any, 0, len(r.recoveryEvents))
		for _, ev := range r.recoveryEvents {
			entry := map[string]any{"event": ev.Event}
			if payload := cloneJSONMap(ev.Payload); len(payload) > 0 {
				entry["payload"] = payload
			}
			events = append(events, entry)
		}
		recovery["events"] = events
	}
	return recovery
}

func (r *responseRun) cancelRun() bool {
	r.mu.Lock()
	if r.status != "in_progress" {
		r.mu.Unlock()
		return false
	}
	cancel := r.cancel
	if cancel == nil && !r.cancelRequested {
		r.mu.Unlock()
		return false
	}
	r.cancelRequested = true
	r.cancel = nil
	r.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return true
}

func responseRunRecoveryEventMatches(ev responseRunRecoveryEvent, event, key, value string) bool {
	if ev.Event != event || key == "" || value == "" {
		return false
	}
	return stringValue(ev.Payload[key]) == value
}

func (r *responseRun) resolveRecoveryEvent(event, key, value string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.recoveryEvents) == 0 {
		return
	}
	kept := r.recoveryEvents[:0]
	for _, ev := range r.recoveryEvents {
		if responseRunRecoveryEventMatches(ev, event, key, value) {
			continue
		}
		kept = append(kept, ev)
	}
	for i := len(kept); i < len(r.recoveryEvents); i++ {
		r.recoveryEvents[i] = responseRunRecoveryEvent{}
	}
	r.recoveryEvents = kept
}

func (r *responseRun) resolveAskUserRecovery(callID string) {
	r.resolveRecoveryEvent("response.ask_user.prompt", "call_id", strings.TrimSpace(callID))
}

func (r *responseRun) resolveApprovalRecovery(approvalID string) {
	r.resolveRecoveryEvent("response.approval.prompt", "approval_id", strings.TrimSpace(approvalID))
}

type responseRunManager struct {
	mu                sync.Mutex
	runs              map[string]*responseRun
	activeBySession   map[string]string
	idempotencyByKey  map[string]string
	cleanupTimers     map[string]*time.Timer
	terminalRetention time.Duration
	runWG             sync.WaitGroup
	closed            bool
}

const (
	defaultResponseRunRetention        = 5 * time.Minute
	defaultResponseRunReplayLimit      = 2048
	defaultResponseRunSubscriberBuffer = 256
	defaultServeRequestTimeout         = 30 * time.Minute
)

func responseRunTimeoutMessage(timeout time.Duration) string {
	return fmt.Sprintf("Response run timed out after %s. Continue to resume from saved progress, or move long-running investigations to a background job.", humanDuration(timeout))
}

func humanDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		hours := int(d / time.Hour)
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}
	if d%time.Minute == 0 {
		minutes := int(d / time.Minute)
		if minutes == 1 {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", minutes)
	}
	return d.String()
}

func newServeResponseRunManager() *responseRunManager {
	return newServeResponseRunManagerWithRetention(defaultResponseRunRetention)
}

func newServeResponseRunManagerWithRetention(retention time.Duration) *responseRunManager {
	return &responseRunManager{
		runs:              make(map[string]*responseRun),
		activeBySession:   make(map[string]string),
		idempotencyByKey:  make(map[string]string),
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

func responseRunIdempotencyScope(sessionID, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return key
	}
	return sessionID + "\x00" + key
}

func (m *responseRunManager) create(run *responseRun) error {
	_, duplicate, err := m.createOrGetByIdempotency(run, "")
	if duplicate {
		return fmt.Errorf("response run %q already exists", run.id)
	}
	return err
}

func (m *responseRunManager) createOrGetByIdempotency(run *responseRun, idempotencyKey string) (*responseRun, bool, error) {
	if run == nil || strings.TrimSpace(run.id) == "" {
		return nil, false, fmt.Errorf("response run id is required")
	}
	key := responseRunIdempotencyScope(run.sessionID, idempotencyKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, fmt.Errorf("server is shutting down")
	}
	if key != "" {
		if existingID := strings.TrimSpace(m.idempotencyByKey[key]); existingID != "" {
			if existing, ok := m.runs[existingID]; ok && existing != nil {
				return existing, true, nil
			}
			delete(m.idempotencyByKey, key)
		}
	}
	if _, exists := m.runs[run.id]; exists {
		return nil, false, fmt.Errorf("response run %q already exists", run.id)
	}
	m.runs[run.id] = run
	if key != "" {
		m.idempotencyByKey[key] = run.id
	}
	return run, false, nil
}

func (m *responseRunManager) getByIdempotencyKey(sessionID, idempotencyKey string) (*responseRun, bool) {
	key := responseRunIdempotencyScope(sessionID, idempotencyKey)
	if key == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	runID := strings.TrimSpace(m.idempotencyByKey[key])
	if runID == "" {
		return nil, false
	}
	run, ok := m.runs[runID]
	if !ok || run == nil {
		delete(m.idempotencyByKey, key)
		return nil, false
	}
	return run, true
}

func (m *responseRunManager) start(fn func()) error {
	if fn == nil {
		return fmt.Errorf("response run function is required")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("server is shutting down")
	}
	m.runWG.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.runWG.Done()
		fn()
	}()
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
	for key, runID := range m.idempotencyByKey {
		if runID == id {
			delete(m.idempotencyByKey, key)
		}
	}
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
		for key, runID := range m.idempotencyByKey {
			if runID == id {
				delete(m.idempotencyByKey, key)
			}
		}
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

// ActiveSessionIDs returns session IDs that currently have an active
// response run. Does not touch any runtime TTLs.
func (m *responseRunManager) ActiveSessionIDs() map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]bool, len(m.activeBySession))
	for sid := range m.activeBySession {
		result[sid] = true
	}
	return result
}

func (m *responseRunManager) Close() {
	m.CloseContext(context.Background())
}

func (m *responseRunManager) CloseContext(ctx context.Context) {
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
	waitDone := make(chan struct{})
	go func() {
		m.runWG.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
	case <-ctx.Done():
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

// appendJSONString appends a JSON-encoded string to dst without allocating a
// separate []byte (unlike json.Marshal). Handles all characters that require
// escaping in JSON strings; non-ASCII UTF-8 bytes pass through unchanged.
func appendJSONString(dst []byte, s string) []byte {
	const hexChars = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b >= 0x20 && b != '"' && b != '\\' {
			continue
		}
		dst = append(dst, s[start:i]...)
		start = i + 1
		switch b {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hexChars[b>>4], hexChars[b&0xf])
		}
	}
	dst = append(dst, s[start:]...)
	dst = append(dst, '"')
	return dst
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

func appendUniqueStrings(dst []string, values ...string) []string {
	for _, value := range values {
		if value == "" {
			continue
		}
		seen := false
		for _, existing := range dst {
			if existing == value {
				seen = true
				break
			}
		}
		if !seen {
			dst = append(dst, value)
		}
	}
	return dst
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
	b := make([]byte, 0, 4+20+1+7+len(ev.Event)+1+6+len(ev.Data)+2)
	b = append(b, "id: "...)
	b = strconv.AppendInt(b, ev.Sequence, 10)
	b = append(b, "\nevent: "...)
	b = append(b, ev.Event...)
	b = append(b, "\ndata: "...)
	b = append(b, ev.Data...)
	b = append(b, "\n\n"...)
	_, err := w.Write(b)
	return err
}

type responseRunStreamState struct {
	outputIndex        int
	toolsSeen          bool
	model              string
	reasoningEffort    string
	reasoningEffortSet bool
}

func newResponseRunStreamState(model, reasoningEffort string) *responseRunStreamState {
	effort := strings.TrimSpace(reasoningEffort)
	return &responseRunStreamState{
		model:              strings.TrimSpace(model),
		reasoningEffort:    effort,
		reasoningEffortSet: effort != "",
	}
}

func (s *responseRunStreamState) appliedModel(fallback string) string {
	if s != nil && strings.TrimSpace(s.model) != "" {
		return strings.TrimSpace(s.model)
	}
	return strings.TrimSpace(fallback)
}

func (s *responseRunStreamState) appliedReasoningEffort(fallback string) (string, bool) {
	if s != nil && s.reasoningEffortSet {
		return strings.TrimSpace(s.reasoningEffort), true
	}
	fallback = strings.TrimSpace(fallback)
	return fallback, fallback != ""
}

func (s *serveServer) toolImageURLs(imagePaths []string) []string {
	if len(imagePaths) == 0 {
		return nil
	}
	imageURLs := make([]string, 0, len(imagePaths))
	for _, imgPath := range imagePaths {
		if s.cfg.filesDir != "" {
			if served, ok := s.ensureFileServeable(imgPath); ok {
				imageURLs = append(imageURLs, serveRoutePath(s.cfg.filesRoute(), s.cfg.filesDir, served))
			}
			continue
		}
		if served, ok := s.ensureImageServeable(imgPath); ok {
			imageURLs = append(imageURLs, serveRoutePath(s.cfg.imagesRoute(), s.imageOutputDir(), served))
		}
	}
	return imageURLs
}

func (s *serveServer) suppressResponseRunServerToolEvent(runtime *serveRuntime, toolName string) bool {
	return s != nil && s.cfg.suppressServerTools && runtime != nil && runtime.isServerExecutedTool(toolName)
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
		return run.appendTextDeltaEvent(state.outputIndex, ev.Text)
	case llm.EventAttemptDiscard:
		state.toolsSeen = false
		return run.appendEvent("response.attempt.discard", map[string]any{
			"output_index": state.outputIndex,
		})
	case llm.EventToolCall:
		if ev.Tool == nil {
			return nil
		}
		// Suppress tool calls for server-executed tools in API mode
		if s.suppressResponseRunServerToolEvent(runtime, ev.Tool.Name) {
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
		if s.suppressResponseRunServerToolEvent(runtime, ev.ToolName) {
			return nil
		}
		if ev.ToolName == tools.AskUserToolName && runtime != nil {
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
		if ev.ToolName == tools.AskUserToolName && runtime != nil {
			runtime.clearPendingAskUser(ev.ToolCallID)
		}
		if s.suppressResponseRunServerToolEvent(runtime, ev.ToolName) {
			return nil
		}
		payload := map[string]any{
			"call_id":   ev.ToolCallID,
			"tool_name": ev.ToolName,
			"success":   ev.ToolSuccess,
		}
		if len(ev.ToolImages) > 0 {
			if imageURLs := s.toolImageURLs(ev.ToolImages); len(imageURLs) > 0 {
				payload["images"] = imageURLs
			}
		}
		if err := run.appendEvent("response.tool_exec.end", payload); err != nil {
			return err
		}
		// Metadata only — diff content is served by the session
		// file-changes endpoints on demand.
		for _, fc := range ev.ToolFileChanges {
			if err := run.appendEvent("response.file_change", map[string]any{
				"path":         fc.Path,
				"kind":         fc.Kind,
				"adds":         fc.Adds,
				"dels":         fc.Dels,
				"seq":          fc.Seq,
				"truncated":    fc.Truncated,
				"tool_call_id": ev.ToolCallID,
			}); err != nil {
				return err
			}
		}
		return nil
	case llm.EventHeartbeat:
		return run.appendEvent("response.heartbeat", map[string]any{
			"call_id":   ev.ToolCallID,
			"tool_name": ev.ToolName,
		})
	case llm.EventPhase:
		if ev.Text == "" {
			return nil
		}
		return run.appendEvent("response.phase", map[string]any{
			"text": ev.Text,
		})
	case llm.EventInterjection:
		payload := map[string]any{
			"text": ev.Text,
		}
		if ev.InterjectionID != "" {
			payload["interjection_id"] = ev.InterjectionID
		}
		if ev.InterjectionStatus != "" {
			payload["status"] = string(ev.InterjectionStatus)
		}
		if atts := interjectionAttachmentsForEvent(ev.Message); len(atts) > 0 {
			payload["attachments"] = atts
		}
		return run.appendEvent("response.interjection", payload)
	case llm.EventModelSwitch:
		model := strings.TrimSpace(ev.Model)
		if model == "" {
			model = strings.TrimSpace(ev.Text)
		}
		if model == "" {
			return nil
		}
		effort := strings.TrimSpace(ev.ReasoningEffort)
		if state != nil {
			state.model = model
			state.reasoningEffort = effort
			state.reasoningEffortSet = true
		}
		return run.appendEvent("response.model_switch", map[string]any{
			"model":            model,
			"reasoning_effort": effort,
		})
	default:
		return nil
	}
}

func attachmentsFromPayload(v any) []map[string]any {
	switch items := v.(type) {
	case []map[string]any:
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if len(item) > 0 {
				out = append(out, cloneJSONMap(item))
			}
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if m := mapValue(item); len(m) > 0 {
				out = append(out, cloneJSONMap(m))
			}
		}
		return out
	default:
		return nil
	}
}

func interjectionAttachmentsForEvent(msg llm.Message) []map[string]any {
	var out []map[string]any
	imageCount := 0
	for _, part := range msg.Parts {
		if part.Type != llm.PartImage {
			continue
		}
		imageCount++
		mediaType := "image"
		if part.ImageData != nil && part.ImageData.MediaType != "" {
			mediaType = part.ImageData.MediaType
		}
		out = append(out, map[string]any{
			"name": fmt.Sprintf("image %d", imageCount),
			"type": mediaType,
		})
	}
	return out
}

func (s *serveServer) storeCompletedResponseRun(runtime *serveRuntime, sessionID, previousResponseID, model string, created int64, result serveRunResult, resetResponseIDsOnSuccess bool) (string, error) {
	mgr := s.ensureResponseRuns()

	respID := "resp_" + randomSuffix()
	run := newResponseRun(respID, sessionID, previousResponseID, model, created, nil)
	if err := mgr.create(run); err != nil {
		return "", err
	}

	cleanup := func() {
		mgr.delete(respID)
	}
	createdResponse := map[string]any{
		"id":      respID,
		"object":  "response",
		"created": created,
		"model":   model,
		"status":  "in_progress",
	}
	if err := run.appendEvent("response.created", map[string]any{
		"response": createdResponse,
	}); err != nil {
		cleanup()
		return "", err
	}
	if result.Text.Len() > 0 {
		if err := run.appendEvent("response.output_text.delta", map[string]any{
			"output_index": 0,
			"delta":        result.Text.String(),
		}); err != nil {
			cleanup()
			return "", err
		}
	}
	durableID := s.latestDurableResponseIDForSessionBestEffort(context.Background(), sessionID)
	completedID := respID
	if durableID != "" {
		completedID = durableID
	}
	if err := run.complete(map[string]any{
		"response": map[string]any{
			"id":            completedID,
			"object":        "response",
			"created":       created,
			"model":         model,
			"status":        "completed",
			"usage":         usagePayload(result.Usage),
			"session_usage": usagePayload(result.SessionUsage),
		},
	}, result.Usage, result.SessionUsage); err != nil {
		cleanup()
		return "", err
	}

	mgr.scheduleCleanup(respID)
	if resetResponseIDsOnSuccess {
		s.unregisterSessionResponseIDs(sessionID)
	}
	if completedID != respID {
		s.registerResponseID(runtime, respID, sessionID)
	}
	s.registerResponseID(runtime, completedID, sessionID)
	return completedID, nil
}

func (s *serveServer) streamFailedResponseRun(ctx context.Context, w http.ResponseWriter, sessionID, previousResponseID, model, errType, errMessage string) {
	mgr := s.ensureResponseRuns()

	respID := "resp_" + randomSuffix()
	created := time.Now().Unix()
	run := newResponseRun(respID, sessionID, previousResponseID, model, created, nil)
	if err := mgr.create(run); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	cleanup := func() {
		mgr.delete(respID)
	}
	createdResponse := map[string]any{
		"id":      respID,
		"object":  "response",
		"created": created,
		"model":   model,
		"status":  "in_progress",
	}
	if err := run.appendEvent("response.created", map[string]any{
		"response": createdResponse,
	}); err != nil {
		cleanup()
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	if _, err := run.fail(map[string]any{
		"error": map[string]any{
			"message": errMessage,
			"type":    errType,
		},
	}, errType, errMessage); err != nil {
		cleanup()
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	mgr.scheduleCleanup(respID)
	s.streamResponseRunEvents(ctx, w, run, 0)
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
	w = newStreamingResponseWriter(w, serveStreamWriteTimeout)
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
	flusher.Flush()
	replay := subscription.replay
	ch := subscription.ch
	subscriberID := subscription.id

	pingMu, stopPing := sseKeepalive(ctx, w, flusher, 10*time.Second)
	var stopPingOnce sync.Once
	stopKeepalive := func() {
		stopPingOnce.Do(stopPing)
	}
	defer stopKeepalive()
	if ch != nil {
		defer run.unsubscribe(ch)
	}

	writeDone := func() {
		stopKeepalive()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}

	writeDroppedStreamError := func() {
		ev, err := run.droppedSubscriberTerminalEvent()
		if err != nil {
			return
		}
		pingMu.Lock()
		writeErr := writeStoredResponseEvent(w, ev)
		flusher.Flush()
		pingMu.Unlock()
		if writeErr != nil {
			return
		}
		writeDone()
	}

	if len(replay) > 0 {
		pingMu.Lock()
		var replayErr error
		for _, ev := range replay {
			if replayErr = writeStoredResponseEvent(w, ev); replayErr != nil {
				break
			}
		}
		flusher.Flush()
		pingMu.Unlock()
		if replayErr != nil {
			return
		}
	}

	if ch == nil {
		writeDone()
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shutdownCh:
			return
		case ev, ok := <-ch:
			if !ok {
				if run.subscriberWasDropped(subscriberID) {
					writeDroppedStreamError()
					return
				}
				writeDone()
				return
			}
			// Drain any immediately available events and write them as a
			// batch under a single lock+Flush to cut syscall overhead at
			// high token rates (~100 events/sec during streaming).
			pingMu.Lock()
			closed := false
			writeErr := writeStoredResponseEvent(w, ev)
		drainLoop:
			for writeErr == nil {
				select {
				case next, nextOK := <-ch:
					if !nextOK {
						closed = true
						break drainLoop
					}
					writeErr = writeStoredResponseEvent(w, next)
				default:
					break drainLoop
				}
			}
			flusher.Flush()
			pingMu.Unlock()
			if writeErr != nil {
				return
			}
			if closed {
				if run.subscriberWasDropped(subscriberID) {
					writeDroppedStreamError()
					return
				}
				writeDone()
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

	// Intentionally detached from the HTTP request context. Runs must survive
	// client disconnects so that:
	//  - SSE connections are fragile (network blips, mobile tab switches, etc.);
	//    killing a run on disconnect would waste partial work.
	//  - Clients reconnect via GET /v1/responses/{id}/events?after=N and replay
	//    events they missed, which only works if the run kept going.
	//  - Explicit cancellation is available via POST /v1/responses/{id}/cancel.
	//  - serve.response_timeout bounds orphan-run lifetime.
	runCtx, cancel := context.WithTimeout(context.Background(), s.responseTimeout())
	run := newResponseRun(respID, sessionID, options.previousResponseID, model, created, cancel)
	createdRun, duplicate, err := mgr.createOrGetByIdempotency(run, options.idempotencyKey)
	if err != nil {
		cancel()
		return nil, err
	}
	if duplicate {
		cancel()
		return createdRun, nil
	}

	if options.uiSession {
		runtime.clearLastUIRunError()
	}

	createdResponse := map[string]any{
		"id":      respID,
		"object":  "response",
		"created": created,
		"model":   model,
		"status":  "in_progress",
	}
	if effort := strings.TrimSpace(llmReq.ReasoningEffort); effort != "" {
		createdResponse["reasoning_effort"] = effort
	}
	if options.modelSwap != nil && options.modelSwap.plan.enabled {
		createdResponse["provider"] = options.modelSwap.plan.requestedProvider
	}
	if err := run.appendEvent("response.created", map[string]any{
		"response": createdResponse,
	}); err != nil {
		cancel()
		mgr.clearActiveRun(sessionID, respID)
		mgr.delete(respID)
		return nil, err
	}

	if err := mgr.start(func() {
		defer func() {
			mgr.clearActiveRun(sessionID, respID)
			mgr.scheduleCleanup(respID)
		}()
		if !stateful {
			defer func() {
				runtime.Close()
				s.unregisterResponseIDs(runtime)
			}()
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

		if options.modelSwap != nil && options.modelSwap.plan.enabled {
			s.executeResponseRunModelSwap(runCtx, runtime, run, stateful, replaceHistory, inputMessages, llmReq, sessionID, respID, model, created, options)
			return
		}

		streamState := newResponseRunStreamState(model, llmReq.ReasoningEffort)
		result, err := runtime.RunWithEventsAndStart(runCtx, stateful, replaceHistory, inputMessages, llmReq, func() {
			mgr.setActiveRun(sessionID, respID)
		}, func(ev llm.Event) error {
			return s.appendResponseRunEvent(runtime, run, streamState, ev)
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				cancelled, cancelErr := run.finishCancelled(map[string]any{
					"response": map[string]any{
						"id":      respID,
						"object":  "response",
						"created": created,
						"model":   model,
						"status":  "cancelled",
					},
				})
				if cancelled {
					if options.uiSession {
						runtime.clearLastUIRunError()
					}
					if cancelErr != nil {
						log.Printf("response run %s failed to append cancellation event: %v", respID, cancelErr)
					}
					return
				}
			}
			errType := "invalid_request_error"
			errMessage := err.Error()
			if errors.Is(err, context.DeadlineExceeded) {
				errType = "timeout_error"
				errMessage = responseRunTimeoutMessage(s.responseTimeout())
			} else if errors.Is(err, errServeSessionBusy) {
				errType = "conflict_error"
			}
			hadSubscribers, failErr := run.fail(map[string]any{
				"error": map[string]any{
					"message": errMessage,
					"type":    errType,
				},
			}, errType, errMessage)
			if options.uiSession {
				switch {
				case hadSubscribers:
					runtime.clearLastUIRunError()
				case errors.Is(err, context.Canceled):
					runtime.clearLastUIRunError()
				default:
					runtime.setLastUIRunError(errMessage)
				}
			}
			if failErr != nil {
				log.Printf("response run %s failed to append terminal event: %v", respID, failErr)
			}
			if failErr != nil && options.uiSession && !errors.Is(err, context.Canceled) {
				runtime.setLastUIRunError(errMessage)
			}
			return
		}

		if options.uiSession {
			runtime.clearLastUIRunError()
		}
		if options.resetResponseIDsOnSuccess {
			s.unregisterSessionResponseIDs(sessionID)
		}
		durableID := s.latestDurableResponseIDForSessionBestEffort(runCtx, sessionID)
		completedID := respID
		if durableID != "" {
			completedID = durableID
		}
		if completedID != respID {
			s.registerResponseID(runtime, respID, sessionID)
		}
		s.registerResponseID(runtime, completedID, sessionID)
		finalModel := streamState.appliedModel(model)
		finalEffort, finalEffortSet := streamState.appliedReasoningEffort(llmReq.ReasoningEffort)
		if options.uiSession && (finalModel != model || finalEffort != strings.TrimSpace(llmReq.ReasoningEffort) || finalEffortSet != (strings.TrimSpace(llmReq.ReasoningEffort) != "")) {
			s.syncPersistedSessionRuntime(runCtx, sessionID, runtime, finalModel, finalEffort)
		}
		completeResponse := map[string]any{
			"id":            completedID,
			"object":        "response",
			"created":       created,
			"model":         finalModel,
			"status":        "completed",
			"usage":         usagePayload(result.Usage),
			"session_usage": usagePayload(result.SessionUsage),
		}
		if finalEffortSet {
			completeResponse["reasoning_effort"] = finalEffort
		}
		if err := run.complete(map[string]any{
			"response": completeResponse,
		}, result.Usage, result.SessionUsage); err != nil {
			log.Printf("response run %s failed to append completion event: %v", respID, err)
		}
	}); err != nil {
		cancel()
		mgr.clearActiveRun(sessionID, respID)
		mgr.delete(respID)
		return nil, err
	}

	return run, nil
}
