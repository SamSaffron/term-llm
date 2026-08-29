package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

	"github.com/samsaffron/term-llm/internal/session"
)

const (
	serveEventReplayLimit      = 2048
	serveEventSubscriberBuffer = 128
	serveEventMaxSubscribers   = 256
	serveEventHeartbeat        = 10 * time.Second
	serveEventPollWait         = 25 * time.Second
	serveEventPollLimit        = 100
	serveEventPollMaxBytes     = 1 << 20
)

const (
	serveEventSessionCreated           = "session.created"
	serveEventSessionMetadataChanged   = "session.metadata_changed"
	serveEventSessionTranscriptChanged = "session.transcript_changed"
	serveEventSessionRuntimeChanged    = "session.runtime_changed"
	serveEventSessionDeleted           = "session.deleted"
	serveEventProjectCreated           = "project.created"
	serveEventProjectUpdated           = "project.updated"
	serveEventProjectDeleted           = "project.deleted"
	serveEventProjectMembershipChanged = "project.membership_changed"
	serveEventRunStarted               = "run.started"
	serveEventRunFinished              = "run.finished"
	serveEventInteractionChanged       = "interaction.changed"
	serveEventChildrenChanged          = "children.changed"
	serveEventFilesChanged             = "files.changed"
	serveEventSnapshotRequired         = "snapshot.required"
)

var validServeEventTypes = map[string]struct{}{
	serveEventSessionCreated: {}, serveEventSessionMetadataChanged: {},
	serveEventSessionTranscriptChanged: {}, serveEventSessionRuntimeChanged: {},
	serveEventSessionDeleted: {}, serveEventProjectCreated: {}, serveEventProjectUpdated: {},
	serveEventProjectDeleted: {}, serveEventProjectMembershipChanged: {}, serveEventRunStarted: {}, serveEventRunFinished: {},
	serveEventInteractionChanged: {}, serveEventChildrenChanged: {}, serveEventFilesChanged: {},
	serveEventSnapshotRequired: {},
}

type serveEvent struct {
	Version         int    `json:"v"`
	Sequence        uint64 `json:"sequence"`
	InstanceID      string `json:"instance_id"`
	Type            string `json:"type"`
	OccurredAt      int64  `json:"occurred_at"`
	SessionID       string `json:"session_id,omitempty"`
	ResponseID      string `json:"response_id,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	TranscriptRev   int64  `json:"transcript_rev,omitempty"`
	RunEpoch        int64  `json:"run_epoch,omitempty"`
	OperationID     string `json:"operation_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type serveEventInput struct {
	Type            string
	SessionID       string
	ResponseID      string
	ProjectID       string
	ParentSessionID string
	TranscriptRev   int64
	RunEpoch        int64
	OperationID     string
	Reason          string
}

type serveEventCursorError struct {
	MinReplayAfter uint64
	Latest         uint64
}

func (e *serveEventCursorError) Error() string { return "event replay is unavailable" }

var errServeEventBrokerClosed = errors.New("event broker is closed")

type serveEventSubscription struct {
	ID     int
	Replay []serveEvent
	Events <-chan serveEvent
}

type serveEventBroker struct {
	mu             sync.Mutex
	instanceID     string
	sequence       uint64
	events         []serveEvent
	subscribers    map[int]chan serveEvent
	nextSubscriber int
	closed         bool
}

func newServeEventBroker() *serveEventBroker {
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		return &serveEventBroker{
			instanceID:  fmt.Sprintf("evt_%d", time.Now().UnixNano()),
			subscribers: make(map[int]chan serveEvent),
		}
	}
	return &serveEventBroker{
		instanceID:  "evt_" + hex.EncodeToString(token[:]),
		subscribers: make(map[int]chan serveEvent),
	}
}

func (b *serveEventBroker) Position() (string, uint64, uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.instanceID, b.sequence, b.minReplayAfterLocked()
}

func (b *serveEventBroker) minReplayAfterLocked() uint64 {
	if len(b.events) == 0 {
		return b.sequence
	}
	return b.events[0].Sequence - 1
}

func (b *serveEventBroker) Publish(input serveEventInput) (serveEvent, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return serveEvent{}, errServeEventBrokerClosed
	}
	b.sequence++
	event := serveEvent{
		Version: 1, Sequence: b.sequence, InstanceID: b.instanceID,
		Type: input.Type, OccurredAt: time.Now().UnixMilli(),
		SessionID: input.SessionID, ResponseID: input.ResponseID, ProjectID: input.ProjectID,
		ParentSessionID: input.ParentSessionID, TranscriptRev: input.TranscriptRev,
		RunEpoch: input.RunEpoch, OperationID: input.OperationID, Reason: input.Reason,
	}
	b.events = append(b.events, event)
	if len(b.events) > serveEventReplayLimit {
		copy(b.events, b.events[len(b.events)-serveEventReplayLimit:])
		b.events = b.events[:serveEventReplayLimit]
	}
	for id, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			log.Printf("[serve] event subscriber %d fell behind at sequence %d; closing stream", id, event.Sequence)
			close(ch)
			delete(b.subscribers, id)
		}
	}
	return event, nil
}

func (b *serveEventBroker) Subscribe(after *uint64) (serveEventSubscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return serveEventSubscription{}, errServeEventBrokerClosed
	}
	if len(b.subscribers) >= serveEventMaxSubscribers {
		return serveEventSubscription{}, fmt.Errorf("event subscriber limit reached")
	}
	if after != nil {
		minimum := b.minReplayAfterLocked()
		if *after < minimum || *after > b.sequence {
			return serveEventSubscription{}, &serveEventCursorError{MinReplayAfter: minimum, Latest: b.sequence}
		}
	}
	var replay []serveEvent
	if after != nil {
		for _, event := range b.events {
			if event.Sequence > *after {
				replay = append(replay, event)
			}
		}
	}
	b.nextSubscriber++
	id := b.nextSubscriber
	ch := make(chan serveEvent, serveEventSubscriberBuffer)
	b.subscribers[id] = ch
	return serveEventSubscription{ID: id, Replay: replay, Events: ch}, nil
}

func (b *serveEventBroker) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subscribers[id]; ok {
		close(ch)
		delete(b.subscribers, id)
	}
}

func (b *serveEventBroker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, id)
	}
}

func (s *serveServer) ensureEventBroker() *serveEventBroker {
	s.eventBrokerMu.Lock()
	defer s.eventBrokerMu.Unlock()
	if s.eventBroker == nil {
		s.eventBroker = newServeEventBroker()
	}
	return s.eventBroker
}

func (s *serveServer) resetEventBroker() {
	s.eventBrokerMu.Lock()
	defer s.eventBrokerMu.Unlock()
	if s.eventBroker != nil {
		s.eventBroker.Close()
	}
	s.eventBroker = newServeEventBroker()
}

func (s *serveServer) closeEventBroker() {
	s.eventBrokerMu.Lock()
	broker := s.eventBroker
	s.eventBrokerMu.Unlock()
	if broker != nil {
		broker.Close()
	}
}

func (s *serveServer) publishEvent(input serveEventInput) {
	input.Type = strings.TrimSpace(input.Type)
	if _, ok := validServeEventTypes[input.Type]; !ok {
		log.Printf("[serve] ignored invalid server event type %q", input.Type)
		return
	}
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.ResponseID = strings.TrimSpace(input.ResponseID)
	input.ProjectID = strings.TrimSpace(input.ProjectID)
	input.ParentSessionID = strings.TrimSpace(input.ParentSessionID)
	input.OperationID = strings.TrimSpace(input.OperationID)
	input.Reason = strings.TrimSpace(input.Reason)
	if _, err := s.ensureEventBroker().Publish(input); err != nil && !errors.Is(err, errServeEventBrokerClosed) {
		log.Printf("[serve] publish event %s: %v", input.Type, err)
	}
}

func (s *serveServer) publishResponseRunEvent(run *responseRun, event string, payload map[string]any) {
	if run == nil {
		return
	}
	switch event {
	case "response.created",
		"response.completed", "response.cancelled", "response.failed",
		"response.ask_user.prompt", "response.ask_user.resolved", "response.approval.prompt", "response.approval.resolved",
		"response.file_change", "response.filesystem_observation", "response.output_claim_diagnostic",
		"response.interjection", "response.compaction", "response.model_switch":
	default:
		return
	}
	base := serveEventInput{
		SessionID: run.sessionID, ResponseID: run.id, RunEpoch: run.runEpoch,
		OperationID: run.clientMessageID,
	}
	switch event {
	case "response.created":
		base.Type, base.Reason = serveEventRunStarted, "started"
		s.publishEvent(base)
	case "response.completed", "response.cancelled", "response.failed":
		base.Type = serveEventRunFinished
		base.Reason = strings.TrimPrefix(event, "response.")
		base.TranscriptRev = run.finalRev
		s.publishEvent(base)
		if run.finalRev > run.startedRev {
			base.Type, base.Reason = serveEventSessionTranscriptChanged, "run_"+base.Reason
			s.publishEvent(base)
		}
	case "response.ask_user.prompt", "response.ask_user.resolved", "response.approval.prompt", "response.approval.resolved":
		base.Type, base.Reason = serveEventInteractionChanged, strings.TrimPrefix(event, "response.")
		s.publishEvent(base)
	case "response.file_change", "response.filesystem_observation", "response.output_claim_diagnostic":
		base.Type, base.Reason = serveEventFilesChanged, strings.TrimPrefix(event, "response.")
		s.publishEvent(base)
	case "response.interjection", "response.compaction", "response.model_switch":
		base.Type, base.Reason = serveEventSessionRuntimeChanged, strings.TrimPrefix(event, "response.")
		s.publishEvent(base)
	}
}

func parseServeEventCursor(r *http.Request) (*uint64, error) {
	queryText := strings.TrimSpace(r.URL.Query().Get("after"))
	headerText := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if queryText != "" && headerText != "" && queryText != headerText {
		return nil, fmt.Errorf("after and Last-Event-ID must match")
	}
	text := queryText
	if text == "" {
		text = headerText
	}
	if text == "" {
		return nil, nil
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("event cursor must be an unsigned integer")
	}
	return &value, nil
}

func (s *serveServer) writeEventCursorError(w http.ResponseWriter, err *serveEventCursorError) {
	instanceID, latest, minimum := s.ensureEventBroker().Position()
	if err != nil {
		latest, minimum = err.Latest, err.MinReplayAfter
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":             map[string]any{"type": "event_cursor_conflict", "message": "event replay is unavailable; refresh authoritative state"},
		"snapshot_required": true, "instance_id": instanceID,
		"min_replay_after": minimum, "latest_sequence": latest,
	})
}

func writeServeEventSSE(w io.Writer, event serveEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, data)
	return err
}

func parseServeEventChannels(r *http.Request) map[string]struct{} {
	channels := make(map[string]struct{})
	for _, raw := range strings.Split(r.URL.Query().Get("channels"), ",") {
		channel := strings.TrimSpace(raw)
		if channel == "" || len(channel) > 256 || len(channels) >= 16 {
			continue
		}
		channels[channel] = struct{}{}
	}
	// session_id was used by the first development version; keep it as a
	// shorthand while first-party clients move to explicit interest channels.
	if id := strings.TrimSpace(r.URL.Query().Get("session_id")); id != "" {
		channels["session:"+id] = struct{}{}
		channels["children:"+id] = struct{}{}
		channels["files:"+id] = struct{}{}
	}
	return channels
}

func serveEventRelevant(event serveEvent, channels map[string]struct{}) bool {
	switch event.Type {
	case serveEventSessionCreated, serveEventSessionMetadataChanged, serveEventSessionDeleted,
		serveEventProjectCreated, serveEventProjectUpdated, serveEventProjectDeleted,
		serveEventProjectMembershipChanged, serveEventRunStarted, serveEventRunFinished, serveEventSnapshotRequired:
		return true
	case serveEventChildrenChanged:
		_, ok := channels["children:"+event.ParentSessionID]
		return ok && event.ParentSessionID != ""
	case serveEventFilesChanged:
		_, ok := channels["files:"+event.SessionID]
		return ok && event.SessionID != ""
	default:
		_, ok := channels["session:"+event.SessionID]
		return ok && event.SessionID != ""
	}
}

func writeServeEventCursorSSE(w io.Writer, instanceID string, sequence uint64) error {
	data, err := json.Marshal(map[string]any{
		"v": 1, "instance_id": instanceID, "latest_sequence": sequence,
		"heartbeat_ms": serveEventHeartbeat.Milliseconds(), "replay_limit": serveEventReplayLimit,
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: cursor\ndata: %s\n\n", sequence, data)
	return err
}

func (s *serveServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	cursor, err := parseServeEventCursor(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	broker := s.ensureEventBroker()
	subscription, err := broker.Subscribe(cursor)
	if cursorErr := new(serveEventCursorError); errors.As(err, &cursorErr) {
		s.writeEventCursorError(w, cursorErr)
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	defer broker.Unsubscribe(subscription.ID)

	ctx, cancel := s.contextWithShutdown(r.Context())
	defer cancel()
	stream := newStreamingResponseWriter(w, serveStreamWriteTimeout)
	flusher, ok := stream.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming is unsupported")
		return
	}
	setSSEHeaders(stream)
	stream.Header().Set("Cache-Control", "no-cache, no-transform")
	instanceID, latest, _ := broker.Position()
	ready := map[string]any{
		"v": 1, "instance_id": instanceID, "latest_sequence": latest,
		"heartbeat_ms": serveEventHeartbeat.Milliseconds(), "replay_limit": serveEventReplayLimit,
	}
	data, _ := json.Marshal(ready)
	if _, err := fmt.Fprintf(stream, "id: %d\nevent: ready\ndata: %s\n\n", latest, data); err != nil {
		return
	}
	flusher.Flush()
	channels := parseServeEventChannels(r)
	pendingCursor := uint64(0)
	for _, event := range subscription.Replay {
		if !serveEventRelevant(event, channels) {
			pendingCursor = event.Sequence
			continue
		}
		if pendingCursor > 0 {
			if err := writeServeEventCursorSSE(stream, instanceID, pendingCursor); err != nil {
				return
			}
			pendingCursor = 0
		}
		if err := writeServeEventSSE(stream, event); err != nil {
			return
		}
		flusher.Flush()
	}

	heartbeat := time.NewTicker(serveEventHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-subscription.Events:
			if !ok {
				return
			}
			if !serveEventRelevant(event, channels) {
				pendingCursor = event.Sequence
				continue
			}
			if pendingCursor > 0 {
				if err := writeServeEventCursorSSE(stream, instanceID, pendingCursor); err != nil {
					return
				}
				pendingCursor = 0
			}
			if err := writeServeEventSSE(stream, event); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if pendingCursor > 0 {
				if err := writeServeEventCursorSSE(stream, instanceID, pendingCursor); err != nil {
					return
				}
				pendingCursor = 0
			}
			if _, err := io.WriteString(stream, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

type serveEventPollResponse struct {
	Object         string       `json:"object"`
	InstanceID     string       `json:"instance_id"`
	Data           []serveEvent `json:"data"`
	LatestSequence uint64       `json:"latest_sequence"`
	NextAfter      uint64       `json:"next_after"`
	TimedOut       bool         `json:"timed_out,omitempty"`
}

func (s *serveServer) handleEventPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	cursor, err := parseServeEventCursor(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	broker := s.ensureEventBroker()
	instanceID, latest, _ := broker.Position()
	if cursor == nil {
		writeJSON(w, http.StatusOK, serveEventPollResponse{
			Object: "list", InstanceID: instanceID, Data: []serveEvent{},
			LatestSequence: latest, NextAfter: latest,
		})
		return
	}
	subscription, err := broker.Subscribe(cursor)
	if cursorErr := new(serveEventCursorError); errors.As(err, &cursorErr) {
		s.writeEventCursorError(w, cursorErr)
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error", err.Error())
		return
	}
	defer broker.Unsubscribe(subscription.ID)

	limit := serveEventPollLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			limit = max(1, min(serveEventPollLimit, parsed))
		}
	}
	wait := serveEventPollWait
	if raw := strings.TrimSpace(r.URL.Query().Get("wait_ms")); raw != "" {
		if parsed, parseErr := strconv.Atoi(raw); parseErr == nil {
			wait = time.Duration(max(1_000, min(30_000, parsed))) * time.Millisecond
		}
	}
	channels := parseServeEventChannels(r)
	events := make([]serveEvent, 0, min(limit, len(subscription.Replay)))
	next := *cursor
	for _, event := range subscription.Replay {
		if len(events) >= limit {
			break
		}
		next = event.Sequence
		if serveEventRelevant(event, channels) {
			events = append(events, event)
		}
	}
	timedOut := false
	ctx, cancel := s.contextWithShutdown(r.Context())
	defer cancel()
	if len(events) == 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		for len(events) == 0 {
			select {
			case event, ok := <-subscription.Events:
				if !ok {
					goto drained
				}
				next = event.Sequence
				if serveEventRelevant(event, channels) {
					events = append(events, event)
				}
			case <-timer.C:
				timedOut = true
				goto drained
			case <-ctx.Done():
				return
			}
		}
	}
	for len(events) < limit {
		select {
		case event, ok := <-subscription.Events:
			if !ok {
				goto drained
			}
			next = event.Sequence
			if serveEventRelevant(event, channels) {
				events = append(events, event)
			}
		default:
			goto drained
		}
	}

drained:
	// Keep the response bounded even if future event envelopes grow.
	for len(events) > 0 {
		encoded, _ := json.Marshal(events)
		if len(encoded) <= serveEventPollMaxBytes {
			break
		}
		events = events[:len(events)-1]
	}
	instanceID, latest, _ = broker.Position()
	writeJSON(w, http.StatusOK, serveEventPollResponse{
		Object: "list", InstanceID: instanceID, Data: events,
		LatestSequence: latest, NextAfter: next, TimedOut: timedOut,
	})
}

const serveEventStoreChangeBatch = 256

func storeChangeCoalesceKey(change session.StoreChange) string {
	switch change.Kind {
	case session.StoreChangeSessionMetadataChanged,
		session.StoreChangeSessionTranscriptChanged,
		session.StoreChangeProjectMembershipChanged:
		return change.Kind + ":session:" + change.SessionID
	case session.StoreChangeProjectUpdated:
		return change.Kind + ":project:" + change.ProjectID
	default:
		return ""
	}
}

func coalesceObservedStoreChanges(changes []session.StoreChange) []session.StoreChange {
	deletedSessions := make(map[string]struct{})
	deletedProjects := make(map[string]struct{})
	for _, change := range changes {
		switch change.Kind {
		case session.StoreChangeSessionDeleted:
			deletedSessions[change.SessionID] = struct{}{}
		case session.StoreChangeProjectDeleted:
			deletedProjects[change.ProjectID] = struct{}{}
		}
	}

	result := make([]session.StoreChange, 0, len(changes))
	positions := make(map[string]int)
	for _, change := range changes {
		key := storeChangeCoalesceKey(change)
		if key != "" {
			if _, deleted := deletedSessions[change.SessionID]; deleted && change.SessionID != "" {
				continue
			}
			if _, deleted := deletedProjects[change.ProjectID]; deleted && change.ProjectID != "" {
				continue
			}
			if position, exists := positions[key]; exists {
				result[position] = change
				continue
			}
			positions[key] = len(result)
		}
		result = append(result, change)
	}
	return result
}

func (s *serveServer) publishObservedStoreChange(change session.StoreChange) {
	input := serveEventInput{
		SessionID:     change.SessionID,
		ProjectID:     change.ProjectID,
		TranscriptRev: change.TranscriptRev,
		Reason:        "store_observed",
	}
	switch change.Kind {
	case session.StoreChangeSessionCreated:
		input.Type = serveEventSessionCreated
		s.publishEvent(input)
		if change.Status == session.StatusActive {
			input.Type = serveEventRunStarted
			s.publishEvent(input)
		}
	case session.StoreChangeSessionDeleted:
		input.Type = serveEventSessionDeleted
		s.publishEvent(input)
	case session.StoreChangeSessionMetadataChanged:
		input.Type = serveEventSessionMetadataChanged
		s.publishEvent(input)
	case session.StoreChangeSessionTranscriptChanged:
		input.Type = serveEventSessionTranscriptChanged
		s.publishEvent(input)
	case session.StoreChangeProjectMembershipChanged:
		input.Type = serveEventProjectMembershipChanged
		s.publishEvent(input)
	case session.StoreChangeSessionStatusChanged:
		if change.Status == session.StatusActive {
			input.Type = serveEventRunStarted
		} else {
			input.Type = serveEventRunFinished
			switch change.Status {
			case session.StatusComplete:
				input.Reason = "completed"
			case session.StatusInterrupted:
				input.Reason = "cancelled"
			case session.StatusError:
				input.Reason = "failed"
			default:
				input.Reason = string(change.Status)
			}
		}
		s.publishEvent(input)
	case session.StoreChangeProjectCreated:
		input.Type = serveEventProjectCreated
		s.publishEvent(input)
	case session.StoreChangeProjectUpdated:
		input.Type = serveEventProjectUpdated
		s.publishEvent(input)
	case session.StoreChangeProjectDeleted:
		input.Type = serveEventProjectDeleted
		input.Reason = "store_observed_deleted"
		s.publishEvent(input)
	default:
		log.Printf("[serve] ignored unknown store change kind %q at sequence %d", change.Kind, change.Sequence)
	}
}

func (s *serveServer) startEventWatcher() {
	s.eventWatcherMu.Lock()
	defer s.eventWatcherMu.Unlock()
	changeStore, ok := session.AsStoreChangeStore(s.store)
	if !ok {
		return
	}
	if s.eventWatcherCancel != nil {
		s.eventWatcherCancel()
		s.eventWatcherWG.Wait()
		s.eventWatcherCancel = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cursor, err := changeStore.StoreChangeCursor(ctx)
	if err != nil {
		cancel()
		log.Printf("[serve] disable store event watcher: %v", err)
		return
	}
	s.eventWatcherCancel = cancel
	s.eventWatcherWG.Add(1)
	go func() {
		defer s.eventWatcherWG.Done()
		loggedReadError := false
		load := func() {
			nextCursor := cursor
			pending := make([]session.StoreChange, 0, serveEventStoreChangeBatch)
			overflow := false
			for {
				changes, err := changeStore.ListStoreChanges(ctx, nextCursor, serveEventStoreChangeBatch)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					var cursorErr *session.StoreChangeCursorError
					if errors.As(err, &cursorErr) {
						s.publishEvent(serveEventInput{Type: serveEventSnapshotRequired, Reason: "store_cursor_gap"})
						cursor = cursorErr.Latest
						return
					}
					if !loggedReadError {
						log.Printf("[serve] read store event changes: %v", err)
						loggedReadError = true
					}
					return
				}
				loggedReadError = false
				for _, change := range changes {
					if change.Sequence <= nextCursor {
						continue
					}
					nextCursor = change.Sequence
					if len(pending) < serveEventReplayLimit {
						pending = append(pending, change)
					} else {
						overflow = true
					}
				}
				if len(changes) < serveEventStoreChangeBatch {
					break
				}
			}
			if overflow {
				s.publishEvent(serveEventInput{Type: serveEventSnapshotRequired, Reason: "store_backlog"})
			} else {
				for _, change := range coalesceObservedStoreChanges(pending) {
					s.publishObservedStoreChange(change)
				}
			}
			cursor = nextCursor
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				load()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (s *serveServer) stopEventWatcher() {
	s.eventWatcherMu.Lock()
	defer s.eventWatcherMu.Unlock()
	if s.eventWatcherCancel == nil {
		return
	}
	s.eventWatcherCancel()
	s.eventWatcherCancel = nil
	s.eventWatcherWG.Wait()
}

// waitForServeEvent is a test helper that avoids timing sleeps in publication tests.
func waitForServeEvent(ctx context.Context, ch <-chan serveEvent) (serveEvent, error) {
	select {
	case event, ok := <-ch:
		if !ok {
			return serveEvent{}, io.EOF
		}
		return event, nil
	case <-ctx.Done():
		return serveEvent{}, ctx.Err()
	}
}
