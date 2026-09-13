package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	"github.com/samsaffron/term-llm/internal/session"
)

type responseRunIdempotencyClaim struct {
	runID       string
	sessionID   string
	fingerprint string
}

type responseRunManager struct {
	mu                    sync.Mutex
	runs                  map[string]*responseRun
	activeBySession       map[string]string
	idempotencyByKey      map[string]responseRunIdempotencyClaim
	idempotencyAdmissions map[string]chan struct{}
	cleanupTimers         map[string]*time.Timer
	nextEpochBySession    map[string]int64
	terminalRetention     time.Duration
	runWG                 sync.WaitGroup
	closed                bool
	steeringTransitions   sync.Map
	boundaries            sync.Map // map[session ID]*sync.Mutex
	idempotencyReplays    atomic.Uint64
}

type responseRunDiagnostics struct {
	IdempotencyReplays uint64 `json:"idempotency_replays"`
}

func (m *responseRunManager) Diagnostics() responseRunDiagnostics {
	if m == nil {
		return responseRunDiagnostics{}
	}
	return responseRunDiagnostics{IdempotencyReplays: m.idempotencyReplays.Load()}
}

const (
	defaultResponseRunRetention        = 5 * time.Minute
	defaultResponseRunReplayLimit      = 2048
	defaultResponseRunSubscriberBuffer = 256
	defaultServeRequestTimeout         = 30 * time.Minute
	responseRunHoldGrace               = 30 * time.Second
	maxResponseRunDelegationHold       = time.Hour + responseRunHoldGrace
)

var (
	errResponseRunTimeout     = errors.New("response run timeout")
	errResponseRunKeyConflict = errors.New("idempotency key was already used with a different request")
)

type responseRunTimerHandle interface {
	Stop() bool
}

type responseRunClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) responseRunTimerHandle
}

type realResponseRunClock struct{}

func (realResponseRunClock) Now() time.Time { return time.Now() }

func (realResponseRunClock) AfterFunc(delay time.Duration, fn func()) responseRunTimerHandle {
	return time.AfterFunc(delay, fn)
}

type responseRunTimerHold struct {
	extend  func(time.Time)
	release func()
}

type responseRunTimerHoldState struct {
	timer       *responseRunTimer
	resume      func()
	handle      responseRunTimerHandle
	expiresAt   time.Time
	absoluteCap time.Time
	generation  uint64
}

func noopResponseRunTimerHold() responseRunTimerHold {
	return responseRunTimerHold{extend: func(time.Time) {}, release: func() {}}
}

// responseRunTimer bounds inactivity between the user request and each completed
// LLM response. Interactive waits and verified, deadline-bounded delegations
// pause the current inactivity window.
type responseRunTimer struct {
	mu         sync.Mutex
	cancel     context.CancelCauseFunc
	timer      responseRunTimerHandle
	clock      responseRunClock
	timeout    time.Duration
	remaining  time.Duration
	activeAt   time.Time
	generation uint64
	pauses     int
	holds      map[*responseRunTimerHoldState]struct{}
	stopped    bool
}

func newResponseRunTimer(timeout time.Duration) (context.Context, *responseRunTimer) {
	return newResponseRunTimerWithClock(timeout, realResponseRunClock{})
}

func newResponseRunTimerWithClock(timeout time.Duration, clock responseRunClock) (context.Context, *responseRunTimer) {
	ctx, cancel := context.WithCancelCause(context.Background())
	t := &responseRunTimer{
		cancel:    cancel,
		clock:     clock,
		timeout:   timeout,
		remaining: timeout,
	}
	t.mu.Lock()
	t.armLocked(timeout)
	t.mu.Unlock()
	return ctx, t
}

func (t *responseRunTimer) armLocked(remaining time.Duration) {
	t.generation++
	generation := t.generation
	t.activeAt = t.clock.Now()
	t.timer = t.clock.AfterFunc(remaining, func() {
		t.mu.Lock()
		if t.stopped || t.pauses > 0 || t.generation != generation {
			t.mu.Unlock()
			return
		}
		t.remaining = 0
		t.stopped = true
		t.mu.Unlock()
		t.cancel(errResponseRunTimeout)
	})
}

// refresh starts a fresh inactivity window after an LLM response completes.
func (t *responseRunTimer) refresh() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	t.generation++ // Invalidate a callback already leaving the stopped timer.
	t.remaining = t.timeout
	if t.pauses == 0 {
		t.armLocked(t.remaining)
	}
}

func (t *responseRunTimer) pause() func() {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return func() {}
	}
	t.pauses++
	expired := false
	if t.pauses == 1 {
		if t.timer != nil {
			t.timer.Stop()
		}
		t.generation++ // Invalidate a callback already leaving the stopped timer.
		t.remaining -= t.clock.Now().Sub(t.activeAt)
		if t.remaining <= 0 {
			t.remaining = 0
			t.pauses = 0
			t.stopped = true
			expired = true
		}
	}
	t.mu.Unlock()
	if expired {
		t.cancel(errResponseRunTimeout)
		return func() {}
	}

	var once sync.Once
	return func() {
		once.Do(t.resume)
	}
}

func (t *responseRunTimer) resume() {
	t.mu.Lock()
	if t.stopped || t.pauses == 0 {
		t.mu.Unlock()
		return
	}
	t.pauses--
	if t.pauses > 0 {
		t.mu.Unlock()
		return
	}
	remaining := t.remaining
	if remaining > 0 {
		t.armLocked(remaining)
	}
	t.mu.Unlock()
	if remaining <= 0 {
		t.cancel(errResponseRunTimeout)
	}
}

func (t *responseRunTimer) holdUntil(deadline time.Time) responseRunTimerHold {
	if t == nil || deadline.IsZero() {
		return noopResponseRunTimerHold()
	}
	now := t.clock.Now()
	absoluteCap := now.Add(maxResponseRunDelegationHold)
	expiresAt := deadline.Add(responseRunHoldGrace)
	if expiresAt.After(absoluteCap) {
		expiresAt = absoluteCap
	}
	if !expiresAt.After(now) {
		return noopResponseRunTimerHold()
	}

	resume := t.pause()
	state := &responseRunTimerHoldState{timer: t, resume: resume, expiresAt: expiresAt, absoluteCap: absoluteCap}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return noopResponseRunTimerHold()
	}
	if t.holds == nil {
		t.holds = make(map[*responseRunTimerHoldState]struct{})
	}
	t.holds[state] = struct{}{}
	t.armHoldLocked(state)
	t.mu.Unlock()

	return responseRunTimerHold{
		extend:  state.extend,
		release: state.release,
	}
}

func (t *responseRunTimer) armHoldLocked(state *responseRunTimerHoldState) {
	state.generation++
	generation := state.generation
	delay := state.expiresAt.Sub(t.clock.Now())
	if delay < 0 {
		delay = 0
	}
	state.handle = t.clock.AfterFunc(delay, func() {
		t.mu.Lock()
		if t.stopped || state.generation != generation {
			t.mu.Unlock()
			return
		}
		if _, ok := t.holds[state]; !ok {
			t.mu.Unlock()
			return
		}
		delete(t.holds, state)
		state.handle = nil
		resume := state.resume
		state.resume = nil
		t.mu.Unlock()
		if resume != nil {
			resume() // Expiry preserves the frozen inactivity budget.
		}
	})
}

func (state *responseRunTimerHoldState) extend(deadline time.Time) {
	if state == nil || state.timer == nil || deadline.IsZero() {
		return
	}
	t := state.timer
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	if _, ok := t.holds[state]; !ok {
		return // Released or expired holds cannot be revived.
	}
	if !t.clock.Now().Before(state.expiresAt) {
		return // The deadline is final even if its scheduled callback is delayed.
	}
	expiresAt := deadline.Add(responseRunHoldGrace)
	if expiresAt.After(state.absoluteCap) {
		expiresAt = state.absoluteCap
	}
	if !expiresAt.After(state.expiresAt) {
		return
	}
	if state.handle != nil {
		state.handle.Stop()
	}
	state.expiresAt = expiresAt
	t.armHoldLocked(state)
}

func (state *responseRunTimerHoldState) release() {
	if state == nil || state.timer == nil {
		return
	}
	t := state.timer
	t.mu.Lock()
	if _, ok := t.holds[state]; !ok {
		t.mu.Unlock()
		return
	}
	delete(t.holds, state)
	state.generation++
	if state.handle != nil {
		state.handle.Stop()
		state.handle = nil
	}
	resume := state.resume
	state.resume = nil
	stopped := t.stopped
	t.mu.Unlock()
	if stopped || resume == nil {
		return
	}
	// A normally completed delegation gives the parent a fresh, finite window.
	t.refresh()
	resume()
}

func (t *responseRunTimer) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	t.generation++
	if t.timer != nil {
		t.timer.Stop()
	}
	for hold := range t.holds {
		hold.generation++
		if hold.handle != nil {
			hold.handle.Stop()
			hold.handle = nil
		}
		hold.resume = nil
		delete(t.holds, hold)
	}
	t.mu.Unlock()
	t.cancel(nil)
}

func responseRunTimedOut(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), errResponseRunTimeout)
}

func responseRunTimeoutMessage(timeout time.Duration) string {
	return fmt.Sprintf("Timed out because no LLM response completed within %s. Continue to resume from saved progress, or move long-running investigations to a background job.", humanDuration(timeout))
}

func responseRunDeadlineMessage(runCtx context.Context, timeout time.Duration) string {
	if responseRunTimedOut(runCtx) || (runCtx != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded)) {
		return responseRunTimeoutMessage(timeout)
	}
	return "The model provider request timed out before the response run deadline. Continue to retry from saved progress."
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
		runs:               make(map[string]*responseRun),
		activeBySession:    make(map[string]string),
		idempotencyByKey:   make(map[string]responseRunIdempotencyClaim),
		cleanupTimers:      make(map[string]*time.Timer),
		nextEpochBySession: make(map[string]int64),
		terminalRetention:  retention,
	}
}

func (s *serveServer) responseOwnerID() string {
	s.responseOwnerOnce.Do(func() {
		s.responseOwnerInstanceID = "owner_" + randomSuffix()
	})
	return s.responseOwnerInstanceID
}

func (s *serveServer) ensureResponseRuns() *responseRunManager {
	s.responseRunsOnce.Do(func() {
		if s.responseRuns == nil {
			s.responseRuns = newServeResponseRunManager()
		}
	})
	s.responseOwnerID()
	if s.shutdownCh != nil {
		s.startResponseLifecycle()
	}
	return s.responseRuns
}

func (s *serveServer) startResponseLifecycle() {
	s.responseLifecycleOnce.Do(func() {
		lifecycle, ok := session.AsServeResponseLifecycleStore(s.store)
		if !ok {
			return
		}
		ownerID := s.responseOwnerID()
		ctx, cancel := context.WithCancel(context.Background())
		s.responseLifecycleCancel = cancel
		s.responseLifecycleWG.Add(1)
		go func() {
			defer s.responseLifecycleWG.Done()
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			s.sweepAndRenewResponseLifecycle(ctx, lifecycle, ownerID)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.sweepAndRenewResponseLifecycle(ctx, lifecycle, ownerID)
				}
			}
		}()
	})
}

func (s *serveServer) sweepAndRenewResponseLifecycle(ctx context.Context, lifecycle session.ServeResponseLifecycleStore, processOwnerID string) {
	ctx, release, err := restart.Default.Activity(ctx)
	if err != nil {
		return
	}
	defer release()
	interactionStore, interactionProjectionSupported := session.AsResponseRunInteractionStore(s.store)
	type ownedRun struct {
		run                 *responseRun
		responseID, ownerID string
		token               int64
		leaseExpiresAt      time.Time
		cancel              context.CancelFunc
		interactionState    session.ResponseRunInteractionState
	}
	owned := make([]ownedRun, 0)
	skipRecovery := false
	if s.responseRuns != nil {
		s.responseRuns.mu.Lock()
		runs := make([]*responseRun, 0, len(s.responseRuns.runs))
		for _, run := range s.responseRuns.runs {
			if run != nil {
				runs = append(runs, run)
			}
		}
		s.responseRuns.mu.Unlock()
		for _, run := range runs {
			// Terminalization may briefly own run.mu while committing its durable
			// marker. Skipping that run for one 10s pass is safer than allowing one
			// slow finalizer to delay every other lease renewal.
			if !run.mu.TryLock() {
				skipRecovery = true
				continue
			}
			if run.ownerInstanceID == processOwnerID && run.fencingToken > 0 && run.status == "in_progress" {
				owned = append(owned, ownedRun{run: run, responseID: run.id, ownerID: run.ownerInstanceID,
					token: run.fencingToken, leaseExpiresAt: run.leaseExpiresAt, cancel: run.cancel,
					interactionState: run.interactionStateLocked()})
			}
			run.mu.Unlock()
		}
	}
	semaphore := make(chan struct{}, 32)
	var renewWG sync.WaitGroup
	for _, candidate := range owned {
		candidate := candidate
		renewWG.Add(1)
		go func() {
			defer renewWG.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			renewCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			lease, err := lifecycle.RenewResponseRunLease(renewCtx, candidate.responseID, candidate.ownerID, candidate.token)
			cancel()
			if err != nil {
				s.attentionDiagnostics.LeaseRenewFailures.Add(1)
				log.Printf("[serve] response lifecycle lease renewal failed for %.12s: %v", candidate.responseID, err)
				if candidate.cancel != nil && (errors.Is(err, session.ErrResponseRunLeaseLost) || time.Until(candidate.leaseExpiresAt) <= 12*time.Second) {
					candidate.cancel()
				}
				return
			}
			candidate.run.mu.Lock()
			candidate.run.leaseExpiresAt = lease.LeaseExpiresAt
			candidate.run.mu.Unlock()
			if interactionProjectionSupported {
				projectionCtx, projectionCancel := context.WithTimeout(ctx, 5*time.Second)
				err := interactionStore.SetResponseRunInteractionState(projectionCtx, candidate.interactionState)
				projectionCancel()
				if err != nil && !errors.Is(err, session.ErrResponseRunLeaseLost) {
					log.Printf("[serve] response interaction reconciliation failed for %.12s: %v", candidate.responseID, err)
				}
			}
		}()
	}
	renewWG.Wait()
	// A contended run may be inside its terminal transaction. Do not let this
	// process's sweeper orphan it merely because renewal intentionally used
	// TryLock; the next pass will recover genuinely abandoned rows.
	if skipRecovery {
		return
	}
	recoverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	recovered, err := lifecycle.RecoverExpiredResponseRuns(recoverCtx, 100)
	cancel()
	if len(recovered) > 0 {
		s.attentionDiagnostics.OrphanRecoveries.Add(uint64(len(recovered)))
		s.attentionDiagnostics.MarkerWrites.Add(uint64(len(recovered)))
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[serve] response lifecycle orphan sweep failed: %v", err)
	}
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

func responseRunClaimMatches(claim responseRunIdempotencyClaim, fingerprint string) bool {
	fingerprint = strings.TrimSpace(fingerprint)
	return fingerprint == "" || claim.fingerprint == "" || claim.fingerprint == fingerprint
}

// admitIdempotency serializes preparation of the same logical request, not its
// execution. The owner releases after run admission (or a preparation failure),
// so retries can replay a live run without racing runtime/workspace setup.
func (m *responseRunManager) admitIdempotency(ctx context.Context, scope, key string) (func(), error) {
	claimKey := responseRunIdempotencyScope(scope, key)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m.mu.Lock()
		if pending, ok := m.idempotencyAdmissions[claimKey]; ok {
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending:
				continue
			}
		}
		if m.idempotencyAdmissions == nil {
			m.idempotencyAdmissions = make(map[string]chan struct{})
		}
		pending := make(chan struct{})
		m.idempotencyAdmissions[claimKey] = pending
		m.mu.Unlock()
		return sync.OnceFunc(func() {
			m.mu.Lock()
			delete(m.idempotencyAdmissions, claimKey)
			close(pending)
			m.mu.Unlock()
		}), nil
	}
}

func (m *responseRunManager) createOrGetByIdempotency(run *responseRun, idempotencyKey string) (*responseRun, bool, error) {
	return m.registerRun(run, idempotencyKey, false)
}

// restore registers an existing invocation, not a new admission. Its epoch and
// idempotency claim must match both the retained replay and the durable lease.
func (m *responseRunManager) restore(run *responseRun) error {
	if run == nil || run.runEpoch <= 0 {
		return errors.New("restored response requires an existing run epoch")
	}
	_, _, err := m.registerRun(run, run.idempotencyKey, true)
	return err
}

func (m *responseRunManager) registerRun(run *responseRun, idempotencyKey string, restoring bool) (*responseRun, bool, error) {
	if run == nil || strings.TrimSpace(run.id) == "" {
		return nil, false, fmt.Errorf("response run id is required")
	}
	scope := strings.TrimSpace(run.idempotencyScope)
	if scope == "" {
		scope = run.sessionID
	}
	key := responseRunIdempotencyScope(scope, idempotencyKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, false, fmt.Errorf("server is shutting down")
	}
	if key != "" {
		if claim, ok := m.idempotencyByKey[key]; ok {
			if restoring {
				return nil, false, errResponseRunKeyConflict
			}
			if !responseRunClaimMatches(claim, run.requestFingerprint) {
				return nil, false, errResponseRunKeyConflict
			}
			if existing, exists := m.runs[claim.runID]; exists && existing != nil {
				m.idempotencyReplays.Add(1)
				return existing, true, nil
			}
			if claim.runID != "" {
				delete(m.idempotencyByKey, key)
			}
		}
	}
	if _, exists := m.runs[run.id]; exists {
		return nil, false, fmt.Errorf("response run %q already exists", run.id)
	}
	if !restoring {
		run.runEpoch = max(m.nextEpochBySession[run.sessionID]+1, time.Now().UnixMicro())
	}
	m.nextEpochBySession[run.sessionID] = max(m.nextEpochBySession[run.sessionID], run.runEpoch)
	run.idempotencyKey = strings.TrimSpace(idempotencyKey)
	m.runs[run.id] = run
	if key != "" {
		m.idempotencyByKey[key] = responseRunIdempotencyClaim{runID: run.id, sessionID: run.sessionID, fingerprint: strings.TrimSpace(run.requestFingerprint)}
	}
	return run, false, nil
}

func (m *responseRunManager) reserveSessionForIdempotency(scope, idempotencyKey, fingerprint string) (string, error) {
	key := responseRunIdempotencyScope(scope, idempotencyKey)
	if key == "" {
		return "", fmt.Errorf("idempotency scope and key are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", fmt.Errorf("server is shutting down")
	}
	if claim, ok := m.idempotencyByKey[key]; ok {
		if !responseRunClaimMatches(claim, fingerprint) {
			return "", errResponseRunKeyConflict
		}
		if claim.sessionID != "" {
			return claim.sessionID, nil
		}
	}
	sessionID := session.NewID()
	m.idempotencyByKey[key] = responseRunIdempotencyClaim{
		sessionID:   sessionID,
		fingerprint: strings.TrimSpace(fingerprint),
	}
	return sessionID, nil
}

func (m *responseRunManager) getByIdempotencyKey(sessionID, idempotencyKey string) (*responseRun, bool) {
	run, found, _ := m.getByIdempotencyClaim(sessionID, idempotencyKey, "")
	return run, found
}

func (m *responseRunManager) getByIdempotencyClaim(scope, idempotencyKey, fingerprint string) (*responseRun, bool, error) {
	key := responseRunIdempotencyScope(scope, idempotencyKey)
	if key == "" {
		return nil, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	claim, ok := m.idempotencyByKey[key]
	if !ok {
		return nil, false, nil
	}
	if !responseRunClaimMatches(claim, fingerprint) {
		return nil, false, errResponseRunKeyConflict
	}
	if strings.TrimSpace(claim.runID) == "" {
		return nil, false, nil
	}
	run, ok := m.runs[claim.runID]
	if !ok || run == nil {
		delete(m.idempotencyByKey, key)
		return nil, false, nil
	}
	m.idempotencyReplays.Add(1)
	return run, true, nil
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
	for key, claim := range m.idempotencyByKey {
		if claim.runID == id {
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
		for key, claim := range m.idempotencyByKey {
			if claim.runID == id {
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

func (m *responseRunManager) sessionBoundary(sessionID string) *sync.Mutex {
	boundary, _ := m.boundaries.LoadOrStore(sessionID, &sync.Mutex{})
	return boundary.(*sync.Mutex)
}

func (m *responseRunManager) trySetActiveRun(sessionID, runID string) bool {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return false
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	if t := m.steeringTransition(sessionID); t != nil && t.replacementID != runID {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if active := m.activeBySession[sessionID]; active != "" && active != runID {
		return false
	}
	m.activeBySession[sessionID] = runID
	return true
}

func (m *responseRunManager) setActiveRun(sessionID, runID string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	m.activeBySession[sessionID] = runID
	m.mu.Unlock()
}

func (m *responseRunManager) clearActiveRun(sessionID, runID string) {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(runID) == "" {
		return
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeBySession[sessionID] == runID {
		delete(m.activeBySession, sessionID)
	}
}

func (m *responseRunManager) withExpectedActiveRun(sessionID, expectedID string, expectedEpoch int64, fn func()) bool {
	if m == nil || strings.TrimSpace(sessionID) == "" || fn == nil {
		return false
	}
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	if m.steeringTransition(sessionID) != nil {
		return false
	}
	m.mu.Lock()
	activeID := m.activeBySession[sessionID]
	run := m.runs[activeID]
	matches := strings.TrimSpace(expectedID) == "" || activeID == strings.TrimSpace(expectedID)
	if matches && expectedEpoch > 0 {
		matches = run != nil && run.runEpoch == expectedEpoch
	}
	m.mu.Unlock()
	if !matches {
		return false
	}
	fn()
	return true
}

func (m *responseRunManager) activeRunID(sessionID string) string {
	if strings.TrimSpace(sessionID) == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeBySession[sessionID]
}

func (m *responseRunManager) runIfSessionIdle(sessionID string, fn func()) bool {
	if strings.TrimSpace(sessionID) == "" || fn == nil {
		return false
	}
	// Serialize only work for this session. The manager mutex protects the
	// activity map briefly and is never held across persistence or other I/O.
	boundary := m.sessionBoundary(sessionID)
	boundary.Lock()
	defer boundary.Unlock()
	m.mu.Lock()
	active := m.activeBySession[sessionID] != "" || m.steeringTransition(sessionID) != nil
	m.mu.Unlock()
	if active {
		return false
	}
	fn()
	return true
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
		run.cancelPendingInteractions("cancelled-by-agent")
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

func appendUniqueWebMedia(dst []webMediaEntry, values ...webMediaEntry) []webMediaEntry {
	for _, value := range values {
		if value.URL == "" {
			continue
		}
		seen := false
		for _, existing := range dst {
			if (value.Reference != "" && existing.Reference == value.Reference) ||
				(value.Reference == "" && existing.Reference == "" && existing.URL == value.URL && existing.MediaType == value.MediaType) {
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
	var payload any
	dec := json.NewDecoder(bytes.NewReader(ev.Data))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return err
	}
	data, err := steeringWireJSON(w, payload)
	if err != nil {
		return err
	}
	ev.Data = data
	ev.Event = steeringWireEvent(w, ev.Event)

	b := make([]byte, 0, 4+20+1+7+len(ev.Event)+1+6+len(ev.Data)+2)
	b = append(b, "id: "...)
	b = strconv.AppendInt(b, ev.Sequence, 10)
	b = append(b, "\nevent: "...)
	b = append(b, ev.Event...)
	b = append(b, "\ndata: "...)
	b = append(b, ev.Data...)
	b = append(b, "\n\n"...)
	_, err = w.Write(b)
	return err
}
