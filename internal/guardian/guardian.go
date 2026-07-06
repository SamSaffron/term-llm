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
	Transcript       []TranscriptEntry
	TranscriptOffset int
	ApprovalContext  string
	Policy           string
	PromptMode       PromptMode
	ScopeID          string
}

type Decision struct {
	RiskLevel         string `json:"risk_level"`
	UserAuthorization string `json:"user_authorization"`
	Outcome           string `json:"outcome"`
	Rationale         string `json:"rationale"`
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

	raw, err := r.runReviewRequest(ctx, providerMessages)
	if err != nil {
		r.resetLocked()
		return Decision{}, err
	}
	decision, err := ParseDecision(raw)
	if err != nil {
		r.resetLocked()
		return Decision{}, err
	}

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

func (r *Reviewer) runReviewRequest(ctx context.Context, messages []llm.Message) (string, error) {
	stream, err := r.Provider.Stream(ctx, llm.Request{Model: r.Model, Messages: messages, MaxOutputTokens: 2000, Temperature: 0, TemperatureSet: true})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	var b strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch event.Type {
		case llm.EventTextDelta:
			b.WriteString(event.Text)
		case llm.EventError:
			if event.Err != nil {
				return "", event.Err
			}
		}
	}
	return b.String(), nil
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
