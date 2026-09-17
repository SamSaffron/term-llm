package live

import (
	"context"
	"errors"
	"fmt"
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
)

// ControllerOptions configures a Controller.
type ControllerOptions struct {
	Session   Session
	Delegator Delegator
	// Observer receives host-visible updates. It must not block for long.
	Observer func(Update)
	// FlushInterval batches streamed agent text before it is appended.
	FlushInterval time.Duration
	// BusyRetry is the delay between retries when the chat session is busy.
	BusyRetry time.Duration
	// BusyRetries caps how many times a delegation waits for a busy session.
	BusyRetries int
}

type queuedDelegation = DelegationRequest

type pendingDelegation struct {
	request    queuedDelegation
	retrySteer bool
}

// Controller owns the live conversation state: transcripts, the delegation
// queue, and streaming delegated output back to the voice model.
type Controller struct {
	session     Session
	delegator   Delegator
	observe     func(Update)
	flush       time.Duration
	busyRetry   time.Duration
	busyRetries int

	queue chan queuedDelegation

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
	return &Controller{
		session:     opts.Session,
		delegator:   opts.Delegator,
		observe:     observe,
		flush:       flush,
		busyRetry:   busyRetry,
		busyRetries: busyRetries,
		queue:       make(chan queuedDelegation, delegationQueueDepth),
	}
}

// Start runs the event and delegation loops until the session ends or Close is
// called.
func (c *Controller) Start(ctx context.Context) {
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		c.consumeEvents(runCtx)
	}()
	go func() {
		defer c.wg.Done()
		c.runDelegations(runCtx)
	}()
}

// Close ends the provider session and waits for both loops to finish.
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
	defer close(c.queue)
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
			if c.handleEvent(event) {
				c.observe(Update{Kind: UpdateEnded})
				return
			}
		}
	}
}

// handleEvent applies one provider event and reports whether the session ended.
func (c *Controller) handleEvent(event Event) bool {
	switch event.Kind {
	case EventSessionStarted:
		c.observe(Update{Kind: UpdateStarted})
	case EventUserTranscriptInterim:
		c.observe(Update{Kind: UpdateTranscript, Role: RoleUser, Text: event.Text, Interim: true})
	case EventUserTranscript:
		c.observe(Update{Kind: UpdateTranscript, Role: RoleUser, Text: c.appendPartial(RoleUser, event.Text)})
		c.flushPendingDelegations()
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
		c.enqueueDelegation(event)
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

func (c *Controller) enqueueDelegation(event Event) {
	c.mu.Lock()
	if event.DelegationID != "" {
		if _, duplicate := c.delegationIDs[event.DelegationID]; duplicate {
			c.mu.Unlock()
			return
		}
		if c.delegationIDs == nil {
			c.delegationIDs = make(map[string]struct{})
		}
		c.delegationIDs[event.DelegationID] = struct{}{}
	}
	c.mu.Unlock()
	if !c.queueDelegation(event) {
		c.mu.Lock()
		c.pendingDelegations = append(c.pendingDelegations, event)
		c.mu.Unlock()
	}
}

// queueDelegation resolves metadata-only provider events against accumulated
// transcript fragments. It returns false when no task text is available yet so
// the event can be retried after a later user transcript delta.
func (c *Controller) queueDelegation(event Event) bool {
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
		c.mu.Unlock()
		return false
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

	c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationQueued, Text: request.Input})
	select {
	case c.queue <- request:
	default:
		c.observe(Update{Kind: UpdateDelegation, DelegationID: request.ID, State: DelegationFailed, Text: "too many pending requests"})
	}
	return true
}

func (c *Controller) flushPendingDelegations() {
	c.mu.Lock()
	pending := c.pendingDelegations
	c.pendingDelegations = nil
	c.mu.Unlock()
	for _, event := range pending {
		if c.queueDelegation(event) {
			continue
		}
		c.mu.Lock()
		c.pendingDelegations = append(c.pendingDelegations, event)
		c.mu.Unlock()
	}
}

func (c *Controller) runDelegations(ctx context.Context) {
	if _, ok := c.delegator.(SteeringDelegator); !ok {
		for request := range c.queue {
			if ctx.Err() != nil {
				return
			}
			c.runDelegation(ctx, request)
		}
		return
	}
	c.runSteeringDelegations(ctx)
}

// runSteeringDelegations processes delegations for hosts that can steer a
// running turn. New requests are always offered to Steer first, even while a
// previous Run blocks, so corrections arrive promptly. At most one Run (and
// its output stream) is ever active; Steer admissions complete as guidance
// without starting a second Run.
func (c *Controller) runSteeringDelegations(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	queue := c.queue
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
				c.appendDelegation(ctx, request.ID, DelegationChunk{Channel: ChannelCommentary, Text: steeringNotice})
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
		case request, ok := <-incoming:
			if !ok {
				queue = nil
			} else {
				pending = append(pending, request)
			}
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
			writer.emit(DelegationChunk{Channel: ChannelCommentary, Text: busyNotice})
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
	if chunk.Channel == ChannelCommentary {
		// Commentary is short status text; send it without waiting for the
		// speakable batch so the voice model can fill the silence.
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
	w.flush()
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
