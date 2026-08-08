package guardian

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	DefaultTimeout = 90 * time.Second
	// Bound process-local guardian review sessions so long-running auto mode does
	// not grow provider context indefinitely. A fresh full prompt re-baselines the
	// cache after this many successful reviews.
	maxReviewSessionTurns = 50
)

type PromptMode int

const (
	PromptModeFull PromptMode = iota
	PromptModeDelta
)

type TranscriptEntry struct {
	Role string
	Text string
}

type Request struct {
	Command          string
	WorkDir          string
	ToolName         string
	Path             string
	Selector         string
	IsWrite          bool
	IsDirectory      bool
	Transcript       []TranscriptEntry
	TranscriptOffset int
	ApprovalContext  string
	Policy           string
	PromptMode       PromptMode
	ScopeID          string
	WorkspaceAccess  string
	Reason           string
}

type Decision struct {
	RiskLevel         string    `json:"risk_level"`
	UserAuthorization string    `json:"user_authorization"`
	Outcome           string    `json:"outcome"`
	Rationale         string    `json:"rationale"`
	Model             string    `json:"-"`
	Usage             llm.Usage `json:"-"`
}

func (d Decision) Allowed() bool { return strings.EqualFold(strings.TrimSpace(d.Outcome), "allow") }

type Reviewer struct {
	Provider llm.Provider
	Model    string
	Policy   string
	Timeout  time.Duration

	mu                    sync.Mutex
	sessionActive         bool
	scopeID               string
	transcriptCount       int
	transcriptFingerprint uint64
	reviewMessages        []llm.Message
	reviewTurnCount       int
}

// ReviewerFactory constructs an independently stateful reviewer. A provider
// instance must not be shared by multiple reviewers returned by the factory.
type ReviewerFactory func() (*Reviewer, error)

// ReviewerPool bounds parallel policy checks while preserving each reviewer's
// provider conversation state. It creates one primary reviewer eagerly, expands
// lazily under contention, and reuses idle reviewers in LIFO order so sequential
// reviews keep a warm delta session.
type ReviewerPool struct {
	mu                sync.Mutex
	max               int
	total             int
	idle              []*Reviewer
	all               []*Reviewer
	factory           ReviewerFactory
	expansionDisabled bool
	changed           chan struct{}
	closed            bool
	closeCtx          context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	closeOnce         sync.Once
}

func NewReviewerPool(max int, factory ReviewerFactory) (*ReviewerPool, error) {
	if max <= 0 {
		return nil, fmt.Errorf("guardian reviewer pool size must be positive")
	}
	if factory == nil {
		return nil, fmt.Errorf("guardian reviewer factory is nil")
	}
	primary, err := factory()
	if err != nil {
		return nil, err
	}
	if primary == nil {
		return nil, fmt.Errorf("guardian reviewer factory returned nil")
	}
	closeCtx, cancel := context.WithCancel(context.Background())
	return &ReviewerPool{
		max:      max,
		total:    1,
		idle:     []*Reviewer{primary},
		all:      []*Reviewer{primary},
		factory:  factory,
		changed:  make(chan struct{}),
		closeCtx: closeCtx,
		cancel:   cancel,
	}, nil
}

func (p *ReviewerPool) Review(ctx context.Context, req Request) (Decision, error) {
	if p == nil {
		return Decision{}, fmt.Errorf("guardian reviewer pool is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reviewer, err := p.acquire(ctx)
	if err != nil {
		return Decision{}, err
	}
	defer p.release(reviewer)

	reviewCtx, cancel := context.WithCancel(ctx)
	stopCloseCancel := context.AfterFunc(p.closeCtx, cancel)
	defer func() {
		stopCloseCancel()
		cancel()
	}()
	return reviewer.Review(reviewCtx, req)
}

func (p *ReviewerPool) acquire(ctx context.Context) (*Reviewer, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, fmt.Errorf("guardian reviewer pool is closed")
		}
		if last := len(p.idle) - 1; last >= 0 {
			reviewer := p.idle[last]
			p.idle = p.idle[:last]
			p.wg.Add(1)
			p.mu.Unlock()
			return reviewer, nil
		}
		if !p.expansionDisabled && p.total < p.max {
			p.total++ // Reserve capacity before constructing outside the lock.
			p.wg.Add(1)
			p.mu.Unlock()

			reviewer, err := p.factory()
			p.mu.Lock()
			if err != nil || reviewer == nil {
				p.total--
				// A usable primary already exists. Degrade to that capacity instead
				// of failing all Guardian setup because optional expansion failed.
				p.expansionDisabled = true
				p.notifyChangedLocked()
				p.mu.Unlock()
				p.wg.Done()
				continue
			}
			if p.closed {
				p.total--
				p.mu.Unlock()
				p.wg.Done()
				cleanupReviewer(reviewer)
				return nil, fmt.Errorf("guardian reviewer pool is closed")
			}
			p.all = append(p.all, reviewer)
			p.mu.Unlock()
			return reviewer, nil
		}
		changed := p.changed
		closeCtx := p.closeCtx
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-closeCtx.Done():
			return nil, fmt.Errorf("guardian reviewer pool is closed")
		case <-changed:
		}
	}
}

func (p *ReviewerPool) release(reviewer *Reviewer) {
	p.mu.Lock()
	if !p.closed {
		p.idle = append(p.idle, reviewer)
	}
	p.notifyChangedLocked()
	p.mu.Unlock()
	p.wg.Done()
}

func (p *ReviewerPool) notifyChangedLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// Close cancels in-flight checks, waits for their reviewer locks to be released,
// then resets and cleans every provider owned by the pool. It is idempotent.
func (p *ReviewerPool) Close() {
	if p == nil {
		return
	}
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		p.notifyChangedLocked()
		p.mu.Unlock()

		p.wg.Wait()
		for _, reviewer := range p.all {
			cleanupReviewer(reviewer)
		}
	})
}

func cleanupReviewer(reviewer *Reviewer) {
	if reviewer == nil {
		return
	}
	reviewer.Reset()
	if cleaner, ok := reviewer.Provider.(llm.ProviderCleaner); ok {
		cleaner.CleanupMCP()
	}
}

func (r *Reviewer) Review(ctx context.Context, req Request) (Decision, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.Provider == nil {
		return Decision{}, fmt.Errorf("guardian provider is nil")
	}
	if r.sessionActive && (r.reviewTurnCount >= maxReviewSessionTurns || r.shouldResetForRequestLocked(req)) {
		r.resetLocked()
	}

	mode := PromptModeFull
	transcript := req.Transcript
	transcriptOffset := 0
	if r.sessionActive {
		mode = PromptModeDelta
		transcriptOffset = r.transcriptCount
		transcript = req.Transcript[r.transcriptCount:]
	}

	policy := strings.TrimSpace(req.Policy)
	if policy == "" {
		policy = strings.TrimSpace(r.Policy)
	}
	if policy == "" {
		policy = DefaultPolicy
	}
	promptReq := req
	promptReq.Transcript = transcript
	promptReq.TranscriptOffset = transcriptOffset
	promptReq.PromptMode = mode

	turnMessages := r.turnMessages(policy, promptReq, mode)
	providerMessages := make([]llm.Message, 0, len(r.reviewMessages)+len(turnMessages))
	providerMessages = append(providerMessages, r.reviewMessages...)
	providerMessages = append(providerMessages, turnMessages...)

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	raw, usage, err := r.runReviewRequest(ctx, providerMessages)
	accounting := Decision{Model: strings.TrimSpace(r.Model), Usage: usage}
	if err != nil {
		r.resetLocked()
		return accounting, err
	}
	decision, err := ParseDecision(raw)
	if err != nil {
		r.resetLocked()
		return accounting, err
	}
	decision.Model = accounting.Model
	decision.Usage = accounting.Usage

	r.reviewMessages = append(providerMessages, llm.AssistantText(canonicalDecisionJSON(decision)))
	r.sessionActive = true
	r.scopeID = strings.TrimSpace(req.ScopeID)
	r.transcriptCount = len(req.Transcript)
	r.transcriptFingerprint = transcriptFingerprint(req.Transcript)
	r.reviewTurnCount++
	return decision, nil
}

func (r *Reviewer) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetLocked()
}

func (r *Reviewer) resetLocked() {
	r.sessionActive = false
	r.scopeID = ""
	r.transcriptCount = 0
	r.transcriptFingerprint = 0
	r.reviewMessages = nil
	r.reviewTurnCount = 0
	if r.Provider != nil {
		if resetter, ok := r.Provider.(interface{ ResetConversation() }); ok {
			resetter.ResetConversation()
		}
	}
}

func (r *Reviewer) shouldResetForRequestLocked(req Request) bool {
	if len(req.Transcript) < r.transcriptCount {
		return true
	}
	if strings.TrimSpace(req.ScopeID) != r.scopeID {
		return true
	}
	if transcriptFingerprint(req.Transcript[:r.transcriptCount]) != r.transcriptFingerprint {
		return true
	}
	return false
}

func (r *Reviewer) turnMessages(policy string, req Request, mode PromptMode) []llm.Message {
	messages := []llm.Message{}
	if mode == PromptModeFull {
		messages = append(messages, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: policy + "\n\nReturn strict JSON only, with no markdown fences or commentary. Fields: risk_level, user_authorization, outcome, rationale. risk_level must be one of low, medium, high, critical. user_authorization must be one of high, medium, low, unknown. outcome must be allow or deny."}}})
	}
	messages = append(messages, llm.UserText(BuildPrompt(req)))
	return messages
}

func (r *Reviewer) runReviewRequest(ctx context.Context, messages []llm.Message) (string, llm.Usage, error) {
	stream, err := r.Provider.Stream(ctx, llm.Request{Model: r.Model, Messages: messages, MaxOutputTokens: 2000, Temperature: 0, TemperatureSet: true})
	if err != nil {
		return "", llm.Usage{}, err
	}
	defer stream.Close()
	var b strings.Builder
	var usage llm.Usage
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", usage, err
		}
		switch event.Type {
		case llm.EventTextDelta:
			b.WriteString(event.Text)
		case llm.EventUsage:
			if event.Use != nil {
				usage.Add(*event.Use)
			}
		case llm.EventError:
			if event.Err != nil {
				return "", usage, event.Err
			}
		}
	}
	return b.String(), usage, nil
}

func canonicalDecisionJSON(d Decision) string {
	if d.Allowed() && strings.TrimSpace(d.RiskLevel) == "" && strings.TrimSpace(d.UserAuthorization) == "" && strings.TrimSpace(d.Rationale) == "" {
		return `{"outcome":"allow"}`
	}
	b, err := json.Marshal(d)
	if err != nil {
		outcome := strings.TrimSpace(d.Outcome)
		if outcome == "" {
			outcome = "deny"
		}
		return fmt.Sprintf(`{"outcome":%q}`, outcome)
	}
	return string(b)
}

func transcriptFingerprint(entries []TranscriptEntry) uint64 {
	h := fnv.New64a()
	for _, entry := range entries {
		_, _ = h.Write([]byte(entry.Role))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(entry.Text))
		_, _ = h.Write([]byte{0xff})
	}
	return h.Sum64()
}

func LoadPolicy(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return DefaultPolicy, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func ParseDecision(text string) (Decision, error) {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j >= i {
			text = text[i : j+1]
		}
	}
	var d Decision
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		return Decision{}, err
	}
	outcome := strings.ToLower(strings.TrimSpace(d.Outcome))
	if outcome != "allow" && outcome != "deny" {
		return Decision{}, fmt.Errorf("invalid guardian outcome %q", d.Outcome)
	}
	d.Outcome = outcome
	d.RiskLevel = strings.ToLower(strings.TrimSpace(d.RiskLevel))
	d.UserAuthorization = strings.ToLower(strings.TrimSpace(d.UserAuthorization))
	d.Rationale = strings.TrimSpace(d.Rationale)
	if d.Rationale == "" {
		d.Rationale = "no rationale provided"
	}
	return d, nil
}
