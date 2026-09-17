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

// statesFor reports the lifecycle one delegation id went through, in order.
func (r *updateRecorder) statesFor(delegationID string) []DelegationState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []DelegationState
	for _, update := range r.updates {
		if update.Kind == UpdateDelegation && update.DelegationID == delegationID {
			out = append(out, update.State)
		}
	}
	return out
}

// terminalFor reports the update that ended one delegation — done, refused, or
// failed — which is where its state and any user-facing text are stated.
func (r *updateRecorder) terminalFor(delegationID string) (Update, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.updates) - 1; i >= 0; i-- {
		update := r.updates[i]
		if update.Kind != UpdateDelegation || update.DelegationID != delegationID {
			continue
		}
		switch update.State {
		case DelegationDone, DelegationRefused, DelegationFailed:
			return update, true
		}
	}
	return Update{}, false
}

// transcriptContains reports whether any published transcript text carries a
// fragment. It is how a test observes that the event loop is still delivering
// deltas while something else is blocked.
func (r *updateRecorder) transcriptContains(fragment string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, update := range r.updates {
		if update.Kind == UpdateTranscript && strings.Contains(update.Text, fragment) {
			return true
		}
	}
	return false
}

func startController(t *testing.T, session *fakeSession, delegator Delegator, recorder *updateRecorder) *Controller {
	t.Helper()
	return startControllerWith(t, session, ControllerOptions{Delegator: delegator}, recorder)
}

// startControllerWith starts a controller with extra options, such as a control
// lane, merged over the test defaults.
func startControllerWith(t *testing.T, session *fakeSession, opts ControllerOptions, recorder *updateRecorder) *Controller {
	t.Helper()
	opts.Session = session
	opts.Observer = recorder.observe
	opts.FlushInterval = 5 * time.Millisecond
	opts.BusyRetry = 5 * time.Millisecond
	opts.BusyRetries = 3
	controller := NewController(opts)
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
		// The host announces the request as queued the moment it has it — nothing is
		// executing it yet, it is waiting to be routed — and the session lane
		// announces `running` when it actually starts the turn. The two states are
		// not decoration: a request waiting behind a long task stays `queued`, which
		// is the only way the panel can tell waiting apart from working.
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

func TestControllerDefersMetadataOnlyDelegationUntilTranscriptAndDeduplicates(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{}
	controller := startController(t, session, delegator, &updateRecorder{})

	metadata := Event{Kind: EventDelegationCreated, RawType: "session.delegation.created", DelegationID: "item_live"}
	session.emit(metadata)
	session.emit(metadata)
	waitFor(t, "pending metadata-only delegation", func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return len(controller.pendingDelegations) == 1
	})
	if delegator.count() != 0 {
		t.Fatal("metadata-only delegation ran without transcript text")
	}

	session.emit(Event{Kind: EventAssistantTranscript, Role: RoleAssistant, Text: "I can help. "})
	session.emit(Event{Kind: EventUserTranscript, Role: RoleUser, Text: "run the tests"})
	waitFor(t, "deferred delegation", func() bool { return delegator.count() == 1 })
	request := delegator.lastRequest()
	if request.ID != "item_live" || request.Input != "run the tests" {
		t.Fatalf("request = %+v", request)
	}
	if !strings.Contains(request.TranscriptDelta, "user: run the tests") || !strings.Contains(request.TranscriptDelta, "assistant: I can help.") {
		t.Fatalf("transcript context = %q", request.TranscriptDelta)
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
	// A session-lane failure is not a refusal: something ran and went wrong, so the
	// cause stays user-facing along with the state.
	if terminal, ok := recorder.terminalFor("item_1"); !ok || !strings.Contains(terminal.Text, "the tool exploded") {
		t.Fatalf("session-lane failure lost its text: %+v", terminal)
	}
}

// TestControllerKeepsAWaitingDelegationQueuedUntilItRuns pins the distinction the
// delegation states exist for. A request that arrives while a task is running has
// been accepted and is executing nothing, and the panel can only say so if that
// request stays `queued` until its own turn starts.
//
// This regressed when routing moved off the event loop: `running` was published on
// arrival, so a request stuck behind a twenty-minute task looked exactly like one
// being worked on.
func TestControllerKeepsAWaitingDelegationQueuedUntilItRuns(t *testing.T) {
	session := newFakeSession()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	delegator := &fakeDelegator{run: func(attempt int, emit func(DelegationChunk)) error {
		if attempt == 1 {
			close(started)
			<-release
		}
		emit(DelegationChunk{Channel: ChannelSpeakable, Text: "done"})
		return nil
	}}
	recorder := &updateRecorder{}
	startController(t, session, delegator, recorder)
	// Registered after the controller's own cleanup so a failed assertion still
	// releases the blocked run before Close waits for the delegation loop.
	t.Cleanup(releaseAll)

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "task", Text: "the long one"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the first delegation never started")
	}

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "waiting", Text: "the next one"})
	waitFor(t, "the waiting delegation to be accepted", func() bool {
		return len(recorder.statesFor("waiting")) > 0
	})
	// Give the route worker room to publish anything further it was going to.
	time.Sleep(20 * time.Millisecond)
	if states := recorder.statesFor("waiting"); len(states) != 1 || states[0] != DelegationQueued {
		t.Fatalf("a delegation waiting behind a running task reported %v, want only [queued]", states)
	}

	releaseAll()
	waitFor(t, "the waiting delegation to run", func() bool {
		states := recorder.statesFor("waiting")
		return len(states) == 3 && states[0] == DelegationQueued && states[1] == DelegationRunning && states[2] == DelegationDone
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
	if controller.handleEvent(context.Background(), Event{Kind: EventError, Text: "voice change rejected", ErrorHandled: true}) {
		t.Fatal("recoverable tool error ended the call")
	}
	if len(updates) != 0 {
		t.Fatalf("tool rejection also failed the conversational UI: %+v", updates)
	}
	controller.handleEvent(context.Background(), Event{Kind: EventError, Text: "unhandled provider error"})
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
	waitFor(t, "retained correction", func() bool {
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		for _, u := range recorder.updates {
			// The host holds the request from the moment it arrives, and nothing is
			// executing it yet — the task it will steer is what is running — so the
			// state that says "this has not been lost" is `queued`. A steered
			// correction never reaches `running`: it is folded into the running task
			// rather than becoming a turn of its own.
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

type completingSession struct {
	*fakeSession
	completions        []string
	outputAtCompletion string
}

func (s *completingSession) CompleteDelegation(_ context.Context, id string) error {
	s.completions = append(s.completions, id)
	s.outputAtCompletion = s.speakable()
	return nil
}

func TestControllerExplicitDelegationCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		err        error
	}{
		{name: "success", text: "complete result"},
		{name: "empty"},
		{name: "failure", err: errors.New("tool failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := &completingSession{fakeSession: newFakeSession()}
			delegator := &fakeDelegator{run: func(_ int, emit func(DelegationChunk)) error {
				if tc.text != "" {
					emit(DelegationChunk{Channel: ChannelSpeakable, Text: tc.text})
				}
				return tc.err
			}}
			c := &Controller{session: session, delegator: delegator, flush: time.Hour, observe: func(Update) {}}
			c.runDelegation(context.Background(), queuedDelegation{ID: "call_test", Input: "do work"})
			if len(session.completions) != 1 || session.completions[0] != "call_test" {
				t.Fatalf("completions = %v", session.completions)
			}
			want := tc.text
			if tc.err != nil {
				want = delegationFailureText(tc.err)
			}
			if session.outputAtCompletion != want {
				t.Fatalf("output at completion = %q, want %q", session.outputAtCompletion, want)
			}
		})
	}
}

// fakeRouter records every delegation the controller offers it and answers with a
// scripted decision. Every delegation goes through this seam, so what it records is
// also the contract: the resolved input and the recent conversation the router is
// allowed to resolve a reference against.
type fakeRouter struct {
	mu       sync.Mutex
	requests []RouteRequest
	route    func(context.Context, RouteRequest) (RouteResult, error)
}

func (r *fakeRouter) Route(ctx context.Context, request RouteRequest) (RouteResult, error) {
	r.mu.Lock()
	r.requests = append(r.requests, request)
	run := r.route
	r.mu.Unlock()
	if run == nil {
		// A router with nothing scripted hands the request over untouched. That is
		// what it does with almost every request, and it is the only answer that
		// leaves the session lane's behaviour exactly as it was.
		return RouteResult{}, nil
	}
	return run(ctx, request)
}

func (r *fakeRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *fakeRouter) lastRequest() RouteRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return RouteRequest{}
	}
	return r.requests[len(r.requests)-1]
}

// TestControllerRoutesEveryDelegationThroughTheRouter pins the seam itself. Work is
// routed like everything else: the router is what tells "which sessions are running?"
// from "fix the parser", and it can only do that by reading the request. The
// transcript delta travels with it so a reference to something the user just heard
// ("the attachment icons one") can be resolved.
func TestControllerRoutesEveryDelegationThroughTheRouter(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{}
	router := &fakeRouter{}
	startControllerWith(t, session, ControllerOptions{Delegator: delegator, Router: router}, &updateRecorder{})

	session.emit(Event{Kind: EventTurnDone, Role: RoleUser, Text: "fix the reflow crash"})
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "work-1", Text: "fix the reflow crash"})

	waitFor(t, "the routed request", func() bool { return router.count() == 1 })
	if request := router.lastRequest(); request.Input != "fix the reflow crash" || request.TranscriptDelta != "user: fix the reflow crash" {
		t.Fatalf("router request = %+v", request)
	}
	waitFor(t, "the work", func() bool { return delegator.count() == 1 })
	if request := delegator.lastRequest(); request.Input != "fix the reflow crash" {
		t.Fatalf("session lane received %q", request.Input)
	}
}

// TestControllerAnswersHandledDelegationsWithoutTheSessionLane is the core
// regression for the whole feature: a request the router handles itself is answered
// as commentary with no chat turn, no run in the bound session, and no steering.
func TestControllerAnswersHandledDelegationsWithoutTheSessionLane(t *testing.T) {
	session := newFakeSession()
	delegator := &steeringTestDelegator{started: make(chan struct{}), release: make(chan struct{}), steered: make(chan DelegationRequest, 4)}
	router := &fakeRouter{route: func(context.Context, RouteRequest) (RouteResult, error) {
		return RouteResult{Handled: true, Answer: "Switched to the Discourse Backport session."}, nil
	}}
	recorder := &updateRecorder{}
	startControllerWith(t, session, ControllerOptions{Delegator: delegator, Router: router}, recorder)

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "control-1", Text: "Switch to the attachment icons one"})

	waitFor(t, "the router's answer", func() bool { return len(session.commentary()) == 1 })
	if text := session.commentary()[0]; text != "Switched to the Discourse Backport session." {
		t.Fatalf("commentary = %q", text)
	}
	if delegator.calls.Load() != 0 || len(delegator.steered) != 0 {
		t.Fatalf("a handled request reached the session lane: runs=%d steers=%d", delegator.calls.Load(), len(delegator.steered))
	}
	// The answer is context for the voice model, never a transcript entry the user
	// typed.
	if speakable := session.speakable(); speakable != "" {
		t.Fatalf("the answer was published as speakable text: %q", speakable)
	}
	waitFor(t, "the done update", func() bool {
		states := recorder.statesFor("control-1")
		return len(states) == 3 && states[0] == DelegationQueued && states[1] == DelegationRunning && states[2] == DelegationDone
	})
}

// TestControllerRunsTheTidiedRequestInTheSessionLane pins the one rewrite the router
// is allowed to make. It reads the request anyway, so it may repair speech
// recognition and drop filler on the way through — and what reaches the session lane
// is the tidied text, not the original.
func TestControllerRunsTheTidiedRequestInTheSessionLane(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{}
	router := &fakeRouter{route: func(context.Context, RouteRequest) (RouteResult, error) {
		return RouteResult{Input: "run the session store tests"}, nil
	}}
	startControllerWith(t, session, ControllerOptions{Delegator: delegator, Router: router}, &updateRecorder{})

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "work-1", Text: "um, uh, run the, um, session store tests"})

	waitFor(t, "the work", func() bool { return delegator.count() == 1 })
	if request := delegator.lastRequest(); request.Input != "run the session store tests" {
		t.Fatalf("session lane received %q, want the routed request", request.Input)
	}
}

// TestControllerFallsOpenWhenRoutingFails is the most important test in this file.
// The router sits in front of every delegation, so its failures must be invisible:
// an error and a host with no router at all each send the ORIGINAL request to the
// session lane. Losing or refusing the user's work because triage was unavailable is
// the one outcome this design must never have.
func TestControllerFallsOpenWhenRoutingFails(t *testing.T) {
	const original = "fix the reflow crash"
	for name, opts := range map[string]ControllerOptions{
		"router error": {Router: &fakeRouter{route: func(context.Context, RouteRequest) (RouteResult, error) {
			return RouteResult{}, errors.New("routing provider exploded")
		}}},
		"no router": {},
	} {
		t.Run(name, func(t *testing.T) {
			session := newFakeSession()
			delegator := &fakeDelegator{}
			options := opts
			options.Delegator = delegator
			startControllerWith(t, session, options, &updateRecorder{})

			session.emit(Event{Kind: EventDelegationCreated, DelegationID: "work-1", Text: original})

			waitFor(t, "the work", func() bool { return delegator.count() == 1 })
			if request := delegator.lastRequest(); request.Input != original {
				t.Fatalf("session lane received %q, want the original request", request.Input)
			}
			// Nothing was refused and nothing was spoken at the user: a failed route
			// is not a user-facing event.
			if commentary := session.commentary(); len(commentary) != 0 {
				t.Fatalf("a routing failure was surfaced to the voice model: %v", commentary)
			}
		})
	}
}

// TestControllerRouteTimeoutBoundsAHangingRouter is the timeout half of fail-open,
// driven the way production hits it: the router blocks until its own context ends,
// because the model behind it is not answering. The controller's deadline is what
// releases the worker, and the request still runs in the session lane with its
// original text.
func TestControllerRouteTimeoutBoundsAHangingRouter(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{}
	router := &fakeRouter{route: func(ctx context.Context, _ RouteRequest) (RouteResult, error) {
		// Never answer; the controller's own deadline has to be what ends this.
		<-ctx.Done()
		return RouteResult{}, ctx.Err()
	}}
	startControllerWith(t, session, ControllerOptions{Delegator: delegator, Router: router, RouteTimeout: 20 * time.Millisecond}, &updateRecorder{})

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "work-1", Text: "run the tests"})

	waitFor(t, "the work", func() bool { return delegator.count() == 1 })
	if request := delegator.lastRequest(); request.Input != "run the tests" {
		t.Fatalf("session lane received %q", request.Input)
	}
}

// TestControllerAnswersHandledRequestsInSubmissionOrder pins the ordering the single
// worker exists for: two requests about the call are answered in the order they
// arrived, because the second one may depend on the first having acted.
func TestControllerAnswersHandledRequestsInSubmissionOrder(t *testing.T) {
	session := newFakeSession()
	router := &fakeRouter{route: func(_ context.Context, request RouteRequest) (RouteResult, error) {
		return RouteResult{Handled: true, Answer: "handled: " + request.Input}, nil
	}}
	startControllerWith(t, session, ControllerOptions{Delegator: &fakeDelegator{}, Router: router}, &updateRecorder{})

	for _, input := range []string{"list the sessions", "switch to session 3", "change your voice to maple"} {
		session.emit(Event{Kind: EventDelegationCreated, DelegationID: input, Text: input})
	}

	waitFor(t, "every answer", func() bool { return len(session.commentary()) == 3 })
	router.mu.Lock()
	defer router.mu.Unlock()
	for i, want := range []string{"list the sessions", "switch to session 3", "change your voice to maple"} {
		if router.requests[i].Input != want {
			t.Fatalf("request %d = %q, want %q (order: %+v)", i, router.requests[i].Input, want, router.requests)
		}
	}
}

// TestControllerDoesNotBlockTheEventLoopOnARoute is the regression the route worker
// exists for. Routing is a model turn that takes seconds, and the event loop must
// keep delivering transcript deltas while it runs: run on the loop, this delta would
// not be observed until the route returned.
//
// The handshake is deliberate rather than a timing assertion: the router blocks until
// the test has seen the delta, so a blocked loop fails by never observing it.
func TestControllerDoesNotBlockTheEventLoopOnARoute(t *testing.T) {
	session := newFakeSession()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	router := &fakeRouter{route: func(context.Context, RouteRequest) (RouteResult, error) {
		close(started)
		<-release
		return RouteResult{Handled: true, Answer: "the answer"}, nil
	}}
	recorder := &updateRecorder{}
	startControllerWith(t, session, ControllerOptions{Delegator: &fakeDelegator{}, Router: router}, recorder)
	// Registered after the controller's own cleanup, so a failed assertion still
	// releases the blocked route before the controller waits for its worker.
	t.Cleanup(releaseAll)

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "slow", Text: "list the sessions"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the route never reached the worker")
	}

	session.emit(Event{Kind: EventAssistantTranscript, Role: RoleAssistant, Text: "still listening"})
	waitFor(t, "a transcript delta while the route runs", func() bool {
		return recorder.transcriptContains("still listening")
	})

	releaseAll()
	waitFor(t, "the answer", func() bool { return len(session.commentary()) == 1 })
}

// TestControllerAnswersWhileADelegationIsBlocked pins the bug this lane exists for: a
// session-management request spoken while a task is running must be answered rather
// than offered to Steer as guidance for that task, and it must complete before the
// task does.
func TestControllerAnswersWhileADelegationIsBlocked(t *testing.T) {
	session := newFakeSession()
	delegator := &steeringTestDelegator{started: make(chan struct{}), release: make(chan struct{}), steered: make(chan DelegationRequest, 4)}
	router := &fakeRouter{route: func(_ context.Context, request RouteRequest) (RouteResult, error) {
		if strings.Contains(request.Input, "7447") {
			return RouteResult{Handled: true, Answer: "Switched to session 7447."}, nil
		}
		// The long job is workspace work, and it is running while the switch is
		// spoken: that is the whole point of the test.
		return RouteResult{}, nil
	}}
	recorder := &updateRecorder{}
	startControllerWith(t, session, ControllerOptions{Delegator: delegator, Router: router}, recorder)

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "work", Text: "start the long job"})
	select {
	case <-delegator.started:
	case <-time.After(time.Second):
		t.Fatal("the work delegation did not start")
	}
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "swap", Text: "switch to session 7447"})

	waitFor(t, "the answer while the task runs", func() bool { return len(session.commentary()) == 1 })
	if delegator.calls.Load() != 1 {
		t.Fatalf("runs = %d, want the blocked task only", delegator.calls.Load())
	}
	if len(delegator.steered) != 0 {
		t.Fatal("a handled request was offered to Steer instead of being answered")
	}
	if states := recorder.statesFor("swap"); len(states) != 3 || states[2] != DelegationDone {
		t.Fatalf("delegation states = %v", states)
	}
	// The task is still blocked, which is what makes "completed before it finished"
	// true: the answer arrived while Run was waiting.
	close(delegator.release)
	waitFor(t, "the task answer", func() bool { return strings.Contains(session.speakable(), "one answer") })
}

// TestControllerRefusesRequestsTheRouteWorkerCannotTake covers the one host decision
// that has to happen on the event loop: five unanswered delegations already own the
// single worker, so the fifth is refused rather than queued without limit, dropped
// silently, or — worse — run as a chat turn that may not have been work at all.
func TestControllerRefusesRequestsTheRouteWorkerCannotTake(t *testing.T) {
	session := newFakeSession()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	router := &fakeRouter{route: func(_ context.Context, request RouteRequest) (RouteResult, error) {
		if request.Input == "slow" {
			close(started)
			<-release
		}
		return RouteResult{Handled: true, Answer: "handled: " + request.Input}, nil
	}}
	recorder := &updateRecorder{}
	startControllerWith(t, session, ControllerOptions{Delegator: &fakeDelegator{}, Router: router}, recorder)
	// The worker holds the first request until the test says so, so a failure partway
	// through must still release it before the controller is asked to close: cleanups
	// run last registered first, and the controller registered its own above.
	t.Cleanup(releaseAll)

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "slow", Text: "slow"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the first request never reached the worker")
	}
	// The worker is blocked, so these fill the channel exactly.
	for _, id := range []string{"wait-1", "wait-2", "wait-3", "wait-4"} {
		session.emit(Event{Kind: EventDelegationCreated, DelegationID: id, Text: id})
	}
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "overflow", Text: "one too many"})

	waitFor(t, "the overflow refusal", func() bool {
		_, ok := recorder.terminalFor("overflow")
		return ok
	})
	terminal, _ := recorder.terminalFor("overflow")
	if terminal.State != DelegationRefused || terminal.Text != "" {
		t.Fatalf("overflow update = %+v, want a textless refusal", terminal)
	}
	if router.count() != 1 {
		t.Fatalf("routes while the queue is full = %d, want the blocked one only", router.count())
	}

	releaseAll()
	waitFor(t, "the queued answers", func() bool { return len(session.commentary()) == 6 })
	if router.count() != 5 {
		t.Fatalf("routes = %d, want the five accepted requests", router.count())
	}
	for _, id := range []string{"slow", "wait-1", "wait-2", "wait-3", "wait-4"} {
		if states := recorder.statesFor(id); len(states) != 3 || states[2] != DelegationDone {
			t.Fatalf("%s states = %v", id, states)
		}
	}
}

// TestControllerEndsALateDelegationWhenTheStreamIsAlreadyOver is the teardown order
// production hits: the provider's event stream ends, and the route that was already
// in flight finishes afterwards with the request belonging to the session lane. The
// route worker does that send from its own goroutine, so a queue closed by the event
// loop would be a `send on closed channel` panic — the whole process down — no matter
// how the send is guarded, because the close can land between the guard and the send.
//
// The queue is therefore never closed. The request is reported terminally instead,
// which is also what keeps the voice model from waiting forever for an answer
// nothing will produce.
func TestControllerEndsALateDelegationWhenTheStreamIsAlreadyOver(t *testing.T) {
	session := newFakeSession()
	routed := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	router := &fakeRouter{route: func(context.Context, RouteRequest) (RouteResult, error) {
		close(routed)
		<-release
		// Not handled and not handed over: this is the session lane's request, and
		// the send that used to panic.
		return RouteResult{}, nil
	}}
	recorder := &updateRecorder{}
	delegator := &fakeDelegator{}
	controller := startControllerWith(t, session, ControllerOptions{Delegator: delegator, Router: router}, recorder)
	// Registered after the controller's own cleanup, so a failed assertion still
	// releases the blocked route before Close waits for its worker.
	t.Cleanup(releaseAll)

	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "late", Text: "fix the parser"})
	select {
	case <-routed:
	case <-time.After(time.Second):
		t.Fatal("the route never started")
	}
	// The stream ends first — the handshake, not a sleep: the route completes only
	// after the controller has seen the end of the stream.
	session.finish()
	select {
	case <-controller.eventsDone:
	case <-time.After(time.Second):
		t.Fatal("the controller did not see the end of the stream")
	}
	releaseAll()

	waitFor(t, "the late delegation to end", func() bool {
		_, ok := recorder.terminalFor("late")
		return ok
	})
	terminal, _ := recorder.terminalFor("late")
	if terminal.State != DelegationFailed || terminal.Text == "" {
		t.Fatalf("terminal update = %+v, want a failure carrying its reason", terminal)
	}
	// Exactly one terminal state: a second one would mean two paths ended the same
	// delegation, and the browser renders the last one it is sent.
	terminalStates := 0
	for _, state := range recorder.statesFor("late") {
		switch state {
		case DelegationDone, DelegationRefused, DelegationFailed:
			terminalStates++
		}
	}
	if terminalStates != 1 {
		t.Fatalf("delegation states = %v, want exactly one terminal state", recorder.statesFor("late"))
	}
	if delegator.count() != 0 {
		t.Fatalf("a request routed after the stream ended still ran %d times", delegator.count())
	}
}

// TestControllerCompletesADelegationTheDelegationQueueCannotTake pins the overflow
// path, which reports the request the way every other terminal path does. Publishing
// `failed` without completing the delegation — which is what this used to do — leaves
// the browser showing a failure while the provider still waits on the sideband for an
// answer to a request that will never run: the voice model hangs instead of repeating
// itself.
func TestControllerCompletesADelegationTheDelegationQueueCannotTake(t *testing.T) {
	session := &completingSession{fakeSession: newFakeSession()}
	recorder := &updateRecorder{}
	controller := &Controller{
		session:    session,
		observe:    recorder.observe,
		queue:      make(chan queuedDelegation, delegationQueueDepth),
		eventsDone: make(chan struct{}),
	}
	for i := range delegationQueueDepth {
		controller.queue <- queuedDelegation{ID: fmt.Sprintf("filler-%02d", i), Input: "waiting"}
	}

	controller.enqueueRoutedDelegation(context.Background(), queuedDelegation{ID: "overflow", Input: "one too many"})

	terminal, ok := recorder.terminalFor("overflow")
	if !ok || terminal.State != DelegationFailed || !strings.Contains(terminal.Text, "too many") {
		t.Fatalf("overflow update = %+v, want a failure explaining the backlog", terminal)
	}
	// The two audiences, and the part the old path forgot: the voice model is told why
	// on commentary, and the provider's delegation is completed so the model is not
	// left waiting on it.
	if commentary := session.commentary(); len(commentary) != 1 || !strings.Contains(commentary[0], "too many") {
		t.Fatalf("the voice model was not told why: %v", commentary)
	}
	if len(session.completions) != 1 || session.completions[0] != "overflow" {
		t.Fatalf("completions = %v, want the overflowing delegation completed", session.completions)
	}
}

// TestControllerWithoutARouterNeverRefusesForTheRouteQueue pins what switching the
// control plane off costs. With no router the request is resolved and enqueued inline,
// which is the pre-routing path: admission is the delegation queue's depth, no worker
// can refuse it for being busy, and resolution stays atomic with the arrival event.
// Hopping through the route worker instead put the default configuration on a queue
// four deep with a hard refusal, and on the second-producer path at the same time.
func TestControllerWithoutARouterNeverRefusesForTheRouteQueue(t *testing.T) {
	session := newFakeSession()
	delegator := &fakeDelegator{}
	recorder := &updateRecorder{}
	controller := startControllerWith(t, session, ControllerOptions{Delegator: delegator}, recorder)

	// The routing worker's queue is full, which with a router would refuse the
	// requests below. Each planted job is a metadata-only event, so it parks itself
	// instead of running as work and the assertions stay about the real delegations.
	for range routeQueueDepth {
		controller.routeQueue <- routeJob{event: Event{Kind: EventDelegationCreated, RawType: "session.delegation.created"}}
	}
	const requests = routeQueueDepth + 3
	for i := range requests {
		id := fmt.Sprintf("work-%d", i)
		session.emit(Event{Kind: EventDelegationCreated, DelegationID: id, Text: "request " + id})
	}

	waitFor(t, "every request in the session lane", func() bool { return delegator.count() == requests })
	seen := make([]string, 0, requests)
	for _, request := range delegator.allRequests() {
		seen = append(seen, request.Input)
	}
	for i := range requests {
		want := fmt.Sprintf("request work-%d", i)
		if seen[i] != want {
			t.Fatalf("session lane received %q at %d, want %q (order: %v)", seen[i], i, want, seen)
		}
	}
	for _, state := range recorder.states() {
		if state == DelegationRefused || state == DelegationFailed {
			t.Fatalf("a delegation the routing queue had no room for was reported %s: %v", state, recorder.states())
		}
	}
}

// TestControllerDeduplicatesReplayedDelegations keeps a provider replay from being
// routed, run, or answered twice.
func TestControllerDeduplicatesReplayedDelegations(t *testing.T) {
	session := newFakeSession()
	router := &fakeRouter{route: func(_ context.Context, request RouteRequest) (RouteResult, error) {
		return RouteResult{Handled: true, Answer: "handled: " + request.Input}, nil
	}}
	recorder := &updateRecorder{}
	startControllerWith(t, session, ControllerOptions{Delegator: &fakeDelegator{}, Router: router}, recorder)

	replay := Event{Kind: EventDelegationCreated, DelegationID: "control-replay", Text: "change your voice to maple"}
	session.emit(replay)
	session.emit(replay)
	session.emit(Event{Kind: EventDelegationCreated, DelegationID: "control-next", Text: "change your voice to spruce"})

	// The worker answers in submission order, so the second answer proves the
	// replayed event has already been consumed without being answered twice.
	waitFor(t, "both answers", func() bool { return len(session.commentary()) == 2 })
	if router.count() != 2 {
		t.Fatalf("routes = %d, want one per distinct delegation", router.count())
	}
	if states := recorder.statesFor("control-replay"); len(states) != 3 {
		t.Fatalf("replayed delegation states = %v", states)
	}
}

// TestControllerResolvesMetadataOnlyDelegationBeforeRouting covers the shape the
// production failure arrived in: on the frameless protocol a delegation may carry no
// text at all, so the router reads the user's own words from the transcript. The
// resolution happens before the request is classified, which is the only order that
// can route it correctly.
func TestControllerResolvesMetadataOnlyDelegationBeforeRouting(t *testing.T) {
	session := newFakeSession()
	router := &fakeRouter{route: func(context.Context, RouteRequest) (RouteResult, error) {
		return RouteResult{Handled: true, Answer: "Moved to the session you were managing."}, nil
	}}
	controller := startControllerWith(t, session, ControllerOptions{Delegator: &fakeDelegator{}, Router: router}, &updateRecorder{})

	// Metadata-only delegation first; the request text only exists in the transcript.
	session.emit(Event{Kind: EventDelegationCreated, RawType: "session.delegation.created", DelegationID: "item_live"})
	waitFor(t, "pending metadata-only delegation", func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return len(controller.pendingDelegations) == 1
	})
	if router.count() != 0 {
		t.Fatal("a delegation with no request text was routed")
	}

	const spoken = "Do you mind switching back to the session where I was managing session with voice"
	session.emit(Event{Kind: EventUserTranscript, Role: RoleUser, Text: spoken})

	waitFor(t, "the answer", func() bool { return len(session.commentary()) == 1 })
	if request := router.lastRequest(); request.Input != spoken {
		t.Fatalf("router received %q, want %q", request.Input, spoken)
	}
}

// TestControllerReportsFailedAndRefusedDelegationsToBothAudiences pins the split. The
// voice model is always told why, on commentary, because it is the only channel that
// can explain. The browser is told only what state the delegation reached: a failure
// keeps its text, because something was attempted and went wrong, while a refusal
// carries none, because its correction is addressed to the voice model.
func TestControllerReportsFailedAndRefusedDelegationsToBothAudiences(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		session := newFakeSession()
		router := &fakeRouter{route: func(context.Context, RouteRequest) (RouteResult, error) {
			// A handled route with nothing to say: the user asked for something and
			// would otherwise hear silence.
			return RouteResult{Handled: true}, nil
		}}
		recorder := &updateRecorder{}
		startControllerWith(t, session, ControllerOptions{Delegator: &fakeDelegator{}, Router: router}, recorder)

		session.emit(Event{Kind: EventDelegationCreated, DelegationID: "empty", Text: "what sessions are running?"})

		waitFor(t, "the failure", func() bool {
			_, ok := recorder.terminalFor("empty")
			return ok
		})
		terminal, _ := recorder.terminalFor("empty")
		if terminal.State != DelegationFailed || terminal.Text == "" {
			t.Fatalf("terminal update = %+v, want a failure carrying its reason", terminal)
		}
		waitFor(t, "the failure commentary", func() bool { return len(session.commentary()) == 1 })
		if text := session.commentary()[0]; !strings.Contains(text, "no answer") {
			t.Fatalf("the voice model was not told why: %q", text)
		}
	})

	t.Run("refusal", func(t *testing.T) {
		// A refusal is the host declining a request before anything ran, and the one
		// this lane still produces is the worker's capacity path.
		session := newFakeSession()
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
		router := &fakeRouter{route: func(_ context.Context, request RouteRequest) (RouteResult, error) {
			if request.Input == "slow" {
				<-release
			}
			return RouteResult{Handled: true, Answer: "handled"}, nil
		}}
		recorder := &updateRecorder{}
		startControllerWith(t, session, ControllerOptions{Delegator: &fakeDelegator{}, Router: router}, recorder)
		t.Cleanup(releaseAll)

		for _, id := range []string{"slow", "a", "b", "c", "d", "overflow"} {
			session.emit(Event{Kind: EventDelegationCreated, DelegationID: id, Text: id})
		}
		waitFor(t, "the overflow refusal", func() bool {
			_, ok := recorder.terminalFor("overflow")
			return ok
		})
		terminal, _ := recorder.terminalFor("overflow")
		if terminal.State != DelegationRefused || terminal.Text != "" {
			t.Fatalf("terminal update = %+v, want a textless refusal", terminal)
		}
		releaseAll()
	})
}
