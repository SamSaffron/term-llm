package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

type serveEventChangeStoreStub struct {
	session.Store
}

func (*serveEventChangeStoreStub) StoreChangeCursor(context.Context) (int64, error) {
	return 0, nil
}

func (*serveEventChangeStoreStub) ListStoreChanges(context.Context, int64, int) ([]session.StoreChange, error) {
	return nil, nil
}

func TestEventWatcherConcurrentLifecycle(t *testing.T) {
	s := &serveServer{store: &serveEventChangeStoreStub{}, eventBroker: newServeEventBroker()}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.startEventWatcher()
		}()
		go func() {
			defer wg.Done()
			s.stopEventWatcher()
		}()
	}
	wg.Wait()
	s.stopEventWatcher()
}

func TestServeEventBrokerReplayAndCursorBounds(t *testing.T) {
	broker := newServeEventBroker()
	first, err := broker.Publish(serveEventInput{Type: serveEventSessionCreated, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := broker.Publish(serveEventInput{Type: serveEventRunStarted, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	after := first.Sequence
	subscription, err := broker.Subscribe(&after)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Unsubscribe(subscription.ID)
	if len(subscription.Replay) != 1 || subscription.Replay[0].Sequence != second.Sequence {
		t.Fatalf("replay = %#v, want sequence %d", subscription.Replay, second.Sequence)
	}

	future := second.Sequence + 1
	_, err = broker.Subscribe(&future)
	var cursorErr *serveEventCursorError
	if !errors.As(err, &cursorErr) || cursorErr.Latest != second.Sequence {
		t.Fatalf("future cursor error = %#v, want latest %d", err, second.Sequence)
	}
}

func TestServeEventBrokerDropsSlowSubscriberWithoutBlocking(t *testing.T) {
	broker := newServeEventBroker()
	subscription, err := broker.Subscribe(nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < serveEventSubscriberBuffer+1; i++ {
		if _, err := broker.Publish(serveEventInput{Type: serveEventRunStarted, SessionID: "s1"}); err != nil {
			t.Fatal(err)
		}
	}
	for range subscription.Events {
	}
	broker.Close()
	broker.Close()
	if _, err := broker.Publish(serveEventInput{Type: serveEventRunStarted}); !errors.Is(err, errServeEventBrokerClosed) {
		t.Fatalf("publish after close error = %v", err)
	}
}

func TestServeEventPollFiltersUnregisteredDetailChannelsAndAdvancesCursor(t *testing.T) {
	broker := newServeEventBroker()
	s := &serveServer{eventBroker: broker}
	_, _ = broker.Publish(serveEventInput{Type: serveEventFilesChanged, SessionID: "other"})
	_, _ = broker.Publish(serveEventInput{Type: serveEventRunStarted, SessionID: "other"})
	_, _ = broker.Publish(serveEventInput{Type: serveEventFilesChanged, SessionID: "wanted"})

	req := httptest.NewRequest(http.MethodGet, "/v1/events/poll?after=0&channels=files:wanted", nil)
	recorder := httptest.NewRecorder()
	s.handleEventPoll(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response serveEventPollResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.NextAfter != 3 {
		t.Fatalf("next_after = %d, want 3", response.NextAfter)
	}
	if len(response.Data) != 2 || response.Data[0].Type != serveEventRunStarted || response.Data[1].SessionID != "wanted" {
		t.Fatalf("filtered data = %#v", response.Data)
	}
	for _, event := range response.Data {
		if event.Type == serveEventFilesChanged && event.SessionID == "other" {
			t.Fatal("received file details for an unregistered session")
		}
	}
}

func TestCoalesceObservedStoreChangesBoundsTranscriptInvalidations(t *testing.T) {
	changes := []session.StoreChange{
		{Sequence: 1, Kind: session.StoreChangeSessionTranscriptChanged, SessionID: "s1", TranscriptRev: 1},
		{Sequence: 2, Kind: session.StoreChangeSessionTranscriptChanged, SessionID: "s1", TranscriptRev: 2},
		{Sequence: 3, Kind: session.StoreChangeSessionStatusChanged, SessionID: "s1", Status: session.StatusActive},
		{Sequence: 4, Kind: session.StoreChangeSessionStatusChanged, SessionID: "s1", Status: session.StatusComplete},
		{Sequence: 5, Kind: session.StoreChangeSessionMetadataChanged, SessionID: "s2"},
		{Sequence: 6, Kind: session.StoreChangeSessionDeleted, SessionID: "s2"},
	}
	got := coalesceObservedStoreChanges(changes)
	if len(got) != 4 {
		t.Fatalf("coalesced = %#v, want latest transcript, two statuses, and delete", got)
	}
	if got[0].Kind != session.StoreChangeSessionTranscriptChanged || got[0].TranscriptRev != 2 {
		t.Fatalf("coalesced transcript = %#v", got[0])
	}
	if got[1].Status != session.StatusActive || got[2].Status != session.StatusComplete || got[3].Kind != session.StoreChangeSessionDeleted {
		t.Fatalf("coalesced transitions = %#v", got)
	}
}

func TestPublishObservedStoreChangeMapsDurableTransitions(t *testing.T) {
	broker := newServeEventBroker()
	s := &serveServer{eventBroker: broker}

	s.publishObservedStoreChange(session.StoreChange{
		Kind: session.StoreChangeSessionCreated, SessionID: "s1", ProjectID: "p1", Status: session.StatusActive,
	})
	s.publishObservedStoreChange(session.StoreChange{
		Kind: session.StoreChangeSessionTranscriptChanged, SessionID: "s1", TranscriptRev: 9,
	})
	s.publishObservedStoreChange(session.StoreChange{
		Kind: session.StoreChangeSessionStatusChanged, SessionID: "s1", TranscriptRev: 9, Status: session.StatusComplete,
	})

	broker.mu.Lock()
	events := append([]serveEvent(nil), broker.events...)
	broker.mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("events = %#v, want created, started, transcript, finished", events)
	}
	want := []string{serveEventSessionCreated, serveEventRunStarted, serveEventSessionTranscriptChanged, serveEventRunFinished}
	for i, event := range events {
		if event.Type != want[i] {
			t.Fatalf("event %d type = %q, want %q", i, event.Type, want[i])
		}
	}
	if events[2].TranscriptRev != 9 || events[3].Reason != "completed" {
		t.Fatalf("mapped events = %#v", events)
	}
}

func TestServeEventsSendsFlushedReadyControl(t *testing.T) {
	s := &serveServer{eventBroker: newServeEventBroker()}
	server := httptest.NewServer(http.HandlerFunc(s.handleEvents))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"?channels=session:s1", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	reader := bufio.NewReader(response.Body)
	deadline := time.After(2 * time.Second)
	lines := ""
	for !strings.Contains(lines, "\n\n") {
		line := make(chan string, 1)
		go func() {
			value, _ := reader.ReadString('\n')
			line <- value
		}()
		select {
		case value := <-line:
			lines += value
		case <-deadline:
			t.Fatal("ready event was not flushed")
		}
	}
	if !strings.Contains(lines, "event: ready") || !strings.Contains(lines, "instance_id") {
		t.Fatalf("ready event = %q", lines)
	}
	cancel()
}
