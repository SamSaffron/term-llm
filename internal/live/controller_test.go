package live

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type appendRecord struct {
	DelegationID string
	Chunk        DelegationChunk
}

type fakeSession struct {
	events chan Event

	mu      sync.Mutex
	appends []appendRecord
	texts   []string
	closed  bool
}

func newFakeSession() *fakeSession {
	return &fakeSession{events: make(chan Event, 16)}
}

func (f *fakeSession) AnswerSDP() string    { return "v=0" }
func (f *fakeSession) Events() <-chan Event { return f.events }
func (f *fakeSession) emit(event Event)     { f.events <- event }

// finish ends the event stream the way a provider session does.
func (f *fakeSession) finish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
}

func (f *fakeSession) Close(context.Context) error {
	f.finish()
	return nil
}

func (f *fakeSession) AppendDelegation(_ context.Context, delegationID string, chunk DelegationChunk) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appends = append(f.appends, appendRecord{DelegationID: delegationID, Chunk: chunk})
	return nil
}

func (f *fakeSession) AppendText(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return nil
}

func (f *fakeSession) speakable() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, record := range f.appends {
		if record.Chunk.Channel == ChannelSpeakable {
			b.WriteString(record.Chunk.Text)
		}
	}
	return b.String()
}

func (f *fakeSession) commentary() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, record := range f.appends {
		if record.Chunk.Channel == ChannelCommentary {
			out = append(out, record.Chunk.Text)
		}
	}
	return out
}

type fakeDelegator struct {
	mu       sync.Mutex
	requests []DelegationRequest
	run      func(attempt int, emit func(DelegationChunk)) error
}

func (d *fakeDelegator) Run(_ context.Context, request DelegationRequest, emit func(DelegationChunk)) error {
	d.mu.Lock()
	d.requests = append(d.requests, request)
	attempt := len(d.requests)
	d.mu.Unlock()
	if d.run == nil {
		return nil
	}
	return d.run(attempt, emit)
}

func (d *fakeDelegator) lastRequest() DelegationRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.requests) == 0 {
		return DelegationRequest{}
	}
	return d.requests[len(d.requests)-1]
}

func (d *fakeDelegator) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

func (d *fakeDelegator) allRequests() []DelegationRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DelegationRequest(nil), d.requests...)
}

type updateRecorder struct {
	mu      sync.Mutex
	updates []Update
}

func (r *updateRecorder) observe(update Update) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, update)
}

func (r *updateRecorder) states() []DelegationState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []DelegationState
	for _, update := range r.updates {
		if update.Kind == UpdateDelegation {
			out = append(out, update.State)
		}
	}
	return out
}

func startController(t *testing.T, session *fakeSession, delegator Delegator, recorder *updateRecorder) *Controller {
	t.Helper()
	controller := NewController(ControllerOptions{
		Session:       session,
		Delegator:     delegator,
		Observer:      recorder.observe,
		FlushInterval: 5 * time.Millisecond,
		BusyRetry:     5 * time.Millisecond,
		BusyRetries:   3,
	})
	controller.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = controller.Close(ctx)
	})
	return controller
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestControllerRunsDelegationAndSpeaksTheResult(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{run: func(_ int, emit func(DelegationChunk)) error {
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: "Found "})
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: "three files."})
		return nil
	}}
	recorder := &updateRecorder{}
	startController(t, session, delegator, recorder)

	session.emit(Event{Kind: EventTurnDone, Role: RoleUser, Text: "list the files"})
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "item_1", Text: "list the files"})

	waitFor(t, "the delegation output", func() bool { return session.speakable() == "Found three files." })

	request := delegator.lastRequest()
	if request.Input != "list the files" {
		t.Fatalf("request input = %q", request.Input)
	}
	if request.TranscriptDelta != "user: list the files" {
		t.Fatalf("request transcript delta = %q", request.TranscriptDelta)
	}
	waitFor(t, "the done update", func() bool {
		states := recorder.states()
		return len(states) == 3 && states[0] == DelegationQueued && states[1] == DelegationRunning && states[2] == DelegationDone
	})
}

func TestControllerFallsBackToSpokenTranscriptForEmptyDelegations(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{}
	startController(t, session, delegator, &updateRecorder{})

	session.emit(Event{Kind: EventUserTranscript, Role: RoleUser, Text: "run the "})
	session.emit(Event{Kind: EventUserTranscript, Role: RoleUser, Text: "tests"})
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "item_1"})

	waitFor(t, "the delegated prompt", func() bool { return delegator.count() == 1 })
	if request := delegator.lastRequest(); request.Input != "run the tests" {
		t.Fatalf("request = %+v", request)
	}
}

func TestControllerRunsDelegationsOneAtATime(t *testing.T) {
	session := newFakeSession()
	var concurrent, peak atomic.Int32
	delegator := &fakeDelegator{run: func(_ int, emit func(DelegationChunk)) error {
		current := concurrent.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: "ok"})
		concurrent.Add(-1)
		return nil
	}}
	startController(t, session, delegator, &updateRecorder{})

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "item_1", Text: "first"})
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "item_2", Text: "second"})

	waitFor(t, "both delegations", func() bool { return delegator.count() == 2 })
	waitFor(t, "the second answer", func() bool { return session.speakable() == "okok" })
	if peak.Load() != 1 {
		t.Fatalf("delegations overlapped: peak concurrency = %d", peak.Load())
	}
}

func TestControllerSpeaksDelegationFailures(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{run: func(_ int, _ func(DelegationChunk)) error {
		return errors.New("the tool exploded")
	}}
	recorder := &updateRecorder{}
	startController(t, session, delegator, recorder)

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "item_1", Text: "do it"})

	waitFor(t, "the spoken failure", func() bool {
		return strings.Contains(session.speakable(), "The task failed: the tool exploded")
	})
	waitFor(t, "the failed update", func() bool {
		states := recorder.states()
		return len(states) > 0 && states[len(states)-1] == DelegationFailed
	})
}

func TestControllerKeepsDelegationQueuedWhileTheSessionIsBusy(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{run: func(attempt int, emit func(DelegationChunk)) error {
		if attempt == 1 {
			return fmt.Errorf("web turn active: %w", ErrDelegationBusy)
		}
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: "done"})
		return nil
	}}
	startController(t, session, delegator, &updateRecorder{})

	session.emit(Event{Kind: EventTurnDone, Role: RoleUser, Text: "do it now"})
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "item_1", Text: "do it"})

	waitFor(t, "the retry", func() bool { return session.speakable() == "done" })
	requests := delegator.allRequests()
	if len(requests) != 2 || requests[0] != requests[1] || requests[0].Input != "do it" || requests[0].TranscriptDelta != "user: do it now" {
		t.Fatalf("requests changed across retry: %+v", requests)
	}
	commentary := session.commentary()
	if len(commentary) != 1 || !strings.Contains(commentary[0], "previous request") {
		t.Fatalf("commentary = %v", commentary)
	}
}

func TestControllerTruncatesRunawayDelegationOutput(t *testing.T) {
	session := newFakeSession()
	head := strings.Repeat("h", delegationHeadBytes)
	middle := strings.Repeat("m", 5000)
	tail := strings.Repeat("t", delegationTailBytes)
	delegator := &fakeDelegator{run: func(_ int, emit func(DelegationChunk)) error {
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: head})
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: middle})
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: tail})
		return nil
	}}
	startController(t, session, delegator, &updateRecorder{})

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "item_1", Text: "dump everything"})

	waitFor(t, "the truncated output", func() bool {
		return strings.Contains(session.speakable(), truncationMarker)
	})
	spoken := session.speakable()
	if !strings.HasPrefix(spoken, head) {
		t.Fatal("the head of the output was not spoken")
	}
	if !strings.HasSuffix(spoken, tail) {
		t.Fatal("the tail of the output was not spoken")
	}
	if strings.Contains(spoken, strings.Repeat("m", 3000)) {
		t.Fatal("the middle of a runaway output should be dropped")
	}
}

func TestControllerPrefixesTypedUserText(t *testing.T) {
	session := newFakeSession()
	controller := startController(t, session, &fakeDelegator{}, &updateRecorder{})

	if err := controller.AppendUserText(context.Background(), "  open main.go  "); err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.texts) != 1 || session.texts[0] != "[USER] open main.go" {
		t.Fatalf("texts = %v", session.texts)
	}
}

func TestControllerReportsSessionEnd(t *testing.T) {
	session := newFakeSession()
	recorder := &updateRecorder{}
	startController(t, session, &fakeDelegator{}, recorder)

	session.emit(Event{Kind: EventError, Text: "provider said no"})
	session.finish()

	waitFor(t, "the ended update", func() bool {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		sawError, sawEnd := false, false
		for _, update := range recorder.updates {
			switch update.Kind {
			case UpdateError:
				sawError = update.Text == "provider said no"
			case UpdateEnded:
				sawEnd = true
			}
		}
		return sawError && sawEnd
	})
}

func TestControllerDoesNotFailCallForErrorAlreadyDeliveredToTool(t *testing.T) {
	var updates []Update
	controller := NewController(ControllerOptions{Observer: func(update Update) { updates = append(updates, update) }})
	if controller.handleEvent(Event{Kind: EventError, Text: "voice change rejected", ErrorHandled: true}) {
		t.Fatal("recoverable tool error ended the call")
	}
	if len(updates) != 0 {
		t.Fatalf("tool rejection also failed the conversational UI: %+v", updates)
	}
	controller.handleEvent(Event{Kind: EventError, Text: "unhandled provider error"})
	if len(updates) != 1 || updates[0].Kind != UpdateError {
		t.Fatalf("unhandled provider error was lost: %+v", updates)
	}
}

type steeringTestDelegator struct {
	started    chan struct{}
	release    chan struct{}
	steered    chan DelegationRequest
	calls      atomic.Int32
	steerError error
	notReady   atomic.Bool
}

func (d *steeringTestDelegator) Run(ctx context.Context, _ DelegationRequest, emit func(DelegationChunk)) error {
	d.calls.Add(1)
	close(d.started)
	select {
	case <-d.release:
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: "one answer"})
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (d *steeringTestDelegator) Steer(_ context.Context, request DelegationRequest) (bool, error) {
	if d.notReady.Load() {
		return false, nil
	}
	select {
	case <-d.started:
		d.steered <- request
		return true, d.steerError
	default:
		return false, nil
	}
}
func TestControllerSteersBlockedRunWithoutDuplicateOutput(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			session := newFakeSession()
			d := &steeringTestDelegator{started: make(chan struct{}), release: make(chan struct{}), steered: make(chan DelegationRequest, 4)}
			if fail {
				d.steerError = errors.New("admission rejected")
			}
			recorder := &updateRecorder{}
			c := startController(t, session, d, recorder)
			session.emit(Event{Kind: EventDelegationCreated, DelegationID: "first", Text: "inspect the change"})
			select {
			case <-d.started:
			case <-time.After(time.Second):
				t.Fatal("run did not start")
			}
			session.emit(Event{Kind: EventDelegationCreated, DelegationID: "correction", Text: "actually a changeset"})
			session.emit(Event{Kind: EventDelegationCreated, DelegationID: "correction", Text: "actually a changeset"})
			select {
			case request := <-d.steered:
				if request.ID != "correction" || request.Input != "actually a changeset" {
					t.Fatalf("request = %+v", request)
				}
			case <-time.After(time.Second):
				t.Fatal("correction waited for blocked run")
			}
			waitFor(t, "guidance completion", func() bool {
				recorder.mu.Lock()
				defer recorder.mu.Unlock()
				want := DelegationDone
				if fail {
					want = DelegationFailed
				}
				for _, u := range recorder.updates {
					if u.DelegationID == "correction" && u.State == want {
						return true
					}
				}
				return false
			})
			close(d.release)
			session.finish()
			if err := c.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if d.calls.Load() != 1 {
				t.Fatalf("runs = %d", d.calls.Load())
			}
			if len(d.steered) != 0 {
				t.Fatal("duplicate steering admitted")
			}
			if strings.Count(session.speakable(), "one answer") != 1 {
				t.Fatalf("output = %q", session.speakable())
			}
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			for _, u := range recorder.updates {
				if u.Kind == UpdateError {
					t.Fatalf("guidance failed call: %+v", u)
				}
			}
		})
	}
}

func TestControllerCloseCancelsBlockedSteeringRun(t *testing.T) {
	session := newFakeSession()
	d := &steeringTestDelegator{started: make(chan struct{}), release: make(chan struct{}), steered: make(chan DelegationRequest, 1)}
	c := startController(t, session, d, &updateRecorder{})
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "blocked", Text: "work"})
	select {
	case <-d.started:
	case <-time.After(time.Second):
		t.Fatal("run did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- c.Close(context.Background()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close leaked blocked delegation")
	}
}

func TestControllerRetriesSteeringDuringRuntimeStartup(t *testing.T) {
	session := newFakeSession()
	d := &steeringTestDelegator{started: make(chan struct{}), release: make(chan struct{}), steered: make(chan DelegationRequest, 1)}
	d.notReady.Store(true)
	recorder := &updateRecorder{}
	c := startController(t, session, d, recorder)
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "task", Text: "work"})
	select {
	case <-d.started:
	case <-time.After(time.Second):
		t.Fatal("run did not start")
	}
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "correction", Text: "guide"})
	waitFor(t, "queued correction", func() bool {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		for _, u := range recorder.updates {
			if u.DelegationID == "correction" && u.State == DelegationQueued {
				return true
			}
		}
		return false
	})
	d.notReady.Store(false)
	select {
	case <-d.steered:
	case <-time.After(time.Second):
		t.Fatal("guidance was stranded until run completion")
	}
	if d.calls.Load() != 1 {
		t.Fatal("started a second output stream")
	}
	close(d.release)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
