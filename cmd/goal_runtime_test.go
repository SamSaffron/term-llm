package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestRunnerActiveGoalAutoContinuesUntilUpdateGoalComplete(t *testing.T) {
	ctx := context.Background()
	store := newGoalTestStore(t)
	goal := session.NewGoal("finish the integration", 0, time.Now())
	createGoalTestSession(t, store, "sess-goal-loop", goal)

	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddTurn(llm.MockTurn{Text: "working", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5}})
	provider.AddToolCall("goal-1", tools.UpdateGoalToolName, map[string]any{
		"status":   "complete",
		"reason":   "all requirements satisfied",
		"evidence": "tests passed",
	})
	provider.AddTurn(llm.MockTurn{Text: "complete", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}})

	runner := newCmdRunner(goalTestConfig(), cmdRunnerOptions{Store: store}).(*cmdRunner)
	_, err := runner.Run(ctx, runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		SessionID:        "sess-goal-loop",
		Messages:         []llm.Message{llm.UserText("please continue")},
		ProviderInstance: provider,
		Persist:          true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	got, err := store.Get(ctx, "sess-goal-loop")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Goal == nil || got.Goal.Status != session.GoalStatusComplete {
		t.Fatalf("goal status = %+v, want complete", got.Goal)
	}
	if got.Goal.TokensUsed <= 0 {
		t.Fatalf("goal tokens used = %d, want > 0", got.Goal.TokensUsed)
	}
	if provider.CurrentTurn() != 3 {
		t.Fatalf("provider turns = %d, want 3 (first pass, update_goal call, final after tool)", provider.CurrentTurn())
	}
	if len(provider.Requests) < 2 {
		t.Fatalf("provider requests = %d, want at least 2", len(provider.Requests))
	}
	firstPrompt := llm.MessageText(provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1])
	if !strings.Contains(firstPrompt, "Completion audit") || !strings.Contains(firstPrompt, "finish the integration") {
		t.Fatalf("first goal prompt missing continuation steering: %q", firstPrompt)
	}
}

func TestRunnerActiveGoalBudgetExhaustionPausesAfterWrapup(t *testing.T) {
	ctx := context.Background()
	store := newGoalTestStore(t)
	goal := session.NewGoal("use a tiny budget", 10, time.Now())
	createGoalTestSession(t, store, "sess-goal-budget", goal)

	provider := llm.NewMockProvider("mock")
	provider.AddTurn(llm.MockTurn{Text: "spent", Usage: llm.Usage{InputTokens: 8, OutputTokens: 5}})
	provider.AddTurn(llm.MockTurn{Text: "wrap up", Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}})

	runner := newCmdRunner(goalTestConfig(), cmdRunnerOptions{Store: store}).(*cmdRunner)
	_, err := runner.Run(ctx, runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		SessionID:        "sess-goal-budget",
		Messages:         []llm.Message{llm.UserText("start")},
		ProviderInstance: provider,
		Persist:          true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got, err := store.Get(ctx, "sess-goal-budget")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Goal == nil || got.Goal.Status != session.GoalStatusBudgetLimited {
		t.Fatalf("goal status = %+v, want budget_limited", got.Goal)
	}
	if provider.CurrentTurn() != 2 {
		t.Fatalf("provider turns = %d, want budget wrap-up pass only", provider.CurrentTurn())
	}
	lastReq := provider.Requests[len(provider.Requests)-1]
	lastPrompt := llm.MessageText(lastReq.Messages[len(lastReq.Messages)-1])
	if !strings.Contains(lastPrompt, "reached its token budget") {
		t.Fatalf("budget prompt missing from final request: %q", lastPrompt)
	}
}

func TestRunnerActiveGoalCancelPausesGoal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := newGoalTestStore(t)
	goal := session.NewGoal("cancel me", 0, time.Now())
	createGoalTestSession(t, store, "sess-goal-cancel", goal)

	provider := llm.NewMockProvider("mock")
	provider.AddTurn(llm.MockTurn{Text: "never", Delay: 200 * time.Millisecond})

	runner := newCmdRunner(goalTestConfig(), cmdRunnerOptions{Store: store}).(*cmdRunner)
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, runpkg.Request{
			Platform:         runpkg.PlatformConsole,
			SessionID:        "sess-goal-cancel",
			Messages:         []llm.Message{llm.UserText("go")},
			ProviderInstance: provider,
			Persist:          true,
		}, eventSinkFunc(nil))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	got, err := store.Get(context.Background(), "sess-goal-cancel")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Goal == nil || got.Goal.Status != session.GoalStatusPaused {
		t.Fatalf("goal status = %+v, want paused", got.Goal)
	}
}

func TestRunnerActiveGoalHonorsExternalPauseBetweenPasses(t *testing.T) {
	ctx := context.Background()
	store := newGoalTestStore(t)
	goal := session.NewGoal("pause externally", 0, time.Now())
	createGoalTestSession(t, store, "sess-goal-external-pause", goal)

	provider := llm.NewMockProvider("mock")
	provider.AddTurn(llm.MockTurn{Text: "working", Usage: llm.Usage{InputTokens: 3, OutputTokens: 2}})
	paused := false
	sink := eventSinkFunc(func(ev llm.Event) {
		if paused || ev.Type != llm.EventUsage {
			return
		}
		paused = true
		sess, err := store.Get(ctx, "sess-goal-external-pause")
		if err != nil || sess == nil || sess.Goal == nil {
			t.Fatalf("load goal during event: sess=%+v err=%v", sess, err)
		}
		updated := sess.Goal.Clone()
		updated.Status = session.GoalStatusPaused
		updated.PausedAt = time.Now()
		updated.UpdatedAt = updated.PausedAt
		updated.LastReason = "paused from another client"
		if err := session.UpdateGoal(ctx, store, "sess-goal-external-pause", updated); err != nil {
			t.Fatalf("pause goal during event: %v", err)
		}
	})

	runner := newCmdRunner(goalTestConfig(), cmdRunnerOptions{Store: store}).(*cmdRunner)
	_, err := runner.Run(ctx, runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		SessionID:        "sess-goal-external-pause",
		Messages:         []llm.Message{llm.UserText("go")},
		ProviderInstance: provider,
		Persist:          true,
	}, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got, err := store.Get(ctx, "sess-goal-external-pause")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Goal == nil || got.Goal.Status != session.GoalStatusPaused || got.Goal.LastReason != "paused from another client" {
		t.Fatalf("goal after external pause = %+v, want paused with external reason", got.Goal)
	}
	if got.Goal.TokensUsed <= 0 {
		t.Fatalf("goal tokens used = %d, want current pass accounted", got.Goal.TokensUsed)
	}
	if provider.CurrentTurn() != 1 {
		t.Fatalf("provider turns = %d, want no automatic continuation after external pause", provider.CurrentTurn())
	}
}

func TestRunnerActiveGoalHonorsExternalClearBetweenPasses(t *testing.T) {
	ctx := context.Background()
	store := newGoalTestStore(t)
	goal := session.NewGoal("clear externally", 0, time.Now())
	createGoalTestSession(t, store, "sess-goal-external-clear", goal)

	provider := llm.NewMockProvider("mock")
	provider.AddTurn(llm.MockTurn{Text: "working", Usage: llm.Usage{InputTokens: 3, OutputTokens: 2}})
	cleared := false
	sink := eventSinkFunc(func(ev llm.Event) {
		if cleared || ev.Type != llm.EventUsage {
			return
		}
		cleared = true
		if err := session.UpdateGoal(ctx, store, "sess-goal-external-clear", nil); err != nil {
			t.Fatalf("clear goal during event: %v", err)
		}
	})

	runner := newCmdRunner(goalTestConfig(), cmdRunnerOptions{Store: store}).(*cmdRunner)
	_, err := runner.Run(ctx, runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		SessionID:        "sess-goal-external-clear",
		Messages:         []llm.Message{llm.UserText("go")},
		ProviderInstance: provider,
		Persist:          true,
	}, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got, err := store.Get(ctx, "sess-goal-external-clear")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Goal != nil {
		t.Fatalf("goal after external clear = %+v, want nil", got.Goal)
	}
	if provider.CurrentTurn() != 1 {
		t.Fatalf("provider turns = %d, want no automatic continuation after external clear", provider.CurrentTurn())
	}
}

func TestRunnerActiveGoalHonorsExternalEditBetweenPasses(t *testing.T) {
	ctx := context.Background()
	store := newGoalTestStore(t)
	goal := session.NewGoal("old objective", 0, time.Now())
	createGoalTestSession(t, store, "sess-goal-external-edit", goal)

	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddTurn(llm.MockTurn{Text: "old work", Usage: llm.Usage{InputTokens: 3, OutputTokens: 2}})
	provider.AddToolCall("goal-edit-complete", tools.UpdateGoalToolName, map[string]any{
		"status": "complete",
		"reason": "revised goal satisfied",
	})
	provider.AddTurn(llm.MockTurn{Text: "complete", Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}})
	edited := false
	sink := eventSinkFunc(func(ev llm.Event) {
		if edited || ev.Type != llm.EventUsage {
			return
		}
		edited = true
		sess, err := store.Get(ctx, "sess-goal-external-edit")
		if err != nil || sess == nil || sess.Goal == nil {
			t.Fatalf("load goal during event: sess=%+v err=%v", sess, err)
		}
		updated := sess.Goal.Clone()
		updated.Objective = "revised objective"
		updated.UpdatedNotice = true
		updated.UpdatedAt = time.Now()
		if err := session.UpdateGoal(ctx, store, "sess-goal-external-edit", updated); err != nil {
			t.Fatalf("edit goal during event: %v", err)
		}
	})

	runner := newCmdRunner(goalTestConfig(), cmdRunnerOptions{Store: store}).(*cmdRunner)
	_, err := runner.Run(ctx, runpkg.Request{
		Platform:         runpkg.PlatformConsole,
		SessionID:        "sess-goal-external-edit",
		Messages:         []llm.Message{llm.UserText("go")},
		ProviderInstance: provider,
		Persist:          true,
	}, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	got, err := store.Get(ctx, "sess-goal-external-edit")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Goal == nil || got.Goal.Objective != "revised objective" || got.Goal.Status != session.GoalStatusComplete {
		t.Fatalf("goal after external edit = %+v, want revised complete goal", got.Goal)
	}
	if len(provider.Requests) < 2 {
		t.Fatalf("provider requests = %d, want edit continuation", len(provider.Requests))
	}
	secondPrompt := llm.MessageText(provider.Requests[1].Messages[len(provider.Requests[1].Messages)-1])
	if !strings.Contains(secondPrompt, "objective was edited") || !strings.Contains(secondPrompt, "revised objective") {
		t.Fatalf("second goal prompt = %q, want objective-updated prompt for revised objective", secondPrompt)
	}
}

func TestRunnerSyntheticGoalCallbackOnlyWhenRuntimePersistenceDisabled(t *testing.T) {
	runCase := func(t *testing.T, disableRuntimePersistence bool) int {
		t.Helper()
		ctx := context.Background()
		store := newGoalTestStore(t)
		goal := session.NewGoal("record synthetic prompt", 0, time.Now())
		createGoalTestSession(t, store, "sess-goal-synthetic", goal)
		provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
		provider.AddToolCall("goal-synthetic-complete", tools.UpdateGoalToolName, map[string]any{
			"status": "complete",
			"reason": "done",
		})
		provider.AddTurn(llm.MockTurn{Text: "done", Usage: llm.Usage{InputTokens: 1, OutputTokens: 1}})
		calls := 0
		runner := newCmdRunner(goalTestConfig(), cmdRunnerOptions{Store: store}).(*cmdRunner)
		_, err := runner.Run(ctx, runpkg.Request{
			Platform:                  runpkg.PlatformConsole,
			SessionID:                 "sess-goal-synthetic",
			Messages:                  []llm.Message{llm.UserText("go")},
			ProviderInstance:          provider,
			Persist:                   true,
			DisableRuntimePersistence: disableRuntimePersistence,
			OnSyntheticUserMessage: func(context.Context, llm.Message) error {
				calls++
				return nil
			},
		}, eventSinkFunc(nil))
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		return calls
	}

	if calls := runCase(t, false); calls != 0 {
		t.Fatalf("synthetic callback calls with runtime persistence = %d, want 0", calls)
	}
	if calls := runCase(t, true); calls != 1 {
		t.Fatalf("synthetic callback calls with runtime persistence disabled = %d, want 1", calls)
	}
}

func newGoalTestStore(t *testing.T) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createGoalTestSession(t *testing.T, store session.Store, id string, goal *session.Goal) {
	t.Helper()
	if err := store.Create(context.Background(), &session.Session{
		ID:        id,
		Provider:  "mock",
		Model:     "mock-model",
		Mode:      session.ModeChat,
		Origin:    session.OriginTUI,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Status:    session.StatusActive,
		Goal:      goal,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
}

func goalTestConfig() *config.Config {
	return &config.Config{
		DefaultProvider: "mock",
		Providers: map[string]config.ProviderConfig{
			"mock": {Model: "mock-model"},
		},
	}
}
