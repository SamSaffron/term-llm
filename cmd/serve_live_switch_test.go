package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func liveSessionChangedEvents(record *liveSession) []liveSessionEvent {
	record.mu.Lock()
	defer record.mu.Unlock()
	var events []liveSessionEvent
	for _, event := range record.events {
		if event.Type == liveEventSessionChanged {
			events = append(events, event)
		}
	}
	return events
}

// liveEventSnapshot copies the whole ring buffer in publish order, so a test can
// reason about event sequencing instead of just membership.
func liveEventSnapshot(record *liveSession) []liveSessionEvent {
	record.mu.Lock()
	defer record.mu.Unlock()
	events, tail := record.eventRangesLocked()
	return append(append(make([]liveSessionEvent, 0, len(events)+len(tail)), events...), tail...)
}

// TestLiveBindingEventsNeverNameTheAbandonedSession guards the invariant behind
// appendBindingEvent: the session an event names must be read under the same lock
// that publishes it. Reading the binding with a separate lock acquisition lets a
// concurrent rebind slip between the read and the publish, leaving a later event
// naming the session the call has just left — and a browser that follows that id
// moves back to the abandoned session. live_switch_session is called from inside a
// delegated turn, so a delegation update racing the rebind it caused is the normal
// path, not an exotic one.
//
// The events themselves define which binding was in force at each sequence: the
// call starts bound to race-source and every rebind publishes exactly one
// live.session_changed, so an event that names anything other than the most recent
// change is one the viewer would follow backwards. Several rebinds run at once to
// keep the publish lock contended for the whole burst, because the defect is a
// lost publish ordering and it is only visible under interleaving.
func TestLiveBindingEventsNeverNameTheAbandonedSession(t *testing.T) {
	targets := []string{"race-t1", "race-t2", "race-t3", "race-t4", "race-t5", "race-t6", "race-t7", "race-t8"}
	for name, update := range map[string]live.Update{
		"delegation": {Kind: live.UpdateDelegation, DelegationID: "delegation-1", State: live.DelegationRunning},
		"started":    {Kind: live.UpdateStarted},
	} {
		t.Run(name, func(t *testing.T) {
			for attempt := range 60 {
				srv := &serveServer{}
				record := newLiveSession("call-binding-race", "race-source")
				if err := srv.registerLiveSession(record); err != nil {
					t.Fatal(err)
				}
				gate := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(len(targets) + 1)
				go func() {
					defer wg.Done()
					<-gate
					for range 40 {
						record.observe(update)
					}
				}()
				for _, target := range targets {
					go func() {
						defer wg.Done()
						<-gate
						if _, err := srv.rebindLiveSession(record, liveSwitchTarget{SessionID: target, Number: 2, Title: target}); err != nil {
							t.Errorf("attempt %d: rebind to %s: %v", attempt, target, err)
						}
					}()
				}
				close(gate)
				wg.Wait()

				events := liveEventSnapshot(record)
				changed := make(map[string]bool, len(targets))
				binding := "race-source"
				for _, event := range events {
					switch event.Type {
					case liveEventSessionChanged:
						target, _ := event.Data["session_id"].(string)
						if changed[target] {
							t.Fatalf("attempt %d: session_changed repeated %s: %+v", attempt, target, events)
						}
						changed[target] = true
						binding = target
					case liveEventStarted, liveEventDelegation:
						if named := event.Data["session_id"]; named != binding {
							t.Fatalf("attempt %d: %s at sequence %d names %#v while the call was bound to %s",
								attempt, event.Type, event.Sequence, named, binding)
						}
					}
				}
				// Every rebind is legal and distinct, so a lost or duplicated
				// transition means the publish lock is not doing its job either.
				for _, target := range targets {
					if !changed[target] {
						t.Fatalf("attempt %d: no session_changed for %s: %+v", attempt, target, events)
					}
				}
				if binding != record.boundSession() {
					t.Fatalf("attempt %d: last published binding %s, call bound to %s", attempt, binding, record.boundSession())
				}
			}
		})
	}
}

// TestLiveRebindPublishesSessionChangedInBindingOrder covers the sequential case
// a browser relies on: each rebind publishes exactly one live.session_changed, in
// binding order, with strictly increasing sequences, and every binding-carrying
// event names the binding in force at its own sequence number.
func TestLiveRebindPublishesSessionChangedInBindingOrder(t *testing.T) {
	srv := &serveServer{}
	record := newLiveSession("call-order", "order-a")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	targets := []string{"order-b", "order-c", "order-d"}
	bindings := append([]string{"order-a"}, targets...)
	for i, target := range targets {
		record.observe(live.Update{Kind: live.UpdateDelegation, DelegationID: "delegation-" + strconv.Itoa(i), State: live.DelegationRunning})
		if _, err := srv.rebindLiveSession(record, liveSwitchTarget{SessionID: target, Number: int64(i + 2), Title: target}); err != nil {
			t.Fatal(err)
		}
	}
	record.observe(live.Update{Kind: live.UpdateDelegation, DelegationID: "delegation-final", State: live.DelegationDone})

	var changed, delegations []string
	last := 0
	for _, event := range liveEventSnapshot(record) {
		if event.Sequence <= last {
			t.Fatalf("sequence %d did not advance past %d", event.Sequence, last)
		}
		last = event.Sequence
		switch event.Type {
		case liveEventSessionChanged:
			changed = append(changed, event.Data["session_id"].(string))
		case liveEventDelegation:
			delegations = append(delegations, event.Data["session_id"].(string))
		}
	}
	if !slices.Equal(changed, targets) {
		t.Fatalf("session_changed order = %v", changed)
	}
	if !slices.Equal(delegations, bindings) {
		t.Fatalf("delegation bindings = %v, want %v", delegations, bindings)
	}
	if record.boundSession() != "order-d" {
		t.Fatalf("binding = %q", record.boundSession())
	}
}

func TestLiveRebindSwapsChatIndexAndPublishesOneEvent(t *testing.T) {
	srv, _ := newStubLiveHarness(t)
	record := newLiveSession("call-rebind", "chat-a")
	record.lastActivity = time.Now().Add(-time.Hour)
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	previous, err := srv.rebindLiveSession(record, liveSwitchTarget{SessionID: "chat-b", Number: 7, Title: "Fix reflow crash"})
	if err != nil || previous != "chat-a" {
		t.Fatalf("rebind = %q, %v", previous, err)
	}
	srv.liveMu.Lock()
	rebound, abandoned := srv.liveByChat["chat-b"], srv.liveByChat["chat-a"]
	srv.liveMu.Unlock()
	if rebound != record.id {
		t.Fatalf("liveByChat[chat-b] = %q", rebound)
	}
	if abandoned != "" {
		t.Fatalf("the abandoned chat still maps to %q", abandoned)
	}
	if record.boundSession() != "chat-b" {
		t.Fatalf("bound session = %q", record.boundSession())
	}
	if record.idleFor(time.Now()) > time.Minute {
		t.Fatal("rebind did not refresh the idle watchdog")
	}
	events := liveSessionChangedEvents(record)
	if len(events) != 1 {
		t.Fatalf("session_changed events = %d", len(events))
	}
	data := events[0].Data
	if data["session_id"] != "chat-b" || data["title"] != "Fix reflow crash" {
		t.Fatalf("event data = %+v", data)
	}
	if number, ok := data["session_number"].(int64); !ok || number != 7 {
		t.Fatalf("event session_number = %#v", data["session_number"])
	}

	// Re-binding to the current session is a reported no-op with no event.
	previous, err = srv.rebindLiveSession(record, liveSwitchTarget{SessionID: "chat-b", Number: 7, Title: "Fix reflow crash"})
	if err != nil || previous != "chat-b" {
		t.Fatalf("no-op rebind = %q, %v", previous, err)
	}
	if events := liveSessionChangedEvents(record); len(events) != 1 {
		t.Fatalf("no-op published %d events", len(events))
	}
}

func TestLiveRebindRejectsTargetHostingAnotherCall(t *testing.T) {
	srv, _ := newStubLiveHarness(t)
	first := newLiveSession("call-one", "chat-a")
	second := newLiveSession("call-two", "chat-c")
	for _, record := range []*liveSession{first, second} {
		if err := srv.registerLiveSession(record); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := srv.rebindLiveSession(first, liveSwitchTarget{SessionID: "chat-c", Number: 3, Title: "Busy"}); !errors.Is(err, errLiveTargetBusy) {
		t.Fatalf("rebind onto a hosted chat = %v", err)
	}
	if first.boundSession() != "chat-a" || len(liveSessionChangedEvents(first)) != 0 {
		t.Fatal("refused rebind still moved the call")
	}
	// A stale index entry for a call that is gone does not block the move.
	srv.liveMu.Lock()
	delete(srv.liveSessions, second.id)
	srv.liveMu.Unlock()
	if _, err := srv.rebindLiveSession(first, liveSwitchTarget{SessionID: "chat-c", Number: 3, Title: "Busy"}); err != nil {
		t.Fatalf("rebind onto a released chat = %v", err)
	}
	if first.boundSession() != "chat-c" {
		t.Fatalf("bound session = %q", first.boundSession())
	}
}

func TestLiveRebindRefusesEndedCallAndSuppressesEvent(t *testing.T) {
	srv, _ := newStubLiveHarness(t)
	record := newLiveSession("call-ended", "chat-a")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	// The call's stream is terminal: no later event may become visible to a
	// reconnecting browser.
	record.appendEvent(liveEventEnded, nil)
	if _, err := srv.rebindLiveSession(record, liveSwitchTarget{SessionID: "chat-b", Number: 2, Title: "B"}); !errors.Is(err, errLiveCallEnded) {
		t.Fatalf("ended rebind = %v", err)
	}
	if record.boundSession() != "chat-a" || len(liveSessionChangedEvents(record)) != 0 {
		t.Fatal("an ended call changed its binding or published an event")
	}

	stopped, _ := newStubLiveHarness(t)
	closed := newLiveSession("call-closed", "chat-a")
	if err := stopped.registerLiveSession(closed); err != nil {
		t.Fatal(err)
	}
	stopped.closeLiveSessions(context.Background())
	if _, err := stopped.rebindLiveSession(closed, liveSwitchTarget{SessionID: "chat-b"}); !errors.Is(err, errLiveCallEnded) {
		t.Fatalf("rebind during shutdown = %v", err)
	}
}

func TestLiveRebindLosesCleanlyToConcurrentStop(t *testing.T) {
	for range 50 {
		srv, _ := newStubLiveHarness(t)
		record := newLiveSession("race-call", "race-a")
		if err := srv.registerLiveSession(record); err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-gate
			_, _ = srv.rebindLiveSession(record, liveSwitchTarget{SessionID: "race-b", Number: 2, Title: "B"})
		}()
		go func() {
			defer wg.Done()
			<-gate
			srv.stopLiveSession(context.Background(), record.id, "user")
		}()
		close(gate)
		wg.Wait()
		if _, ok := srv.lookupLiveSession(record.id); ok {
			t.Fatal("call retained after stop")
		}
		srv.liveMu.Lock()
		for chat, liveID := range srv.liveByChat {
			if liveID == record.id {
				srv.liveMu.Unlock()
				t.Fatalf("stale reverse index %q -> %s", chat, liveID)
			}
		}
		srv.liveMu.Unlock()
	}
}

func TestLiveRebindResolvesSelectorsWithoutFuzzyMatching(t *testing.T) {
	store := newSessionDirectoryTestStore(t)
	ctx := context.Background()
	createDirectorySession(t, store, &session.Session{ID: "20260917-120000-abcdef0123456789", GeneratedShortTitle: "Reflow crash"})
	createDirectorySession(t, store, &session.Session{ID: "20260917-130000-fedcba9876543210", Summary: "Other work", Archived: true})
	target := &session.Session{ID: "subagent-child", IsSubagent: true, ParentID: "20260917-120000-abcdef0123456789"}
	createDirectorySession(t, store, target)
	srv := &serveServer{store: store}
	first, err := store.Get(ctx, "20260917-120000-abcdef0123456789")
	if err != nil || first == nil {
		t.Fatal(err)
	}
	archived, err := store.Get(ctx, "20260917-130000-fedcba9876543210")
	if err != nil || archived == nil {
		t.Fatal(err)
	}

	resolved, err := srv.resolveLiveSwitchTarget(ctx, first.ID)
	if err != nil || resolved.SessionID != first.ID || resolved.Number != first.Number || resolved.Title != "Reflow crash" {
		t.Fatalf("id selector = %+v, %v", resolved, err)
	}
	if resolved, err := srv.resolveLiveSwitchTarget(ctx, strconv.FormatInt(first.Number, 10)); err != nil || resolved.SessionID != first.ID {
		t.Fatalf("number selector = %+v, %v", resolved, err)
	}
	if resolved, err := srv.resolveLiveSwitchTarget(ctx, "#"+strconv.FormatInt(archived.Number, 10)); err != nil || !resolved.Archived {
		t.Fatalf("hash number selector = %+v, %v", resolved, err)
	}
	// A truncated id is a mishearing, not a selector. Every session id is
	// timestamp-prefixed, so a prefix matches many sessions and the store's
	// GetByPrefix has no uniqueness check: resolving one would silently bind the
	// call to whichever matching session happened to be newest.
	for name, selector := range map[string]string{
		"truncated id":          "20260917-1300",
		"truncated first id":    "20260917-120000-abcdef",
		"timestamp":             "20260917",
		"title":                 "Reflow crash",
		"short prefix":          "2026",
		"unknown":               "20260917-140000-0000000000000000",
		"empty":                 "   ",
		"subagent":              "subagent-child",
		"blank-ish":             "#",
		"unknown hash selector": "#99999999",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := srv.resolveLiveSwitchTarget(ctx, selector); !errors.Is(err, errLiveSwitchInvalidTarget) {
				t.Fatalf("selector %q = %v", selector, err)
			}
		})
	}
	// A parented fork is excluded by the same durable parent link the directory's
	// ExcludeSubagents filter uses, but it is not a subagent, so the refusal must
	// not claim it is one.
	createDirectorySession(t, store, &session.Session{ID: "fork-child", ParentID: first.ID})
	if _, err := srv.resolveLiveSwitchTarget(ctx, "fork-child"); !errors.Is(err, errLiveSwitchInvalidTarget) ||
		!strings.Contains(err.Error(), "child session") || strings.Contains(err.Error(), "is a subagent") {
		t.Fatalf("fork selector = %v", err)
	}
	if _, err := (&serveServer{}).resolveLiveSwitchTarget(ctx, "1"); err == nil {
		t.Fatal("missing store accepted a selector")
	}
}

func TestLiveSwitchSessionEndpoint(t *testing.T) {
	store := newSessionDirectoryTestStore(t)
	ctx := context.Background()
	createDirectorySession(t, store, &session.Session{ID: "switch-target", GeneratedShortTitle: "Fix reflow crash"})
	createDirectorySession(t, store, &session.Session{ID: "switch-archived", Summary: "Old work", Archived: true})
	createDirectorySession(t, store, &session.Session{ID: "switch-other", Summary: "Other call"})
	createDirectorySession(t, store, &session.Session{ID: "switch-child", ParentID: "switch-target", IsSubagent: true})
	target, err := store.Get(ctx, "switch-target")
	if err != nil || target == nil {
		t.Fatal(err)
	}

	srv, providerSession := newStubLiveHarness(t)
	srv.store = store
	liveID, sessionID := startStubLiveCall(t, srv, "switch-source")
	if sessionID != "switch-source" {
		t.Fatalf("started session = %q", sessionID)
	}
	second := newLiveSession("call-other", "switch-other")
	if err := srv.registerLiveSession(second); err != nil {
		t.Fatal(err)
	}

	post := func(liveID, body string) (int, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions/"+liveID+"/session", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		srv.handleLiveSessionByID(recorder, request)
		var decoded map[string]any
		if recorder.Body.Len() > 0 {
			_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
		}
		return recorder.Code, decoded
	}

	status, body := post(liveID, `{"session_id":"switch-other"}`)
	if status != http.StatusConflict {
		t.Fatalf("busy target status = %d, body = %v", status, body)
	}
	if status, body := post("live_unknown", `{"session_id":"switch-target"}`); status != http.StatusNotFound {
		t.Fatalf("unknown live id status = %d, body = %v", status, body)
	}
	for name, payload := range map[string]string{
		"subagent":   `{"session_id":"switch-child"}`,
		"unknown":    `{"session_id":"no-such-session"}`,
		"missing":    `{}`,
		"malformed":  `{`,
		"empty body": ``,
	} {
		t.Run(name, func(t *testing.T) {
			if status, body := post(liveID, payload); status != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %v", status, body)
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/live/sessions/"+liveID+"/session", nil)
	recorder := httptest.NewRecorder()
	srv.handleLiveSessionByID(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method status = %d", recorder.Code)
	}

	status, body = post(liveID, `{"session_id":"`+strconv.FormatInt(target.Number, 10)+`"}`)
	if status != http.StatusOK {
		t.Fatalf("switch status = %d, body = %v", status, body)
	}
	if body["live_id"] != liveID || body["session_id"] != "switch-target" || body["title"] != "Fix reflow crash" || body["no_op"] != false {
		t.Fatalf("switch body = %v", body)
	}
	if number, ok := body["session_number"].(float64); !ok || int64(number) != target.Number {
		t.Fatalf("switch session_number = %#v", body["session_number"])
	}
	record, ok := srv.lookupLiveSession(liveID)
	if !ok || record.boundSession() != "switch-target" {
		t.Fatalf("binding = %+v", record)
	}
	if events := liveSessionChangedEvents(record); len(events) != 1 || events[0].Data["session_id"] != "switch-target" {
		t.Fatalf("session_changed events = %+v", events)
	}
	if note := providerSession.lastAppendedText(); !strings.Contains(note, "switch-target") || !strings.Contains(note, "now bound") {
		t.Fatalf("binding note = %q", note)
	}

	status, body = post(liveID, `{"session_id":"switch-target"}`)
	if status != http.StatusOK || body["no_op"] != true {
		t.Fatalf("no-op switch status = %d, body = %v", status, body)
	}
	if events := liveSessionChangedEvents(record); len(events) != 1 {
		t.Fatalf("no-op published %d events", len(events))
	}
	if status, body := post(liveID, `{"session_id":"switch-archived"}`); status != http.StatusOK || body["session_id"] != "switch-archived" {
		t.Fatalf("archived switch status = %d, body = %v", status, body)
	}
}

// startStubLiveCall starts a call through the real handler so the provider
// session and controller are wired exactly as in production.
func startStubLiveCall(t *testing.T, srv *serveServer, sessionID string) (liveID, bound string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"session_id": sessionID, "sdp": browserOffer})
	request := httptest.NewRequest(http.MethodPost, "/v1/live/sessions", strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.handleLiveSessions(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start call status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	liveID, _ = decoded["live_id"].(string)
	bound, _ = decoded["session_id"].(string)
	if liveID == "" {
		t.Fatalf("start call body = %v", decoded)
	}
	return liveID, bound
}

func TestLiveSwitchSessionIsRefusedAfterCallEnds(t *testing.T) {
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: "ended-target", Summary: "Target"})
	srv, _ := newStubLiveHarness(t)
	srv.store = store
	record := newLiveSession("call-ended-switch", "ended-source")
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	srv.stopLiveSession(context.Background(), record.id, "user")
	if _, err := srv.switchLiveSession(context.Background(), record, "ended-target"); !errors.Is(err, errLiveCallEnded) {
		t.Fatalf("switch after end = %v", err)
	}
	if record.boundSession() != "ended-source" {
		t.Fatalf("ended call moved to %q", record.boundSession())
	}
}

// TestLiveDelegationAfterRebindUsesTheNewSessionRuntime is the guard against a
// delegator that caches the binding: the delegator is built before the rebind and
// the delegation that follows must resolve the new session's runtime.
func TestLiveDelegationAfterRebindUsesTheNewSessionRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestServeServer()
	first, _, err := s.runtimeForRequest(ctx, "rebind-a")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.runtimeForRequest(ctx, "rebind-b")
	if err != nil {
		t.Fatal(err)
	}
	second.provider.(*llm.MockProvider).AddTextResponse("answer from b")
	record := newLiveSession("call-rebind-run", "rebind-a")
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	if err := s.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	d := &serveLiveDelegator{server: s, live: record}
	if _, err := s.rebindLiveSession(record, liveSwitchTarget{SessionID: "rebind-b", Number: 2, Title: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Run(ctx, live.DelegationRequest{Input: "do it in b"}, func(live.DelegationChunk) {}); err != nil {
		t.Fatal(err)
	}
	if len(second.provider.(*llm.MockProvider).RecordedRequests()) != 1 {
		t.Fatal("the rebound session's runtime did not receive the delegation")
	}
	if len(first.provider.(*llm.MockProvider).RecordedRequests()) != 0 {
		t.Fatal("the abandoned session's runtime received the delegation")
	}
	if run := s.ensureResponseRuns().latestRun("rebind-b"); run == nil {
		t.Fatal("no run registered for the rebound session")
	}
}

// inFlightLiveDelegation is a delegation blocked inside a tool after a concurrent
// rebind, which is the state both in-flight requirements are stated over.
type inFlightLiveDelegation struct {
	record    *liveSession
	delegator *serveLiveDelegator
	started   *serveRuntime
	elsewhere *llm.MockProvider
	gate      *liveSteeringGateTool
	done      chan error
}

// startInFlightLiveDelegation starts a delegation that blocks inside a tool, so a
// test can rebind the call while the turn is genuinely in flight.
func startInFlightLiveDelegation(t *testing.T, s *serveServer, sessionID string) *inFlightLiveDelegation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	started, _, err := s.runtimeForRequest(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, _, err := s.runtimeForRequest(ctx, "inflight-b")
	if err != nil {
		t.Fatal(err)
	}
	gate := &liveSteeringGateTool{entered: make(chan struct{}), release: make(chan struct{})}
	started.engine.RegisterTool(gate)
	started.provider.(*llm.MockProvider).
		AddToolCall("gate", "live_gate", map[string]any{}).
		AddTextResponse("done in a")
	record := newLiveSession("call-inflight", sessionID)
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	record.voiceSession = &liveVoiceControlStub{voice: "cove"}
	if err := s.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	delegator := &serveLiveDelegator{server: s, live: record}
	done := make(chan error, 1)
	go func() {
		done <- delegator.Run(ctx, live.DelegationRequest{Input: "do the work"}, func(live.DelegationChunk) {})
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal("the delegation never reached its tool")
	}
	// The call moves elsewhere while the delegation is in flight. The rebind must
	// not detach the run from the session it belongs to or re-register it under the
	// new one: that is the state the two in-flight requirements are stated over.
	if _, err := s.rebindLiveSession(record, liveSwitchTarget{SessionID: "inflight-b", Number: 5, Title: "Elsewhere"}); err != nil {
		t.Fatal(err)
	}
	if run := s.ensureResponseRuns().activeRun(sessionID); run == nil {
		t.Fatal("the rebind detached the in-flight run from its session")
	}
	if run := s.ensureResponseRuns().activeRun("inflight-b"); run != nil {
		t.Fatal("the rebind moved the in-flight run to the new session")
	}
	return &inFlightLiveDelegation{
		record: record, delegator: delegator, started: started,
		elsewhere: elsewhere.provider.(*llm.MockProvider), gate: gate, done: done,
	}
}

// TestLiveInFlightDelegationKeepsItsBindingAcrossRebind pins the plan's
// requirement that one delegation is atomic with respect to a concurrent switch:
// the turn that was already running finishes in the session it started in, on
// that session's runtime, while the switch applies to the next turn.
func TestLiveInFlightDelegationKeepsItsBindingAcrossRebind(t *testing.T) {
	s := newTestServeServer()
	inFlight := startInFlightLiveDelegation(t, s, "inflight-a")
	close(inFlight.gate.release)
	if err := <-inFlight.done; err != nil {
		t.Fatalf("in-flight delegation failed across a rebind: %v", err)
	}
	if inFlight.record.boundSession() != "inflight-b" {
		t.Fatalf("binding = %q", inFlight.record.boundSession())
	}
	requests := inFlight.started.provider.(*llm.MockProvider).RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("the in-flight run's provider turns = %d, want 2 in the session it started in", len(requests))
	}
	if requests := inFlight.elsewhere.RecordedRequests(); len(requests) != 0 {
		t.Fatal("the rebind moved an in-flight delegation to the new session")
	}
	if run := s.ensureResponseRuns().latestRun("inflight-a"); run == nil {
		t.Fatal("the run was not owned by the session the delegation started in")
	}
}

// TestLiveSteerSessionTargetsItsArgumentNotTheCurrentBinding pins the §2 split:
// Steer snapshots the binding once and steerSession acts on whichever session its
// caller resolved, so a delegation that is already in flight cannot be steered by
// a request that arrived after the call moved elsewhere.
func TestLiveSteerSessionTargetsItsArgumentNotTheCurrentBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := newTestServeServer()
	inFlight := startInFlightLiveDelegation(t, s, "inflight-steer")

	// The controller's direct path snapshots the binding, which has already moved.
	admitted, err := inFlight.delegator.Steer(ctx, live.DelegationRequest{ID: "late", Input: "late guidance"})
	if err != nil || admitted {
		t.Fatalf("Steer reached the abandoned session: admitted=%v, err=%v", admitted, err)
	}
	// The session the delegation actually started in still accepts guidance.
	admitted, err = inFlight.delegator.steerSession(ctx, "inflight-steer", live.DelegationRequest{ID: "in-flight", Input: "in-flight guidance"})
	if err != nil || !admitted {
		t.Fatalf("in-flight steering = %v, %v", admitted, err)
	}
	if pending := inFlight.started.engine.ListPendingSteering(); len(pending) != 1 || pending[0].DisplayText != "in-flight guidance" {
		t.Fatalf("pending steering = %+v", pending)
	}
	close(inFlight.gate.release)
	if err := <-inFlight.done; err != nil {
		t.Fatalf("in-flight delegation failed across a rebind: %v", err)
	}
}

// TestLiveRebindResetsAbandonedSessionPlatformContext guards §1a: after the call
// switches away, a typed turn in the session it left must receive the explicit
// live-mode reset instead of "Live voice mode is active".
func TestLiveRebindResetsAbandonedSessionPlatformContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newTestServeServer()
	runtime, _, err := s.runtimeForRequest(ctx, "abandoned-a")
	if err != nil {
		t.Fatal(err)
	}
	runtime.platform = "web"
	runtime.platformMessages.Web = "web presentation rules"
	runtime.provider.(*llm.MockProvider).AddTextResponse("delegated answer")
	record := newLiveSession("call-abandoned", "abandoned-a")
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	if err := s.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	delegator := &serveLiveDelegator{server: s, live: record}
	if err := delegator.Run(ctx, live.DelegationRequest{Input: "do the work"}, func(live.DelegationChunk) {}); err != nil {
		t.Fatal(err)
	}
	announced, ok := llm.PlatformContextFrom(runtime.history)
	if !ok || llm.PlatformContextMode(announced) != "live" {
		t.Fatalf("delegation did not announce live mode: %+v", announced)
	}

	if _, err := s.rebindLiveSession(record, liveSwitchTarget{SessionID: "abandoned-b", Number: 2, Title: "Elsewhere"}); err != nil {
		t.Fatal(err)
	}
	typed := func() llm.Message {
		t.Helper()
		runtime.provider.(*llm.MockProvider).AddTextResponse("typed answer")
		if _, err := runtime.Run(ctx, true, false, []llm.Message{llm.UserText("typed request")}, llm.Request{SessionID: "abandoned-a"}); err != nil {
			t.Fatal(err)
		}
		message, ok := llm.PlatformContextFrom(runtime.history)
		if !ok {
			t.Fatal("typed turn injected no platform context")
		}
		return message
	}
	reset := typed()
	// Byte-identical to the liveContext == nil reset branch of
	// preparePlatformContext (covered by TestLiveModeContextTransitionsAndRestart),
	// which is why an abandoned session recovers with no extra code.
	if llm.MessageText(reset) != "web presentation rules\n\n"+liveTextModeContext || llm.PlatformContextMode(reset) != "text" {
		t.Fatalf("abandoned session context = %q, mode = %q", llm.MessageText(reset), llm.PlatformContextMode(reset))
	}
	// The session the call left behind is inactive; the one it moved to is not.
	if text, active := record.executionContextFor("abandoned-a")(); active {
		t.Fatalf("abandoned session still reported active: %q", text)
	}
	if text, active := record.executionContextFor("abandoned-b")(); !active || text == liveTextModeContext {
		t.Fatalf("rebound session reported inactive: %q", text)
	}
}
