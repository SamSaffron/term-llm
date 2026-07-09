package session

import "time"

// GoalStatus represents the lifecycle state of a persistent session goal.
type GoalStatus string

const (
	GoalStatusActive        GoalStatus = "active"
	GoalStatusPaused        GoalStatus = "paused"
	GoalStatusComplete      GoalStatus = "complete"
	GoalStatusBlocked       GoalStatus = "blocked"
	GoalStatusBudgetLimited GoalStatus = "budget_limited"
)

// Goal is the persisted, cross-frontend state for a session objective.
// A nil *Goal means the session has no goal configured.
type Goal struct {
	Objective       string     `json:"objective"`
	Status          GoalStatus `json:"status"`
	TokenBudget     int        `json:"token_budget,omitempty"`
	TokensUsed      int        `json:"tokens_used,omitempty"`
	TimeUsedSeconds int        `json:"time_used_seconds,omitempty"`
	CreatedAt       time.Time  `json:"created_at,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at,omitempty"`
	CompletedAt     time.Time  `json:"completed_at,omitempty"`
	BlockedAt       time.Time  `json:"blocked_at,omitempty"`
	PausedAt        time.Time  `json:"paused_at,omitempty"`
	LastReason      string     `json:"last_reason,omitempty"`
	LastEvidence    string     `json:"last_evidence,omitempty"`
	UpdatedNotice   bool       `json:"updated_notice,omitempty"` // true when the next goal prompt should use objective_updated.md
}

// NewGoal constructs an active goal with normalized timestamps and budget.
func NewGoal(objective string, tokenBudget int, now time.Time) *Goal {
	if now.IsZero() {
		now = time.Now()
	}
	if tokenBudget < 0 {
		tokenBudget = 0
	}
	return &Goal{
		Objective:   objective,
		Status:      GoalStatusActive,
		TokenBudget: tokenBudget,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// Clone returns a deep-enough copy for callers that should not mutate the
// persisted object in place.
func (g *Goal) Clone() *Goal {
	if g == nil {
		return nil
	}
	clone := *g
	return &clone
}

// Normalize fills defaults and clamps invalid counters.
func (g *Goal) Normalize(now time.Time) {
	if g == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if g.Status == "" {
		g.Status = GoalStatusActive
	}
	if g.TokenBudget < 0 {
		g.TokenBudget = 0
	}
	if g.TokensUsed < 0 {
		g.TokensUsed = 0
	}
	if g.TimeUsedSeconds < 0 {
		g.TimeUsedSeconds = 0
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = now
	}
}

// IsActive reports whether the runner should continue pursuing this goal.
func (g *Goal) IsActive() bool {
	return g != nil && g.Status == GoalStatusActive && g.Objective != ""
}

// Exists reports whether the goal has meaningful persisted state.
func (g *Goal) Exists() bool {
	return g != nil && g.Objective != ""
}

// RemainingTokens returns the remaining token budget. A zero budget is unlimited.
func (g *Goal) RemainingTokens() int {
	if g == nil || g.TokenBudget <= 0 {
		return 0
	}
	remaining := g.TokenBudget - g.TokensUsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

// BudgetExhausted reports whether a finite token budget has been consumed.
func (g *Goal) BudgetExhausted() bool {
	return g != nil && g.TokenBudget > 0 && g.TokensUsed >= g.TokenBudget
}
