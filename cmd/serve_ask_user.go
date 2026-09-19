package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

var (
	errServeAskUserNotPending = errors.New("no pending ask_user request")
	errServeAskUserAnswered   = errors.New("ask_user request already answered")
	errServeAskUserCancelled  = errors.New("cancelled by user")
)

// serveAskUserOrigin attributes a prompt raised by a delegated run. A question
// from a subagent is answered on the parent's surface, so the user has to be
// told which worker is asking and what it was sent to do.
type serveAskUserOrigin struct {
	ChildSessionID string `json:"child_session_id,omitempty"`
	ChildAgent     string `json:"child_agent,omitempty"`
	ChildTask      string `json:"child_task,omitempty"`
	ParentCallID   string `json:"parent_call_id,omitempty"`
}

func (o serveAskUserOrigin) delegated() bool { return strings.TrimSpace(o.ChildSessionID) != "" }

type serveAskUserPrompt struct {
	CallID    string                  `json:"call_id"`
	Questions []tools.AskUserQuestion `json:"questions"`
	CreatedAt int64                   `json:"created_at"`
	Origin    *serveAskUserOrigin     `json:"origin,omitempty"`
}

type serveAskUserSubmission struct {
	Answers []tools.AskUserAnswer
	Err     error
}

type serveAskUserPresentation struct {
	run      *responseRun
	callID   string
	resolved bool
}

// serveAskUserGate is the single consumer shared by every presentation of one
// question. Delegated questions have a raw child presentation and a qualified
// parent presentation, but both aliases must race on this lock and channel.
type serveAskUserGate struct {
	mu            sync.Mutex
	resolveMu     sync.Mutex
	questions     []tools.AskUserQuestion
	responseC     chan serveAskUserSubmission
	responded     bool
	outcome       string
	presentations []*serveAskUserPresentation
	resolved      chan struct{}
	resolveOnce   sync.Once
}

type servePendingAskUser struct {
	CallID    string
	CreatedAt time.Time
	Origin    serveAskUserOrigin
	gate      *serveAskUserGate
}

func newServeAskUserGate(questions []tools.AskUserQuestion) *serveAskUserGate {
	return &serveAskUserGate{
		questions: cloneAskUserQuestions(questions),
		responseC: make(chan serveAskUserSubmission, 1),
		resolved:  make(chan struct{}),
	}
}

func cloneAskUserQuestions(questions []tools.AskUserQuestion) []tools.AskUserQuestion {
	if len(questions) == 0 {
		return nil
	}
	cloned := make([]tools.AskUserQuestion, len(questions))
	for i, q := range questions {
		cloned[i] = q
		if len(q.Options) > 0 {
			cloned[i].Options = append([]tools.AskUserOption(nil), q.Options...)
		}
	}
	return cloned
}

func (p *servePendingAskUser) snapshot() serveAskUserPrompt {
	prompt := serveAskUserPrompt{CallID: p.CallID, CreatedAt: p.CreatedAt.UnixMilli()}
	if p.gate != nil {
		p.gate.mu.Lock()
		prompt.Questions = cloneAskUserQuestions(p.gate.questions)
		p.gate.mu.Unlock()
	}
	if p.Origin.delegated() {
		origin := p.Origin
		prompt.Origin = &origin
	}
	return prompt
}

func (p *servePendingAskUser) bindRun(run *responseRun) {
	if p == nil || p.gate == nil || run == nil {
		return
	}
	presentation := &serveAskUserPresentation{run: run, callID: p.CallID}
	p.gate.mu.Lock()
	for _, existing := range p.gate.presentations {
		if existing.run == run && existing.callID == p.CallID {
			p.gate.mu.Unlock()
			return
		}
	}
	p.gate.presentations = append(p.gate.presentations, presentation)
	resolved, outcome := p.gate.responded, p.gate.outcome
	p.gate.mu.Unlock()
	if resolved {
		p.gate.resolvePresentations(outcome)
	}
}

func (g *serveAskUserGate) resolvePresentations(outcome string) {
	if g == nil || outcome == "" {
		return
	}
	g.resolveMu.Lock()
	defer g.resolveMu.Unlock()
	for {
		g.mu.Lock()
		var next *serveAskUserPresentation
		for _, presentation := range g.presentations {
			if !presentation.resolved {
				presentation.resolved = true
				next = presentation
				break
			}
		}
		if next == nil {
			g.resolveOnce.Do(func() { close(g.resolved) })
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()
		next.run.recordResolvedInteraction("ask_user", next.callID, outcome)
	}
}

func (g *serveAskUserGate) abandon(outcome string) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	if g.responded {
		g.mu.Unlock()
		return false
	}
	g.responded = true
	g.outcome = outcome
	g.mu.Unlock()
	g.resolvePresentations(outcome)
	g.responseC <- serveAskUserSubmission{Err: context.Canceled}
	return true
}

func (rt *serveRuntime) prepareAskUser(callID string, questions []tools.AskUserQuestion) (*servePendingAskUser, serveAskUserPrompt) {
	return rt.prepareAskUserFrom(callID, questions, serveAskUserOrigin{})
}

func (rt *serveRuntime) prepareAskUserFrom(callID string, questions []tools.AskUserQuestion, origin serveAskUserOrigin) (*servePendingAskUser, serveAskUserPrompt) {
	rt.askUserMu.Lock()
	if rt.pendingAskUsers == nil {
		rt.pendingAskUsers = make(map[string]*servePendingAskUser)
	}
	pending := rt.pendingAskUsers[callID]
	if pending == nil {
		pending = &servePendingAskUser{
			CallID:    callID,
			CreatedAt: time.Now(),
			Origin:    origin,
			gate:      newServeAskUserGate(questions),
		}
		rt.pendingAskUsers[callID] = pending
	} else if pending.gate != nil {
		pending.gate.mu.Lock()
		if !pending.gate.responded {
			pending.gate.questions = cloneAskUserQuestions(questions)
		}
		pending.gate.mu.Unlock()
	}
	rt.askUserMu.Unlock()
	return pending, pending.snapshot()
}

// aliasAskUser exposes an existing canonical gate under another runtime and
// call ID. The alias owns only its presentation metadata, never another answer
// channel or responded flag.
func (rt *serveRuntime) aliasAskUser(callID string, source *servePendingAskUser, origin serveAskUserOrigin) (*servePendingAskUser, serveAskUserPrompt) {
	if source == nil || source.gate == nil {
		return rt.prepareAskUserFrom(callID, nil, origin)
	}
	rt.askUserMu.Lock()
	if rt.pendingAskUsers == nil {
		rt.pendingAskUsers = make(map[string]*servePendingAskUser)
	}
	pending := rt.pendingAskUsers[callID]
	if pending == nil || pending.gate != source.gate {
		pending = &servePendingAskUser{CallID: callID, CreatedAt: source.CreatedAt, Origin: origin, gate: source.gate}
		rt.pendingAskUsers[callID] = pending
	}
	rt.askUserMu.Unlock()
	return pending, pending.snapshot()
}

func (rt *serveRuntime) prepareAskUserFromToolArgs(callID string, raw json.RawMessage) (*servePendingAskUser, serveAskUserPrompt, error) {
	if callID == "" {
		return nil, serveAskUserPrompt{}, fmt.Errorf("ask_user missing tool call id")
	}
	var args tools.AskUserArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, serveAskUserPrompt{}, fmt.Errorf("parse ask_user args: %w", err)
	}
	if len(args.Questions) == 0 {
		return nil, serveAskUserPrompt{}, fmt.Errorf("ask_user requires at least one question")
	}
	pending, snap := rt.prepareAskUser(callID, args.Questions)
	return pending, snap, nil
}

func (rt *serveRuntime) awaitAskUser(ctx context.Context, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error) {
	callID := llm.CallIDFromContext(ctx)
	if callID == "" {
		return nil, fmt.Errorf("ask_user missing tool call id")
	}
	resumeResponseTimeout := rt.pauseForInteractiveWait()
	defer resumeResponseTimeout()
	pending, _ := rt.prepareAskUser(callID, questions)
	defer rt.removePendingAskUser(callID, pending)
	return pending.await(ctx)
}

func (p *servePendingAskUser) await(ctx context.Context) ([]tools.AskUserAnswer, error) {
	if p == nil || p.gate == nil {
		return nil, errServeAskUserNotPending
	}
	select {
	case submission := <-p.gate.responseC:
		return submission.Answers, submission.Err
	case <-ctx.Done():
		if p.gate.abandon("cancelled-by-agent") {
			return nil, ctx.Err()
		}
		// An endpoint won the canonical gate concurrently with cancellation.
		// Consume that real answer rather than reporting a false successful POST.
		submission := <-p.gate.responseC
		return submission.Answers, submission.Err
	}
}

func (rt *serveRuntime) removePendingAskUser(callID string, pending *servePendingAskUser) {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	if current := rt.pendingAskUsers[callID]; current == pending {
		delete(rt.pendingAskUsers, callID)
	}
}

func (rt *serveRuntime) clearPendingAskUser(callID string) {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	delete(rt.pendingAskUsers, callID)
}

func (rt *serveRuntime) submitAskUser(callID string, answers []tools.AskUserAnswer, cancelled bool) ([]tools.AskUserAnswer, error) {
	rt.askUserMu.Lock()
	pending := rt.pendingAskUsers[callID]
	rt.askUserMu.Unlock()
	if pending == nil || pending.gate == nil {
		return nil, errServeAskUserNotPending
	}

	pending.gate.mu.Lock()
	questions := cloneAskUserQuestions(pending.gate.questions)
	pending.gate.mu.Unlock()
	submission := serveAskUserSubmission{}
	var normalized []tools.AskUserAnswer
	outcome := "answered"
	if cancelled {
		submission.Err = errServeAskUserCancelled
		outcome = "cancelled-by-user"
	} else {
		var err error
		normalized, err = tools.NormalizeAskUserAnswers(questions, answers)
		if err != nil {
			return nil, err
		}
		submission.Answers = normalized
	}

	// Keep the alias alive until the canonical claim is made. Separate runtime
	// locks cannot serialize child and parent endpoints, so only the gate decides
	// the winner.
	rt.askUserMu.Lock()
	if rt.pendingAskUsers[callID] != pending {
		rt.askUserMu.Unlock()
		return nil, errServeAskUserNotPending
	}
	pending.gate.mu.Lock()
	if pending.gate.responded {
		resolved := pending.gate.resolved
		pending.gate.mu.Unlock()
		rt.askUserMu.Unlock()
		<-resolved
		return nil, errServeAskUserAnswered
	}
	pending.gate.responded = true
	pending.gate.outcome = outcome
	pending.gate.mu.Unlock()
	rt.askUserMu.Unlock()

	// Resolve every visible response before releasing the worker. This keeps the
	// aliases alive until concurrent retries can observe their idempotent result.
	pending.gate.resolvePresentations(outcome)
	pending.gate.responseC <- submission
	return normalized, nil
}

func (rt *serveRuntime) pendingAskUserPrompts() []serveAskUserPrompt {
	rt.askUserMu.Lock()
	defer rt.askUserMu.Unlock()
	return sortedPendingSnapshots(rt.pendingAskUsers,
		func(pending *servePendingAskUser) serveAskUserPrompt { return pending.snapshot() },
		func(prompt serveAskUserPrompt) int64 { return prompt.CreatedAt },
	)
}

func (rt *serveRuntime) clearPendingAskUsers() {
	rt.askUserMu.Lock()
	pending := rt.pendingAskUsers
	rt.pendingAskUsers = nil
	rt.askUserMu.Unlock()
	for _, prompt := range pending {
		if prompt != nil && prompt.gate != nil {
			prompt.gate.abandon("cancelled-by-agent")
		}
	}
}
