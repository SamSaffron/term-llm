package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestUpdateGoalStatusRejectsBudgetLimitedResumeWithoutBudget(t *testing.T) {
	now := time.Now()
	goal := session.NewGoal("stay within budget", 10, now)
	goal.TokensUsed = 10
	goal.Status = session.GoalStatusBudgetLimited
	goal.PausedAt = now
	sess := &session.Session{ID: "goal-budget-resume", Goal: goal}
	store := &mockStore{sessions: map[string]*session.Session{sess.ID: sess}}
	m := newCmdTestModel(store)
	m.sess = sess

	result, _ := m.updateGoalStatus(session.GoalStatusActive, "goal resumed")
	m = result.(*Model)
	if m.sess.Goal.Status != session.GoalStatusBudgetLimited {
		t.Fatalf("goal status = %s, want budget_limited", m.sess.Goal.Status)
	}
	if store.updated != nil {
		t.Fatalf("store updated exhausted budget goal: %+v", store.updated.Goal)
	}
	if !strings.Contains(m.footerMessage, "budget is exhausted") {
		t.Fatalf("footerMessage = %q, want budget exhaustion error", m.footerMessage)
	}
}

func TestParseGoalObjectiveAndBudget(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantObjective string
		wantBudget    int
	}{
		{name: "plain", raw: "finish the work", wantObjective: "finish the work", wantBudget: -1},
		{name: "tokens flag", raw: "finish --tokens 42", wantObjective: "finish", wantBudget: 42},
		{name: "budget equals", raw: "finish docs --budget=123", wantObjective: "finish docs", wantBudget: 123},
		{name: "token budget equals", raw: "--token-budget=7 finish", wantObjective: "finish", wantBudget: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objective, budget, err := parseGoalObjectiveAndBudget(tt.raw)
			if err != nil {
				t.Fatalf("parseGoalObjectiveAndBudget() error = %v", err)
			}
			if objective != tt.wantObjective || budget != tt.wantBudget {
				t.Fatalf("parseGoalObjectiveAndBudget() = (%q, %d), want (%q, %d)", objective, budget, tt.wantObjective, tt.wantBudget)
			}
		})
	}
}
