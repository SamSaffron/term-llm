package live

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrDelegationBusy tells the controller the host could not start the turn
// because another turn owns the chat session. The delegation stays queued and
// is retried.
var ErrDelegationBusy = errors.New("live: session is busy")

// Delegator runs delegated work in the bound chat session. Run streams output
// back through emit and returns when the turn finishes.
type Delegator interface {
	Run(ctx context.Context, request DelegationRequest, emit func(DelegationChunk)) error
}

// SteeringDelegator can admit a delegation as guidance for the canonical task
// already running in the bound chat session. A false result means there is no
// active task to steer, so the controller should start an ordinary Run.
type SteeringDelegator interface {
	Steer(ctx context.Context, request DelegationRequest) (bool, error)
}

// Router decides what happens to one delegation before it can become work.
//
// Every delegation passes through it, because a delegation input is free text the
// voice model authored and nothing in the host can tell "which sessions are
// running?" from "fix the parser" without reading it. Two earlier attempts to
// answer that from the text's shape — a "control:" sentinel the model was told to
// emit, then a word-list prose detector — both failed in production on the first
// phrasing the lists did not contain ("Switch to the attachment icons one" had no
// session noun; "Do you mind switching back to ..." used a verb form the list
// lacked). Reading the request is the classification; there is no cheaper rule that
// stays correct.
//
// The host implements this as one small, tool-bearing fast-model turn: either it
// does the session-management work itself and answers, or it hands the request to
// the workspace agent.
type Router interface {
	Route(ctx context.Context, request RouteRequest) (RouteResult, error)
}

// RouteRequest is one delegation offered to the Router. TranscriptDelta is the
// recent conversation, so the router can resolve a reference the user made out
// loud ("the other one", "the attachment icons one") without the host guessing at
// antecedents.
type RouteRequest struct {
	Input           string
	TranscriptDelta string
}

// RouteResult is the Router's decision for one delegation.
type RouteResult struct {
	// Handled means the router did the work itself and Answer is what the voice
	// model should hear.
	Handled bool
	// Answer is the spoken reply, meaningful only when Handled.
	Answer string
	// Input is the request the session lane should run, meaningful only when not
	// Handled. The router may tidy it on the way through — repairing speech
	// recognition and dropping filler — because it has read it anyway. An empty
	// Input means the original request, untouched.
	Input string
}

// UpdateKind classifies a controller update for the host UI.
type UpdateKind string

const (
	// UpdateStarted reports that the provider accepted the session.
	UpdateStarted UpdateKind = "started"
	// UpdateTranscript carries partial or final spoken text.
	UpdateTranscript UpdateKind = "transcript"
	// UpdateDelegation reports delegated-turn progress.
	UpdateDelegation UpdateKind = "delegation"
	// UpdateInterrupted tells the host to discard any buffered provider audio.
	UpdateInterrupted UpdateKind = "interrupted"
	// UpdateError carries a session error.
	UpdateError UpdateKind = "error"
	// UpdateEnded reports that the session finished.
	UpdateEnded UpdateKind = "ended"
)

// DelegationState is the lifecycle of one delegated turn.
type DelegationState string

const (
	// DelegationQueued means the request is waiting for the runner.
	DelegationQueued DelegationState = "queued"
	// DelegationRunning means the chat session is executing it.
	DelegationRunning DelegationState = "running"
	// DelegationDone means the turn finished successfully.
	DelegationDone DelegationState = "done"
	// DelegationRefused means the host declined the request's form or routing and
	// told the voice model how to retry. Nothing failed and nothing ran, so this is
	// not a user-facing error.
	DelegationRefused DelegationState = "refused"
	// DelegationFailed means the turn failed or was cancelled.
	DelegationFailed DelegationState = "failed"
)

// Update is one host-visible controller event.
type Update struct {
	Kind         UpdateKind
	Role         string
	Text         string
	Final        bool
	Interim      bool // replaceable UI-only recognition preview, not transcript history
	DelegationID string
	State        DelegationState
}

const (
	defaultFlushInterval = 200 * time.Millisecond
	defaultBusyRetry     = 2 * time.Second
	defaultBusyRetries   = 15
	delegationQueueDepth = 32
	delegationHeadBytes  = 4000
	delegationTailBytes  = 2000
	truncationMarker     = "\n…output truncated…\n"
	busyNotice           = "Still finishing the previous request."
	steeringNotice       = "Guidance queued for the running task; its result will arrive with that task."
	steeringRetryDelay   = 10 * time.Millisecond
	// routeCallTimeout bounds one routing decision. It is deliberately above the
	// router's own ~30s bound on its turn, so a request that runs long reports the
	// router's reason rather than a bare deadline. This is a backstop for a router
	// that never returns: it cancels the routing turn, so a router that respects its
	// context cannot stall every later delegation behind it in the FIFO worker every
	// request now depends on. It bounds the turn, not the worker — a router that
	// ignores its own cancellation still holds the worker until it returns.
	routeCallTimeout = 45 * time.Second
	// RouteCallTimeout exposes the controller backstop to routers that must budget
	// multiple sequential provider calls within one routing decision.
	RouteCallTimeout = routeCallTimeout
	// routeQueueDepth bounds how many delegations may wait for that worker. A
	// backlog here is a voice model that has stopped listening for answers rather
	// than a load to absorb, so the request that overflows is refused and the model
	// can repeat it; nothing is queued without limit.
	routeQueueDepth = 4
)

// ControllerOptions configures a Controller.
type ControllerOptions struct {
	Session   Session
	Delegator Delegator
	// Router triages every delegation before it can become work. When it is absent
	// or fails, the delegation goes to the session lane untouched: a router outage
	// degrades to the behaviour before routing existed, and never to a refused or
	// lost request.
	Router Router
	// Observer receives host-visible updates. It must not block for long.
	Observer func(Update)
	// FlushInterval batches streamed agent text before it is appended.
	FlushInterval time.Duration
	// BusyRetry is the delay between retries when the chat session is busy.
	BusyRetry time.Duration
	// BusyRetries caps how many times a delegation waits for a busy session.
	BusyRetries int
	// RouteTimeout bounds one Router call. It defaults to a backstop above the
	// router's own turn budget, and is configurable so tests can exercise a router
	// that hangs without waiting for that backstop.
	RouteTimeout time.Duration
}

type queuedDelegation = DelegationRequest

// routeJob is one delegation handed from the event loop to the route worker. The
// event loop has already claimed the delegation id and published its queued state,
// so the worker only has to decide where the request goes.
type routeJob struct {
	event Event
}

// Controller owns the live conversation state: transcripts, the delegation
// queue, and streaming delegated output back to the voice model.
type Controller struct {
	session      Session
	delegator    Delegator
	router       Router
	routeTimeout time.Duration
	observe      func(Update)
	flush        time.Duration
	busyRetry    time.Duration
	busyRetries  int

	queue      chan queuedDelegation
	routeQueue chan routeJob
	// eventsDone is closed when the provider's event stream ends, and never sent
	// on. It is how the delegation loops learn there are no more requests without
	// the queue being closed under a second producer: the route worker sends on
	// queue from its own goroutine, and a send on a closed channel panics no
	// matter how it is guarded, because the close can land between the guard and
	// the send.
	eventsDone chan struct{}

	mu                 sync.Mutex
	userPartial        string
	assistantPartial   string
	lastUserTurn       string
	transcript         []string
	delegationIDs      map[string]struct{}
	pendingDelegations []Event

	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewController builds a controller for an open session.
func NewController(opts ControllerOptions) *Controller {
	observe := opts.Observer
	if observe == nil {
		observe = func(Update) {}
	}
	flush := opts.FlushInterval
	if flush <= 0 {
		flush = defaultFlushInterval
	}
	busyRetry := opts.BusyRetry
	if busyRetry <= 0 {
		busyRetry = defaultBusyRetry
	}
	busyRetries := opts.BusyRetries
	if busyRetries <= 0 {
		busyRetries = defaultBusyRetries
	}
	routeTimeout := opts.RouteTimeout
	if routeTimeout <= 0 {
		routeTimeout = routeCallTimeout
	}
	return &Controller{
		session:      opts.Session,
		delegator:    opts.Delegator,
		router:       opts.Router,
		routeTimeout: routeTimeout,
		observe:      observe,
		flush:        flush,
		busyRetry:    busyRetry,
		busyRetries:  busyRetries,
		queue:        make(chan queuedDelegation, delegationQueueDepth),
		routeQueue:   make(chan routeJob, routeQueueDepth),
		eventsDone:   make(chan struct{}),
	}
}

// Start runs the event, delegation, and routing loops until the session ends or
// Close is called.
func (c *Controller) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.wg.Add(3)
	go func() {
		defer c.wg.Done()
		c.consumeEvents(runCtx)
	}()
	go func() {
		defer c.wg.Done()
		c.runDelegations(runCtx)
	}()
	go func() {
		defer c.wg.Done()
		c.runRoutes(runCtx)
	}()
}

// Close ends the provider session and waits for every loop to finish.
func (c *Controller) Close(ctx context.Context) error {
	var err error
	c.closeOnce.Do(func() {
		err = c.session.Close(ctx)
		if c.cancel != nil {
			c.cancel()
		}
	})
	c.wg.Wait()
	return err
}

// AppendUserText injects text the user typed instead of speaking.
func (c *Controller) AppendUserText(ctx context.Context, text string) error {
	prefixed := prefixUserText(text)
	if prefixed == "" {
		return nil
	}
	c.recordTurn(RoleUser, text)
	return c.session.AppendText(ctx, prefixed)
}

func (c *Controller) consumeEvents(ctx context.Context) {
	// The queue is deliberately never closed. Its other producer is the route
	// worker, and a send on a closed channel panics even inside a select with a
	// default: the close can land between the readiness check and the send. The
	// loops below learn the stream has ended from eventsDone, which only this
	// goroutine closes.
	defer close(c.eventsDone)
	events := c.session.Events()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				c.observe(Update{Kind: UpdateEnded})
				return
			}
			if c.handleEvent(ctx, event) {
				c.observe(Update{Kind: UpdateEnded})
				return
			}
		}
	}
}

// handleEvent applies one provider event and reports whether the session ended.
func (c *Controller) handleEvent(ctx context.Context, event Event) bool {
	switch event.Kind {
	case EventSessionStarted:
		c.observe(Update{Kind: UpdateStarted})
	case EventUserTranscriptInterim:
		c.observe(Update{Kind: UpdateTranscript, Role: RoleUser, Text: event.Text, Interim: true})
	case EventUserTranscript:
		c.observe(Update{Kind: UpdateTranscript, Role: RoleUser, Text: c.appendPartial(RoleUser, event.Text)})
		c.flushPendingDelegations(ctx)
	case EventAssistantTranscript:
		c.observe(Update{Kind: UpdateTranscript, Role: RoleAssistant, Text: c.appendPartial(RoleAssistant, event.Text)})
	case EventTurnDone:
		role := event.Role
		if role != RoleAssistant {
			role = RoleUser
		}
		text := strings.TrimSpace(event.Text)
		if text == "" {
			text = c.partial(role)
		}
		c.recordTurn(role, text)
		c.observe(Update{Kind: UpdateTranscript, Role: role, Text: text, Final: true})
	case EventDelegationCreated:
		c.handleDelegationCreated(ctx, event)
	case EventInterrupted:
		c.mu.Lock()
		c.assistantPartial = ""
		c.mu.Unlock()
		c.observe(Update{Kind: UpdateInterrupted})
	case EventError:
		if !event.ErrorHandled {
			c.observe(Update{Kind: UpdateError, Text: event.Text})
		}
	case EventEnded:
		return true
	}
	return false
}

// handleDelegationCreated accepts one provider delegation event and hands it to
// its next step: the route worker when the control plane is on, the session lane
// directly when it is off.
//
// Nothing is classified here. The event loop runs on a channel that also carries
// every transcript delta, interruption, and later delegation, and routing is a
// model turn that takes seconds, so the loop does two cheap, ordering-sensitive
// things and nothing else: it claims the delegation id, and it publishes the state
// that says the host has the request.
//
// The published state is `queued`, and it means what it says: the host has the
// request and nothing is executing it yet. `queued` covers both of the waits a
// delegation can sit in — ahead of the route worker here, and behind an active
// task in the session lane — while `running` is published by whichever step
// actually executes it. Publishing `running` on arrival instead would collapse
// that distinction and leave a request stuck behind a long task indistinguishable
// from one being worked on.
func (c *Controller) handleDelegationCreated(ctx context.Context, event Event) {
	if !c.claimDelegationID(event.DelegationID) {
		return
	}
	c.observe(Update{Kind: UpdateDelegation, DelegationID: event.DelegationID, State: DelegationQueued})
	if c.submitDelegation(ctx, event) {
		return
	}
	// The worker is at capacity: a backlog here is a voice model that has stopped
	// listening, not a load to absorb. Refusing keeps the event loop free and tells
	// the model why, and it never falls through to the session lane, because the
	// request may be a session control and a misrouted one is the pollution this
	// lane exists to prevent.
	c.finishRefusal(ctx, event.DelegationID, errors.New("too many requests are already waiting"))
}

// submitDelegation offers one claimed delegation to the step that will place it, and
// reports whether the host has taken it. A request that resolution parks for a later
// transcript delta counts as taken: it is the host's to retry, not the model's to
// resend.
//
// With no router there is nothing to decide and no worker to be at capacity, so
// the request is resolved and enqueued inline, on the event loop, exactly as this
// lane worked before triage existed. Hopping it through the route worker instead
// would move resolution away from the arrival event — a transcript delta landing in
// between is then folded into the request — and would put the default configuration
// on the second-producer path for no gain.
func (c *Controller) submitDelegation(ctx context.Context, event Event) bool {
	if c.router == nil {
		c.enqueueUnroutedDelegation(ctx, event)
		return true
	}
	return c.submitRoute(event)
}

// enqueueUnroutedDelegation resolves one delegation on the event loop and hands it
// to the session lane. Resolution may still park the event for a later transcript
// delta, which leaves it the host's to retry rather than lost.
func (c *Controller) enqueueUnroutedDelegation(ctx context.Context, event Event) {
	request, ok := c.resolveDelegation(event)
	if !ok {
		return
	}
	c.enqueueRoutedDelegation(ctx, request)
}

// submitRoute hands one delegation to the route worker without blocking, and
// reports whether it was accepted. The worker is FIFO, so answers stay in
// submission order, which matters because a later request may depend on an earlier
// one having acted ("switch back to that one").
func (c *Controller) submitRoute(event Event) bool {
	select {
	case c.routeQueue <- routeJob{event: event}:
		return true
	default:
		return false
	}
}

// runRoutes routes delegations one at a time, in submission order, so two requests
// about the call cannot overtake each other. It ends with its context: the request
// in flight is cancelled by it and the queue is abandoned, exactly as the
// delegation loop is.
func (c *Controller) runRoutes(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-c.routeQueue:
			c.routeDelegation(ctx, job.event)
		}
	}
}

// routeDelegation decides where one delegation goes, off the event loop.
//
// The input is resolved first, exactly as it was when the delegation queue did the
// resolving: on the frameless protocol a delegation may carry no text at all, and
// the host then takes the request from the user's own transcript. Resolution and
// parking happen under one lock so a transcript delta that arrives while this is
// running cannot be missed — see resolveDelegation.
//
// Routing fails open. A router error, or a router that never returns, sends the
// ORIGINAL request to the session lane: the user asked for something, and a broken
// triage step must not turn that into a refusal or a lost request. Only a request
// that reached a router and came back Handled skips the session lane. An absent
// router never reaches here at all — submitDelegation places those inline.
func (c *Controller) routeDelegation(ctx context.Context, event Event) {
	request, ok := c.resolveDelegation(event)
	if !ok {
		return
	}
	if c.router == nil {
		// Unreachable by construction — submitDelegation places those inline — and
		// still checked, because this runs on a worker goroutine where a nil
		// dereference takes the process down instead of failing one request. No
		// control plane means ordinary work for the bound session.
		c.enqueueRoutedDelegation(ctx, request)
		return
	}
	result, err := c.route(ctx, request)
	if err != nil {
		// Two kinds of error, and only one of them may fall open. A request the
		// router already proved was session management — it ran a control tool — is
		// reported to the voice model, because running it in the session lane would
		// make it the chat turn this lane exists to prevent. Everything else means
		// triage never reached a verdict, so the request is still presumed to be work.
		if isControlFailure(err) {
			// It executed — the tools it ran are what proved the verdict — so it is
			// reported as having run, exactly as a control request that finished is.
			c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationRunning})
			c.finishFailure(ctx, request.ID, err)
			return
		}
		if isControlRefusal(err) {
			// Declined rather than run, so it never reports `running`.
			c.finishFailure(ctx, request.ID, err)
			return
		}
		log.Printf("[live] routing %s failed, running it as work: %v", request.ID, err)
		result = RouteResult{}
	}
	if result.Handled {
		c.finishHandled(ctx, request.ID, result.Answer)
		return
	}
	input := strings.TrimSpace(result.Input)
	if input == "" {
		input = request.Input
	}
	request.Input = input
	c.enqueueRoutedDelegation(ctx, request)
}

// route asks the Router to triage one request. Every failure is an error the caller
// falls open on: this lane has no refusal of its own, because a request the router
// declined to read is still a request the user made. A host with no router never
// gets here — submitDelegation sends those straight to the session lane.
func (c *Controller) route(ctx context.Context, request queuedDelegation) (RouteResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.routeTimeout)
	defer cancel()
	return c.router.Route(callCtx, RouteRequest{Input: request.Input, TranscriptDelta: request.TranscriptDelta})
}

// finishHandled reports one delegation the router answered itself. Nothing here
// touches the bound chat session: no run is started, no message is persisted, and
// the busy-retry path is never consulted, so a session-management request works
// while the bound session has a task running.
//
// An empty answer is a failure rather than a silent success: the user asked for
// something and would otherwise hear nothing at all. It is reported the same way
// any other router failure is — the voice model is told, and the delegation still
// completes so the model always has a next move.
func (c *Controller) finishHandled(ctx context.Context, id, answer string) {
	// Reported after the fact rather than before the router is called, because
	// until it returns there is no way to know whether this delegation is being
	// executed here or is about to be handed to the session lane and wait. It did
	// run, so it is reported as having run, and an empty answer is then a failure
	// of something that ran rather than of something that never started.
	c.observe(Update{Kind: UpdateDelegation, DelegationID: id, State: DelegationRunning})
	if strings.TrimSpace(answer) == "" {
		c.finishFailure(ctx, id, errors.New("the router returned no answer"))
		return
	}
	c.appendDelegation(ctx, id, DelegationChunk{Channel: ChannelQuiet, Text: answer})
	c.completeDelegation(ctx, id)
	c.observe(Update{Kind: UpdateDelegation, DelegationID: id, State: DelegationDone})
}

// finishFailure reports how one delegated request ended, to both audiences at
// once.
//
// The two audiences are told different things, and they are not the same string.
// The voice model hears guidance it can act on — the reason, on the quiet
// channel — because it is the only retry channel. The browser is told only what
// state the delegation reached: a failure carries its text, because something was
// attempted and went wrong and the reason is the user's to see, while a refusal
// carries none.
func (c *Controller) finishFailure(ctx context.Context, id string, err error) {
	c.appendDelegation(ctx, id, DelegationChunk{Channel: ChannelQuiet, Text: controlRejectionText(err)})
	c.completeDelegation(ctx, id)
	if isControlRefusal(err) {
		// DelegationRefused carries no text on purpose: the browser panel renders
		// delegation text as an error, and this text is addressed to the model.
		c.observe(Update{Kind: UpdateDelegation, DelegationID: id, State: DelegationRefused})
		return
	}
	c.observe(Update{Kind: UpdateDelegation, DelegationID: id, State: DelegationFailed, Text: err.Error()})
}

// finishRefusal refuses a request the host declined before anything ran, which is
// the capacity path above and nothing else in this lane.
func (c *Controller) finishRefusal(ctx context.Context, id string, err error) {
	c.finishFailure(ctx, id, refuseControl(err))
}

// resolveDelegation turns one provider event into the request the router and then
// the session lane see, resolving metadata-only events against accumulated
// transcript fragments. It reports false when no task text is available yet, and
// parks the event for a later user transcript delta.
//
// On the frameless protocol a delegation may carry no input text at all: the host
// then takes the request from the user's own transcript, so the text that becomes
// the chat turn is the user's own words. That is exactly how "Um, can you swap to
// the discourse backport angel widget session" reached a transcript in the wild.
//
// Parking happens while the lock is still held, and resolution reads the transcript
// under the same lock, because otherwise a delta arriving between "no text yet" and
// "park it" would be missed and the delegation would wait forever for a turn that
// had already been taken.
func (c *Controller) resolveDelegation(event Event) (queuedDelegation, bool) {
	request := queuedDelegation{
		ID:    event.DelegationID,
		Input: strings.TrimSpace(event.Text),
	}
	c.mu.Lock()
	if request.Input == "" {
		request.Input = strings.TrimSpace(c.userPartial)
	}
	if request.Input == "" && event.RawType != "session.delegation.created" {
		request.Input = strings.TrimSpace(c.lastUserTurn)
	}
	if request.Input == "" && event.RawType == "session.delegation.created" {
		c.pendingDelegations = append(c.pendingDelegations, event)
		c.mu.Unlock()
		return queuedDelegation{}, false
	}

	transcript := append([]string(nil), c.transcript...)
	if event.RawType == "session.delegation.created" {
		if text := strings.TrimSpace(c.userPartial); text != "" {
			transcript = append(transcript, RoleUser+": "+text)
		}
		if text := strings.TrimSpace(c.assistantPartial); text != "" {
			transcript = append(transcript, RoleAssistant+": "+text)
		}
		c.lastUserTurn = request.Input
		c.userPartial = ""
		c.assistantPartial = ""
	}
	request.TranscriptDelta = strings.Join(transcript, "\n")
	c.transcript = nil
	c.mu.Unlock()
	return request, true
}

// enqueueRoutedDelegation pushes one routed request into the session lane.
//
// It publishes no queued state of its own: the event loop already said the host had
// the request, and the session lane announces the turn it starts. The lane's own
// ordering is untouched — runSteeringDelegations still offers every new request to
// Steer first and runs it with the normal busy retry.
//
// Both ways it can decline end the delegation the way every other terminal path
// does. Reporting the state without completing the delegation, as this used to,
// leaves the browser showing a failure while the provider still waits on the
// sideband for an answer to a request that will never run: the voice model hangs
// instead of repeating itself.
func (c *Controller) enqueueRoutedDelegation(ctx context.Context, request queuedDelegation) {
	select {
	case <-c.eventsDone:
		// The provider stream is over, so no consumer remains and nothing can run
		// this request. Sending it anyway would leave it queued forever, and a
		// delegation the voice model is still waiting on is a call that has stopped
		// answering.
		c.finishFailure(ctx, request.ID, errors.New("the live call ended before this request ran"))
		return
	default:
	}
	select {
	case c.queue <- request:
	default:
		// The session lane is a full queue of work nobody is waiting to hear the
		// answer to. The request is not silently dropped: it is completed here, off
		// the event loop, where provider I/O is allowed.
		c.finishFailure(ctx, request.ID, errors.New("too many pending requests"))
	}
}

func (c *Controller) flushPendingDelegations(ctx context.Context) {
	c.mu.Lock()
	pending := c.pendingDelegations
	c.pendingDelegations = nil
	c.mu.Unlock()
	for _, event := range pending {
		// These ids are already claimed and their state already published, so
		// anything that does not place them re-parks them: the next transcript
		// delta retries, and nothing the model was told becomes a lie.
		if c.submitDelegation(ctx, event) {
			continue
		}
		c.mu.Lock()
		c.pendingDelegations = append(c.pendingDelegations, event)
		c.mu.Unlock()
	}
}

// claimDelegationID reserves a provider delegation id so a replayed event is
// ignored. It reports false when the id was already claimed.
func (c *Controller) claimDelegationID(delegationID string) bool {
	if delegationID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, duplicate := c.delegationIDs[delegationID]; duplicate {
		return false
	}
	if c.delegationIDs == nil {
		c.delegationIDs = make(map[string]struct{})
	}
	c.delegationIDs[delegationID] = struct{}{}
	return true
}

// runDelegations runs one delegated turn at a time for hosts that cannot steer,
// and ends when the session does: on Close (ctx), or when the provider's event
// stream ends (eventsDone), after draining whatever the route worker had already
// queued. Draining rather than abandoning is what keeps the last request of a
// finished call from being silently dropped.
func (c *Controller) runDelegations(ctx context.Context) {
	if _, ok := c.delegator.(SteeringDelegator); !ok {
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-c.queue:
				if ctx.Err() != nil {
					return
				}
				c.runDelegation(ctx, request)
				continue
			case <-c.eventsDone:
			}
			c.drainQueuedDelegations(ctx)
			return
		}
	}
	c.runSteeringDelegations(ctx)
}

// drainQueuedDelegations finishes the requests that are already in the queue when
// the event stream ends, then leaves the rest to nobody: nothing can add to it
// except the route worker, whose late arrivals enqueueRoutedDelegation now reports
// terminally instead of leaving to hang.
func (c *Controller) drainQueuedDelegations(ctx context.Context) {
	for {
		select {
		case request := <-c.queue:
			if ctx.Err() != nil {
				return
			}
			c.runDelegation(ctx, request)
		default:
			return
		}
	}
}

// runSteeringDelegations processes delegations for hosts that can steer a
// running turn. New requests are always offered to Steer first, even while a
// previous Run blocks, so corrections arrive promptly. At most one Run (and
// its output stream) is ever active; Steer admissions complete as guidance
// without starting a second Run. Like runDelegations it ends with the session:
// on Close, or when the event stream ends, after finishing what it retained.
func (c *Controller) runSteeringDelegations(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	queue := c.queue
	// events is cleared once it fires so the loop cannot spin on it while a Run is
	// still active: a closed channel is always ready, and the wait below is where
	// this loop spends its time.
	events := c.eventsDone
	var active <-chan struct{}
	var pending []DelegationRequest
	ticker := time.NewTicker(steeringRetryDelay)
	defer ticker.Stop()
	defer func() {
		cancel()
		if active != nil {
			<-active
		}
	}()
	// The loop ends when there is no queue to accept from, no stream left to accept
	// from, no Run in flight, and nothing retained: that is the drain-then-exit the
	// closed queue used to produce.
	for queue != nil || active != nil || len(pending) > 0 {
		if ctx.Err() != nil {
			return
		}
		if len(pending) > 0 {
			request := pending[0]
			admitted, err := c.delegator.(SteeringDelegator).Steer(ctx, request)
			switch {
			case err != nil:
				c.appendDelegation(ctx, request.ID, DelegationChunk{Channel: ChannelSpeakable, Text: delegationFailureText(err)})
				c.completeDelegation(ctx, request.ID)
				c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationFailed, Text: err.Error()})
			case admitted:
				c.appendDelegation(ctx, request.ID, DelegationChunk{Channel: ChannelQuiet, Text: steeringNotice})
				c.completeDelegation(ctx, request.ID)
				c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationDone})
			case active == nil:
				done := make(chan struct{})
				active = done
				go func() { defer close(done); c.runDelegation(ctx, request) }()
			default:
				goto wait
			}
			pending = pending[1:]
			continue
		}
	wait:
		// Stop draining the bounded provider queue while retained requests fill up.
		incoming := queue
		if len(pending) >= delegationQueueDepth {
			incoming = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-events:
			// The provider stream has ended and with it the only source of new
			// requests. Stop accepting, finish what is already retained, and exit.
			queue, events = nil, nil
		case request := <-incoming:
			pending = append(pending, request)
		case <-active:
			active = nil
		case <-ticker.C: // Retry guidance during the small runtime-startup window.
		}
	}
}

func (c *Controller) runDelegation(ctx context.Context, request queuedDelegation) {
	defer c.completeDelegation(ctx, request.ID)
	c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationRunning, Text: request.Input})
	writer := newDelegationWriter(ctx, c, request.ID, c.flush)
	err := c.runWithBusyRetry(ctx, request, writer)
	writer.close()
	if err != nil {
		c.appendDelegation(ctx, request.ID, DelegationChunk{Channel: ChannelSpeakable, Text: delegationFailureText(err)})
		c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationFailed, Text: err.Error()})
		return
	}
	c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationDone})
}

// runWithBusyRetry keeps a delegation queued while another turn owns the chat
// session, telling the voice model once so it does not go silent.
func (c *Controller) runWithBusyRetry(ctx context.Context, request DelegationRequest, writer *delegationWriter) error {
	var err error
	for attempt := 0; attempt <= c.busyRetries; attempt++ {
		err = c.delegator.Run(ctx, request, writer.emit)
		if !errors.Is(err, ErrDelegationBusy) {
			return err
		}
		if attempt == 0 {
			writer.emit(DelegationChunk{Channel: ChannelQuiet, Text: busyNotice})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.busyRetry):
		}
	}
	return err
}

func (c *Controller) appendDelegation(ctx context.Context, delegationID string, chunk DelegationChunk) {
	if strings.TrimSpace(chunk.Text) == "" {
		return
	}
	appendCtx := ctx
	if appendCtx.Err() != nil {
		// The call is winding down but the voice model still needs the tail of
		// this delegation, so use a short independent deadline.
		var cancel context.CancelFunc
		appendCtx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
	}
	if err := c.session.AppendDelegation(appendCtx, delegationID, chunk); err != nil {
		c.observe(Update{Kind: UpdateError, Text: fmt.Sprintf("failed to answer: %v", err)})
	}
}

func (c *Controller) completeDelegation(ctx context.Context, delegationID string) {
	session, ok := c.session.(DelegationCompletionSession)
	if !ok {
		return
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
	}
	if err := session.CompleteDelegation(ctx, delegationID); err != nil {
		c.observe(Update{Kind: UpdateError, Text: fmt.Sprintf("failed to complete answer: %v", err)})
	}
}

func (c *Controller) appendPartial(role, delta string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if role == RoleAssistant {
		c.assistantPartial += delta
		return c.assistantPartial
	}
	c.userPartial += delta
	return c.userPartial
}

func (c *Controller) partial(role string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if role == RoleAssistant {
		return strings.TrimSpace(c.assistantPartial)
	}
	return strings.TrimSpace(c.userPartial)
}

func (c *Controller) recordTurn(role, text string) {
	trimmed := strings.TrimSpace(text)
	c.mu.Lock()
	defer c.mu.Unlock()
	if role == RoleAssistant {
		c.assistantPartial = ""
	} else {
		c.userPartial = ""
		if trimmed != "" {
			c.lastUserTurn = trimmed
		}
	}
	if trimmed == "" {
		return
	}
	c.transcript = append(c.transcript, role+": "+trimmed)
	if len(c.transcript) > 20 {
		c.transcript = c.transcript[len(c.transcript)-20:]
	}
}

// delegationWriter batches streamed agent text into context appends and
// applies the head/tail policy that keeps runaway output speakable.
type delegationWriter struct {
	ctx        context.Context
	controller *Controller
	id         string

	mu        sync.Mutex
	buffer    strings.Builder
	head      int
	tail      []byte
	truncated bool
	closed    bool

	// sendMu orders appends to the session. It is held across a batch's
	// append, so an unbatched note that follows buffered result text cannot
	// reach the voice model ahead of it through the periodic flush. Holding it
	// across network I/O is deliberate: appends to one delegation were already
	// synchronous with the caller, and a stalled append delaying the next one
	// is exactly the ordering this protects.
	sendMu sync.Mutex

	done     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func newDelegationWriter(ctx context.Context, controller *Controller, id string, interval time.Duration) *delegationWriter {
	w := &delegationWriter{ctx: ctx, controller: controller, id: id, done: make(chan struct{})}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-w.done:
				return
			case <-ticker.C:
				w.flush()
			}
		}
	}()
	return w
}

func (w *delegationWriter) emit(chunk DelegationChunk) {
	if strings.TrimSpace(chunk.Text) == "" {
		return
	}
	if chunk.Channel == ChannelQuiet || chunk.Progress {
		// Quiet notes and progress are short status text; send them without
		// waiting for the speakable batch so the voice model hears them while the
		// work is still running. Result text already buffered goes first, so a
		// note never overtakes the output it follows.
		w.sendMu.Lock()
		defer w.sendMu.Unlock()
		w.mu.Lock()
		closed := w.closed
		w.mu.Unlock()
		if closed {
			return // nothing may follow the delegation's final output
		}
		w.flushLocked()
		w.controller.appendDelegation(w.ctx, w.id, chunk)
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	text := chunk.Text
	if remaining := delegationHeadBytes - w.head; remaining > 0 {
		if len(text) <= remaining {
			w.head += len(text)
			w.buffer.WriteString(text)
			ready := w.buffer.Len() >= MaxAppendBytes
			w.mu.Unlock()
			if ready {
				w.flush()
			}
			return
		}
		cut := safeCut(text, remaining)
		w.head += cut
		w.buffer.WriteString(text[:cut])
		text = text[cut:]
	}
	w.truncated = true
	w.tail = append(w.tail, text...)
	if len(w.tail) > delegationTailBytes {
		w.tail = w.tail[len(w.tail)-delegationTailBytes:]
	}
	w.mu.Unlock()
	w.flush()
}

func (w *delegationWriter) flush() {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	w.flushLocked()
}

// flushLocked sends buffered result text. The caller holds sendMu.
func (w *delegationWriter) flushLocked() {
	w.mu.Lock()
	pending := w.buffer.String()
	w.buffer.Reset()
	w.mu.Unlock()
	if pending == "" {
		return
	}
	w.controller.appendDelegation(w.ctx, w.id, DelegationChunk{Channel: ChannelSpeakable, Text: pending})
}

func (w *delegationWriter) close() {
	w.stopOnce.Do(func() { close(w.done) })
	w.wg.Wait()
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	w.flushLocked()
	w.mu.Lock()
	truncated, tail := w.truncated, w.tail
	w.tail = nil
	w.closed = true
	w.mu.Unlock()
	if !truncated {
		return
	}
	trailer := truncationMarker + string(skipPartialRune(tail))
	w.controller.appendDelegation(w.ctx, w.id, DelegationChunk{Channel: ChannelSpeakable, Text: trailer})
}

// safeCut returns the largest offset at or below limit that lands on a rune
// boundary.
func safeCut(text string, limit int) int {
	if limit >= len(text) {
		return len(text)
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return limit
}

// skipPartialRune drops a leading continuation byte left behind by trimming
// the tail buffer from the front.
func skipPartialRune(b []byte) []byte {
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return b
}
