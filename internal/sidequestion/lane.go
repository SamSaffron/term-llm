package sidequestion

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/runboundary"
)

// Anchor is the main-conversation boundary a side lane branches from. It is one
// value with one owner: the messages, the provider state they were captured
// with, and the provider/model identity that state belongs to always travel
// together, so new identity can never end up attached to an old snapshot.
type Anchor struct {
	Messages        []llm.Message
	Provider        runboundary.ProviderContext
	DurableAnchorID int64
	Durable         bool
}

// Clone returns a detached copy safe to retain across turns.
func (a Anchor) Clone() Anchor {
	a.Messages = CloneMessages(a.Messages)
	a.Provider.State = append([]byte(nil), a.Provider.State...)
	return a
}

// TurnMode reports how one lane turn reaches the model.
type TurnMode int

const (
	// TurnReplay rebuilds a bounded standalone request from the anchor.
	TurnReplay TurnMode = iota
	// TurnFork creates the lane by branching the anchor's provider session.
	TurnFork
	// TurnResume continues the lane's own provider session.
	TurnResume
)

func (m TurnMode) String() string {
	switch m {
	case TurnFork:
		return "fork"
	case TurnResume:
		return "resume"
	default:
		return "replay"
	}
}

// Lane is one side discussion: the boundary it branched from, its own
// transcript, and — when the provider can branch from a captured boundary — its
// own provider session carried across turns.
//
// A lane is a lifecycle object, not data. Callers must cancel any running turn
// and then Close it on panel close, panel clear, and every boundary
// invalidation.
type Lane struct {
	mu     sync.Mutex
	anchor Anchor
	// requestPrefix is everything the lane's provider has already been given,
	// plus its replies. It is the provider-facing shape: it keeps the anchor
	// prefix so the recorded resume offset and digest still line up on the next
	// turn. It is not the lane's conversation.
	requestPrefix []llm.Message
	// transcript is the lane's own conversation — policy, questions and answers,
	// without the main transcript it branched from. Promotion writes exactly
	// these into a child session under the lane's anchor.
	transcript    []llm.Message
	usage         []llm.Usage
	provider      llm.Provider
	providerState []byte
	providerKey   string
	model         string
	sessionID     string
	pending       *Turn
}

// TurnRequest is everything one lane turn needs. Both the TUI and the web
// runtime build this instead of repeating the provider/budget/request prelude.
type TurnRequest struct {
	Question        string
	Anchor          Anchor
	History         []Entry
	ProviderKey     string
	Model           string
	ReasoningEffort string
	ReasoningMode   string
	// InputLimit is the runtime's own input budget, when it has one. Zero falls
	// back to the provider/model helper budget.
	InputLimit int
	// AllowFork gates lane creation from the anchor's live provider session.
	AllowFork bool
	// ForkStillAllowed is re-evaluated immediately before a branch streams. The
	// gate cannot be a single snapshot: a main run can start between preparing a
	// branch and launching it, and branching a session another process is writing
	// is exactly what AllowFork exists to avoid. Returning false downgrades the
	// prepared branch to the bounded replay path.
	ForkStillAllowed func() bool
	// ForkSource is the live provider the lane branches from. It is never used
	// for the request itself, and is never mutated.
	ForkSource llm.Provider
	// NewProvider builds a standalone provider for the bounded replay path.
	NewProvider func(providerKey, model string) (llm.Provider, error)
	// SessionID is the term-llm session a promoted lane belongs to. Empty for an
	// unpromoted lane, which keeps its turns out of session state entirely.
	SessionID string
}

// Turn is one prepared lane turn.
type Turn struct {
	lane     *Lane
	mode     TurnMode
	provider llm.Provider
	request  llm.Request
	// stillAllowed and replay let a prepared branch downgrade to a bounded replay
	// if a main run starts before it streams.
	stillAllowed func() bool
	replay       func() (llm.Provider, llm.Request, error)
	// failure records a downgrade that could not be completed, so the turn fails
	// closed instead of streaming from a branch the gate forbids.
	failure error
	// started and released are guarded by lane.mu. A prepared turn owns nothing
	// until it runs, so teardown before Run must release its provider itself
	// rather than waiting for a completion that will never happen.
	started   bool
	released  bool
	cancelled bool
}

// Mode reports how this turn reaches the model.
func (t *Turn) Mode() TurnMode { return t.mode }

// Request exposes the prepared provider request for assertions and debugging.
func (t *Turn) Request() llm.Request { return t.request }

// Abandon releases a prepared turn that will never run. A branch created for it
// is dropped; an already established lane session is kept, because nothing was
// sent to it.
func (t *Turn) Abandon() {
	if t == nil {
		return
	}
	l := t.lane
	l.mu.Lock()
	if l.pending == t {
		l.pending = nil
	}
	retained := t.mode == TurnResume && l.provider == t.provider
	if t.mode == TurnFork && l.provider == t.provider {
		l.clearSessionLocked()
	}
	release := !retained && !t.released
	t.released = t.released || release
	l.mu.Unlock()
	if release {
		cleanupProvider(t.provider)
	}
}

// Anchor returns the boundary this lane branched from.
func (l *Lane) Anchor() Anchor {
	if l == nil {
		return Anchor{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.anchor.Clone()
}

// Continuing reports whether the lane holds its own provider session. While it
// does, the lane is anchored: it no longer re-reads the main boundary.
func (l *Lane) Continuing() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.provider != nil && len(l.requestPrefix) > 0
}

// Transcript returns the lane's own conversation, without the main transcript it
// branched from. Promotion writes exactly these into a child session.
func (l *Lane) Transcript() []llm.Message {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return CloneMessages(l.transcript)
}

// ProviderState returns the lane provider's exported transport state. The live
// clone is not serializable; this is, which is what promotion needs.
func (l *Lane) ProviderState() []byte {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.providerState...)
}

// RequestPrefix returns everything the lane's provider has already been given,
// plus its replies. The next turn appends to it so the provider's recorded
// resume offset and transcript digest still line up.
func (l *Lane) RequestPrefix() []llm.Message {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return CloneMessages(l.requestPrefix)
}

// Usage returns per-turn usage for the lane's own exchanges.
func (l *Lane) Usage() []llm.Usage {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]llm.Usage(nil), l.usage...)
}

// Close releases the lane's provider and drops its anchor and transcript. A turn
// that is still running owns the provider lifetime and releases it itself, so
// callers should cancel before closing to avoid a detached background process.
func (l *Lane) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	provider := l.provider
	var prepared llm.Provider
	if pending := l.pending; pending != nil {
		if pending.started {
			// A running turn is no longer the lane's current turn, so its own
			// completion releases the provider. Releasing here too would clean up
			// an in-flight process twice.
			provider = nil
		} else if !pending.released {
			// A turn prepared but never started has no completion to wait for, so
			// its provider would otherwise leak.
			pending.released = true
			prepared = pending.provider
			if prepared == provider {
				provider = nil
			}
		}
		pending.cancelled = true
		l.pending = nil
	}
	l.clearSessionLocked()
	l.anchor = Anchor{}
	l.providerKey, l.model, l.sessionID = "", "", ""
	l.mu.Unlock()
	cleanupProvider(provider)
	cleanupProvider(prepared)
}

// clearSessionLocked drops everything that belongs to a lane's own provider
// session. It never releases the provider itself; the caller decides whether a
// running turn still owns that.
func (l *Lane) clearSessionLocked() {
	l.provider, l.providerState = nil, nil
	l.requestPrefix, l.transcript, l.usage = nil, nil, nil
}

// PrepareTurn decides, for one question, whether the lane continues, is created
// by branching the anchor's provider session, or falls back to a bounded replay.
//
// Eligibility lives here, once, rather than at each call site.
func (l *Lane) PrepareTurn(request TurnRequest) (*Turn, error) {
	if l == nil {
		return nil, errors.New("side question lane is unavailable")
	}
	question := strings.TrimSpace(request.Question)
	if question == "" {
		return nil, errors.New("question is required")
	}
	if request.NewProvider == nil {
		return nil, errors.New("side questions are unavailable for this runtime")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != nil {
		return nil, errors.New("A side question is already running")
	}

	// The replay recipe is built up front so a prepared branch can still be
	// downgraded once, immediately before it streams.
	replay := func() (llm.Provider, llm.Request, error) {
		provider, err := request.NewProvider(request.ProviderKey, request.Model)
		if err != nil {
			return nil, llm.Request{}, err
		}
		messages, err := BuildMessages(request.Anchor.Messages, request.History, question,
			request.ProviderKey, request.Model, request.InputLimit)
		if err != nil {
			cleanupProvider(provider)
			return nil, llm.Request{}, err
		}
		return provider, buildTurnRequest(request, messages, request.Anchor.Provider.WorkingDir, request.SessionID), nil
	}

	if l.canContinueLocked(request) {
		messages := append(CloneMessages(l.requestPrefix), llm.UserText(question))
		// A continuing lane keeps its own project directory and session identity,
		// not whatever the latest main boundary reports.
		req := buildTurnRequest(request, messages, l.anchor.Provider.WorkingDir, l.sessionID)
		return l.startTurnLocked(TurnResume, l.provider, req, nil, nil), nil
	}
	l.releaseProviderLocked()

	if l.forkEligibleLocked(request) {
		// The request carries the whole anchor; the provider derives what still
		// has to cross the wire from the captured offset. Request shape and wire
		// shape are deliberately different here.
		messages := append(CloneMessages(request.Anchor.Messages), policyMessage(), llm.UserText(question))
		if forked, ok := llm.ForkHelperAtBoundary(request.ForkSource, request.Anchor.Provider.State, messages); ok {
			l.adoptLocked(request)
			l.provider = forked
			req := buildTurnRequest(request, messages, request.Anchor.Provider.WorkingDir, request.SessionID)
			return l.startTurnLocked(TurnFork, forked, req, request.ForkStillAllowed, replay), nil
		}
	}

	provider, req, err := replay()
	if err != nil {
		return nil, err
	}
	l.adoptLocked(request)
	return l.startTurnLocked(TurnReplay, provider, req, nil, nil), nil
}

// canContinueLocked reports whether the existing lane session still answers this
// question.
//
// A lane that owns a session is anchored: it keeps answering from the boundary
// it branched at rather than re-reading the main conversation. That is branch
// semantics, and it is also the only lossless option — its prior exchanges live
// in its own session, and re-anchoring would mean branching a fresh session that
// has never seen them.
func (l *Lane) canContinueLocked(request TurnRequest) bool {
	if l.provider == nil || len(l.requestPrefix) == 0 {
		return false
	}
	if l.providerKey != request.ProviderKey || l.model != request.Model {
		return false
	}
	// Claude Code keys sessions by project directory, so a lane whose project
	// directory moved cannot resume its own session from the new one.
	return l.anchor.Provider.WorkingDir == request.Anchor.Provider.WorkingDir
}

func (l *Lane) forkEligibleLocked(request TurnRequest) bool {
	// Prior side exchanges only reach a new branch through the replay path: a
	// freshly branched session has never seen them, and re-delivering them would
	// mean flattening real answers into text. Branch only when there is nothing
	// to carry.
	return request.AllowFork &&
		len(request.History) == 0 &&
		len(request.Anchor.Messages) > 0 &&
		request.Anchor.Provider.Available() &&
		request.Anchor.Provider.MatchesProviderModel(request.ProviderKey, request.Model) &&
		request.ForkSource != nil &&
		llm.SupportsHelperBoundaryFork(request.ForkSource)
}

func (l *Lane) adoptLocked(request TurnRequest) {
	l.anchor = request.Anchor.Clone()
	l.providerKey, l.model = request.ProviderKey, request.Model
	l.sessionID = request.SessionID
	l.requestPrefix, l.transcript, l.usage, l.providerState = nil, nil, nil, nil
}

func (l *Lane) startTurnLocked(mode TurnMode, provider llm.Provider, req llm.Request,
	stillAllowed func() bool, replay func() (llm.Provider, llm.Request, error)) *Turn {
	turn := &Turn{lane: l, mode: mode, provider: provider, request: req, stillAllowed: stillAllowed, replay: replay}
	l.pending = turn
	return turn
}

func (l *Lane) releaseProviderLocked() {
	provider := l.provider
	l.clearSessionLocked()
	if provider != nil {
		cleanupProvider(provider)
	}
}

// buildTurnRequest assembles one lane turn's provider request. workingDir is
// passed explicitly because Claude Code keys sessions by project directory: a
// branch launched from a different cwd would not find the session to resume.
func buildTurnRequest(request TurnRequest, messages []llm.Message, workingDir, sessionID string) llm.Request {
	return llm.Request{
		Model:           request.Model,
		Messages:        messages,
		ReasoningEffort: request.ReasoningEffort,
		WorkingDir:      workingDir,
		SessionID:       sessionID,
		Responses:       &llm.ResponsesOptions{ReasoningMode: request.ReasoningMode},
	}
}

// Run performs this turn's single provider request and folds the result back
// into the lane.
//
// Ephemeral is decided here and nowhere else: only a turn whose provider came
// from the boundary fork seam runs non-ephemerally, so a Responses helper turn
// can never chain onto the live conversation by convention or by mistake.
func (t *Turn) Run(ctx context.Context, emit func(llm.Event)) (result Result, err error) {
	if !t.claim() {
		// The lane was torn down before this turn started, so its provider has
		// already been released by whoever tore it down.
		return Result{}, errors.New("side question was cancelled before it started")
	}
	// finishTurn must run even if the gate callback, the replay fallback or the
	// provider panics, or the lane stays wedged on a turn that never completes.
	defer func() { t.lane.finishTurn(t, result, err) }()
	t.downgradeStaleFork()
	if t.failure != nil {
		return Result{}, t.failure
	}
	result, err = runRequest(ctx, t.provider, t.request, t.mode == TurnReplay, emit)
	return result, err
}

// claim marks the turn as owning its provider's lifetime. Until it succeeds,
// teardown is responsible for releasing a prepared provider.
func (t *Turn) claim() bool {
	l := t.lane
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.cancelled {
		return false
	}
	t.started = true
	return true
}

// downgradeStaleFork re-checks the fork gate immediately before streaming. A
// main run can start between preparing a branch and launching it, and the whole
// point of the gate is not to branch a session another process is writing.
func (t *Turn) downgradeStaleFork() {
	if t.mode != TurnFork || t.stillAllowed == nil || t.replay == nil || t.stillAllowed() {
		return
	}
	branch := t.provider
	l := t.lane
	provider, request, err := t.replay()
	l.mu.Lock()
	if l.provider == branch {
		l.clearSessionLocked()
	}
	if err != nil {
		// Fail closed. Branching a session another process is writing is exactly
		// what the gate forbids, so a question that cannot fall back is refused
		// rather than answered from a prohibited branch.
		t.provider, t.failure = nil, err
	} else {
		t.mode, t.provider, t.request = TurnReplay, provider, request
	}
	l.mu.Unlock()
	cleanupProvider(branch)
}

func (l *Lane) finishTurn(turn *Turn, result Result, err error) {
	release := turn.mode == TurnReplay
	l.mu.Lock()
	if l.pending != turn {
		// The lane was cleared, closed or re-anchored while this turn ran.
		release, turn.released = !turn.released, true
		l.mu.Unlock()
		if release {
			cleanupProvider(turn.provider)
		}
		return
	}
	l.pending = nil
	if turn.mode != TurnReplay {
		switch {
		case err != nil, result.Synthetic, strings.TrimSpace(result.Response) == "":
			// A failed, cancelled or refused lane turn leaves the branched CLI
			// session in an unverified place. Drop it so the next question
			// re-branches or replays rather than resuming something unknown.
			release = true
			l.clearSessionLocked()
		default:
			answer := llm.AssistantText(result.Response)
			// The provider-facing prefix keeps the anchor so the recorded resume
			// offset and digest still line up next turn.
			l.requestPrefix = append(CloneMessages(turn.request.Messages), answer)
			// The lane's own conversation is only what this branch added. Writing
			// the request prefix into a promoted child would duplicate the main
			// transcript the child already inherits from its branch point.
			l.transcript = append(l.transcript, laneTurnMessages(turn, answer)...)
			l.usage = append(l.usage, result.Usage)
			l.providerState = nil
			if exporter, ok := turn.provider.(llm.ProviderStateExporter); ok {
				if state, exported := exporter.ExportProviderState(); exported {
					l.providerState = state
				}
			}
		}
	}
	release = release && !turn.released
	turn.released = turn.released || release
	l.mu.Unlock()
	if release {
		cleanupProvider(turn.provider)
	}
}

// laneTurnMessages extracts the messages this turn added on top of what the lane
// already held: the policy and question for a branch, the question alone for a
// continuation, plus the answer.
func laneTurnMessages(turn *Turn, answer llm.Message) []llm.Message {
	messages := turn.request.Messages
	added := 1 // the question
	if turn.mode == TurnFork {
		added = 2 // the policy travels with the branch's first turn
	}
	if len(messages) < added {
		return []llm.Message{answer}
	}
	return append(CloneMessages(messages[len(messages)-added:]), answer)
}

func cleanupProvider(provider llm.Provider) {
	if cleaner, ok := provider.(llm.ProviderCleaner); ok {
		cleaner.CleanupMCP()
	}
}

func policyMessage() llm.Message {
	return llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: SystemPolicy}}}
}
