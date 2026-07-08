package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/prompt"
	"github.com/samsaffron/term-llm/internal/session"
	toolpkg "github.com/samsaffron/term-llm/internal/tools"
)

const (
	goalMaxAutoPasses  = 25
	goalPersistTimeout = 5 * time.Second
)

type goalRuntimeState struct {
	mu   sync.Mutex
	goal *session.Goal
}

func newGoalRuntimeState(goal *session.Goal) *goalRuntimeState {
	state := &goalRuntimeState{}
	state.Set(goal)
	return state
}

func (s *goalRuntimeState) Clone() *session.Goal {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goal.Clone()
}

func (s *goalRuntimeState) Set(goal *session.Goal) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.goal = goal.Clone()
}

type goalToolCandidate struct {
	create *toolpkg.CreateGoalArgs
	update *toolpkg.UpdateGoalArgs
}

type goalToolCommit struct {
	Name   string
	Create *toolpkg.CreateGoalArgs
	Update *toolpkg.UpdateGoalArgs
}

type goalToolTracker struct {
	pending map[string]goalToolCandidate
}

func newGoalToolTracker() *goalToolTracker {
	return &goalToolTracker{pending: make(map[string]goalToolCandidate)}
}

func (t *goalToolTracker) observeToolCall(callID, name string, args json.RawMessage) {
	if t == nil || strings.TrimSpace(callID) == "" {
		return
	}
	switch strings.TrimSpace(name) {
	case toolpkg.CreateGoalToolName:
		parsed, err := toolpkg.ParseCreateGoalArgs(args)
		if err != nil {
			return
		}
		t.pending[callID] = goalToolCandidate{create: &parsed}
	case toolpkg.UpdateGoalToolName:
		parsed, err := toolpkg.ParseUpdateGoalArgs(args)
		if err != nil {
			return
		}
		t.pending[callID] = goalToolCandidate{update: &parsed}
	}
}

func (t *goalToolTracker) commitToolCall(callID, name string, success bool) *goalToolCommit {
	if t == nil || strings.TrimSpace(callID) == "" {
		return nil
	}
	candidate, ok := t.pending[callID]
	delete(t.pending, callID)
	if !ok || !success {
		return nil
	}
	name = strings.TrimSpace(name)
	switch name {
	case toolpkg.CreateGoalToolName:
		if candidate.create == nil {
			return nil
		}
		return &goalToolCommit{Name: name, Create: candidate.create}
	case toolpkg.UpdateGoalToolName:
		if candidate.update == nil {
			return nil
		}
		return &goalToolCommit{Name: name, Update: candidate.update}
	default:
		return nil
	}
}

func (rt *serveRuntime) runWithGoal(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onStart func(), onEvent func(llm.Event) error) (serveRunResult, error) {
	goalStore := rt.goalStateStore()
	if goalStore == nil || strings.TrimSpace(req.SessionID) == "" {
		return rt.runOnce(ctx, stateful, replaceHistory, inputMessages, req, onStart, onEvent)
	}

	goal := rt.loadGoal(ctx, req.SessionID)
	if goal == nil || !goal.IsActive() {
		return rt.runOnce(ctx, stateful, replaceHistory, inputMessages, req, onStart, onEvent)
	}
	goal.Normalize(time.Now())
	if !rt.goalMu.TryLock() {
		return serveRunResult{}, errServeSessionBusy
	}
	defer rt.goalMu.Unlock()

	return rt.runActiveGoalLoop(ctx, stateful, replaceHistory, inputMessages, req, onStart, onEvent, goal)
}

func (rt *serveRuntime) goalStateStore() session.Store {
	if rt == nil {
		return nil
	}
	if rt.goalStore != nil {
		return rt.goalStore
	}
	return rt.store
}

func (rt *serveRuntime) loadGoal(ctx context.Context, sessionID string) *session.Goal {
	store := rt.goalStateStore()
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	sess, err := store.Get(ctx, sessionID)
	if err != nil || sess == nil || sess.Goal == nil {
		return nil
	}
	return sess.Goal.Clone()
}

func (rt *serveRuntime) runActiveGoalLoop(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onStart func(), onEvent func(llm.Event) error, goal *session.Goal) (serveRunResult, error) {
	if refreshed := rt.loadGoal(context.Background(), req.SessionID); refreshed != nil {
		goal = refreshed
	}
	if goal == nil || !goal.IsActive() {
		return rt.runOnce(ctx, stateful, replaceHistory, inputMessages, req, onStart, onEvent)
	}
	goal.Normalize(time.Now())
	goalState := newGoalRuntimeState(goal)

	updateTool := toolpkg.NewUpdateGoalTool()
	getTool := toolpkg.NewGetGoalTool(func() *session.Goal { return goalState.Clone() })
	createTool := toolpkg.NewCreateGoalTool()
	goalTools := []llm.Tool{updateTool, getTool, createTool}
	for _, tool := range goalTools {
		rt.engine.RegisterTool(tool)
		defer rt.engine.UnregisterTool(tool.Spec().Name)
	}
	goalSpecs := make([]llm.ToolSpec, 0, len(goalTools))
	for _, tool := range goalTools {
		goalSpecs = append(goalSpecs, tool.Spec())
	}

	tracker := newGoalToolTracker()
	currentInput := append([]llm.Message(nil), inputMessages...)
	var aggregate serveRunResult
	autoPasses := 0
	budgetWrapupPending := false

	appendSynthetic := func(kind prompt.GoalPromptKind) error {
		promptGoal := goalState.Clone()
		if promptGoal == nil || !promptGoal.Exists() {
			return nil
		}
		text := prompt.BuildGoalPrompt(goalPromptData(promptGoal), kind)
		msg := llm.UserText(text)
		currentInput = append(currentInput, msg)
		if rt.syntheticUserCB != nil && rt.store == nil {
			// Callers that disable runtime transcript persistence (notably the TUI)
			// own message storage themselves and need the synthetic goal prompt so the
			// visible/persisted history matches what the provider saw. When rt.store is
			// active, runOnce persists currentInput and invoking this hook as well would
			// duplicate the synthetic user message.
			msgCtx, cancel := progressiveSyntheticUserMessageContext(ctx)
			err := rt.syntheticUserCB(msgCtx, msg)
			cancel()
			if err != nil {
				return err
			}
		}
		if kind == prompt.GoalPromptObjectiveUpdated {
			goalState.Set(rt.consumeGoalUpdatedNotice(context.Background(), req.SessionID, promptGoal))
		}
		return nil
	}

	firstKind := prompt.GoalPromptContinuation
	if goal.UpdatedNotice {
		firstKind = prompt.GoalPromptObjectiveUpdated
	}
	if err := appendSynthetic(firstKind); err != nil {
		return aggregate, err
	}

	for {
		runGoal := rt.loadGoal(context.Background(), req.SessionID)
		if runGoal == nil || !runGoal.Exists() {
			goalState.Set(nil)
			rt.syncSessionMetaGoal(nil)
			return aggregate, nil
		}
		runGoal.Normalize(time.Now())
		if budgetWrapupPending && runGoal.Status == session.GoalStatusBudgetLimited && sameGoalRuntimeTarget(goalState.Clone(), runGoal) {
			// This is the single budget wrap-up pass requested by the runner. The
			// persisted state remains budget_limited so other clients see that new
			// substantive work should not start.
		} else {
			budgetWrapupPending = false
			if !runGoal.IsActive() {
				goalState.Set(runGoal)
				rt.syncSessionMetaGoal(runGoal)
				return aggregate, nil
			}
		}
		goal = runGoal
		goalState.Set(goal)

		autoPasses++
		if autoPasses > goalMaxAutoPasses {
			paused := rt.updateCurrentGoal(context.Background(), req.SessionID, goalState.Clone(), false, func(g *session.Goal) {
				g.Status = session.GoalStatusPaused
				g.PausedAt = time.Now()
				g.LastReason = fmt.Sprintf("paused after %d automatic goal continuations", goalMaxAutoPasses)
			})
			goalState.Set(paused)
			return aggregate, nil
		}

		passReq := req
		passReq.Tools = appendGoalToolSpecs(req.Tools, goalSpecs)
		passUsage := llm.Usage{}
		goalCompleted := false
		wrappedOnEvent := func(ev llm.Event) error {
			switch ev.Type {
			case llm.EventToolCall:
				if ev.Tool != nil {
					callID := strings.TrimSpace(ev.Tool.ID)
					if callID == "" {
						callID = strings.TrimSpace(ev.ToolCallID)
					}
					tracker.observeToolCall(callID, strings.TrimSpace(ev.Tool.Name), ev.Tool.Arguments)
				}
			case llm.EventToolExecEnd:
				if commit := tracker.commitToolCall(strings.TrimSpace(ev.ToolCallID), strings.TrimSpace(ev.ToolName), ev.ToolSuccess); commit != nil {
					latest := rt.applyGoalCommit(context.Background(), req.SessionID, goalState.Clone(), commit)
					goalState.Set(latest)
					goal = latest
					if goal != nil && (goal.Status == session.GoalStatusComplete || goal.Status == session.GoalStatusBlocked) {
						goalCompleted = true
					}
				}
			case llm.EventUsage:
				if ev.Use != nil {
					passUsage.Add(*ev.Use)
				}
			}
			if onEvent != nil {
				return onEvent(ev)
			}
			return nil
		}

		passStart := time.Now()
		result, produced, err := rt.runGoalPass(ctx, stateful, replaceHistory, currentInput, passReq, onStart, wrappedOnEvent)
		onStart = nil
		replaceHistory = false
		aggregate.Text.WriteString(result.Text.String())
		aggregate.ToolCalls = append(aggregate.ToolCalls, result.ToolCalls...)
		aggregate.Usage.Add(result.Usage)
		aggregate.SessionUsage = result.SessionUsage
		if passUsage.IsZero() {
			passUsage = result.Usage
		}
		goal = rt.accountGoalUsage(context.Background(), req.SessionID, passUsage, time.Since(passStart))
		goalState.Set(goal)

		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				paused := rt.updateCurrentGoal(context.Background(), req.SessionID, goalState.Clone(), false, func(g *session.Goal) {
					g.Status = session.GoalStatusPaused
					g.PausedAt = time.Now()
					g.LastReason = "paused because the run was cancelled"
				})
				goalState.Set(paused)
			}
			return aggregate, err
		}

		if goal == nil || !goal.Exists() {
			rt.syncSessionMetaGoal(nil)
			return aggregate, nil
		}
		if goalCompleted || goal.Status == session.GoalStatusComplete || goal.Status == session.GoalStatusBlocked {
			rt.syncSessionMetaGoal(goal)
			return aggregate, nil
		}
		if !goal.IsActive() {
			rt.syncSessionMetaGoal(goal)
			return aggregate, nil
		}

		if goal.BudgetExhausted() {
			budgetGoal := rt.updateCurrentGoal(context.Background(), req.SessionID, goalState.Clone(), false, func(g *session.Goal) {
				g.Status = session.GoalStatusBudgetLimited
				g.PausedAt = time.Now()
				g.LastReason = "token budget exhausted"
			})
			if budgetGoal == nil || !budgetGoal.Exists() {
				goalState.Set(budgetGoal)
				rt.syncSessionMetaGoal(budgetGoal)
				return aggregate, nil
			}
			goalChanged := !sameGoalRuntimeTarget(goalState.Clone(), budgetGoal)
			goal = budgetGoal
			goalState.Set(goal)
			if goal.Status != session.GoalStatusBudgetLimited {
				if !goal.IsActive() {
					rt.syncSessionMetaGoal(goal)
					return aggregate, nil
				}
				nextKind := prompt.GoalPromptContinuation
				if goal.UpdatedNotice || goalChanged {
					nextKind = prompt.GoalPromptObjectiveUpdated
				}
				if err := appendSynthetic(nextKind); err != nil {
					return aggregate, err
				}
				continue
			}
			budgetWrapupPending = true
			if err := appendSynthetic(prompt.GoalPromptBudgetLimit); err != nil {
				return aggregate, err
			}
			continue
		}

		if stateful {
			currentInput = nil
		} else {
			currentInput = append(currentInput, produced...)
		}

		nextKind := prompt.GoalPromptContinuation
		latest := rt.loadGoal(context.Background(), req.SessionID)
		if latest == nil || !latest.Exists() {
			goalState.Set(nil)
			rt.syncSessionMetaGoal(nil)
			return aggregate, nil
		}
		latest.Normalize(time.Now())
		if !latest.IsActive() {
			goalState.Set(latest)
			rt.syncSessionMetaGoal(latest)
			return aggregate, nil
		}
		if latest.UpdatedNotice || !sameGoalRuntimeTarget(goal, latest) {
			nextKind = prompt.GoalPromptObjectiveUpdated
		}
		goal = latest
		goalState.Set(goal)
		if err := appendSynthetic(nextKind); err != nil {
			return aggregate, err
		}
		if onEvent != nil {
			if err := onEvent(llm.Event{Type: llm.EventPhase, Text: "Continuing toward active goal..."}); err != nil {
				return aggregate, err
			}
		}
	}
}

func (rt *serveRuntime) runGoalPass(ctx context.Context, stateful bool, replaceHistory bool, inputMessages []llm.Message, req llm.Request, onStart func(), onEvent func(llm.Event) error) (serveRunResult, []llm.Message, error) {
	origResponseCompleted := rt.responseCompletedCB
	origTurnCompleted := rt.turnCompletedCB
	var produced []llm.Message
	var producedMu sync.Mutex
	assistantCaptured := false
	rt.responseCompletedCB = func(cbCtx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
		var err error
		if origResponseCompleted != nil {
			err = origResponseCompleted(cbCtx, turnIndex, assistantMsg, metrics)
		}
		if err == nil {
			producedMu.Lock()
			produced = append(produced, assistantMsg)
			assistantCaptured = true
			producedMu.Unlock()
		}
		return err
	}
	rt.turnCompletedCB = func(cbCtx context.Context, turnIndex int, messages []llm.Message, metrics llm.TurnMetrics) error {
		producedMu.Lock()
		appendStart := 0
		if assistantCaptured && len(messages) > 0 && messages[0].Role == llm.RoleAssistant {
			appendStart = 1
			assistantCaptured = false
		}
		produced = append(produced, messages[appendStart:]...)
		producedMu.Unlock()
		if origTurnCompleted != nil {
			return origTurnCompleted(cbCtx, turnIndex, messages, metrics)
		}
		return nil
	}
	defer func() {
		rt.responseCompletedCB = origResponseCompleted
		rt.turnCompletedCB = origTurnCompleted
	}()
	result, err := rt.runOnce(ctx, stateful, replaceHistory, inputMessages, req, onStart, onEvent)
	producedMu.Lock()
	out := append([]llm.Message(nil), produced...)
	producedMu.Unlock()
	return result, out, err
}

func appendGoalToolSpecs(base []llm.ToolSpec, goalSpecs []llm.ToolSpec) []llm.ToolSpec {
	out := append([]llm.ToolSpec(nil), base...)
	for _, spec := range goalSpecs {
		found := false
		for _, existing := range out {
			if existing.Name == spec.Name {
				found = true
				break
			}
		}
		if !found {
			out = append(out, spec)
		}
	}
	return out
}

func (rt *serveRuntime) applyGoalCommit(ctx context.Context, sessionID string, expected *session.Goal, commit *goalToolCommit) *session.Goal {
	if expected == nil || commit == nil {
		return rt.loadGoal(ctx, sessionID)
	}
	latest := rt.loadGoal(ctx, sessionID)
	if latest == nil || !latest.Exists() {
		return latest
	}
	if !sameGoalRuntimeTarget(expected, latest) {
		return latest
	}
	now := time.Now()
	if commit.Create != nil {
		if !latest.IsActive() {
			return latest
		}
		created := session.NewGoal(strings.TrimSpace(commit.Create.Objective), commit.Create.TokenBudget, now)
		_ = rt.persistGoal(ctx, sessionID, created)
		return created
	}
	if commit.Update == nil {
		return latest
	}
	if latest.Status != session.GoalStatusActive && latest.Status != session.GoalStatusBudgetLimited {
		return latest
	}
	latest.Status = session.GoalStatus(commit.Update.Status)
	latest.LastReason = firstNonEmptyString(commit.Update.Reason, commit.Update.Message)
	latest.LastEvidence = commit.Update.Evidence
	latest.UpdatedNotice = false
	latest.UpdatedAt = now
	switch latest.Status {
	case session.GoalStatusComplete:
		latest.CompletedAt = now
	case session.GoalStatusBlocked:
		latest.BlockedAt = now
	}
	_ = rt.persistGoal(ctx, sessionID, latest)
	return latest
}

func (rt *serveRuntime) accountGoalUsage(ctx context.Context, sessionID string, usage llm.Usage, elapsed time.Duration) *session.Goal {
	latest := rt.loadGoal(ctx, sessionID)
	if latest == nil || !latest.Exists() {
		return latest
	}
	tokens := goalUsageTokens(usage)
	seconds := 0
	if elapsed > 0 {
		seconds = int(elapsed.Round(time.Second).Seconds())
	}
	if tokens == 0 && seconds == 0 {
		return latest
	}
	latest.TokensUsed += tokens
	latest.TimeUsedSeconds += seconds
	latest.UpdatedAt = time.Now()
	_ = rt.persistGoal(ctx, sessionID, latest)
	return latest
}

func (rt *serveRuntime) consumeGoalUpdatedNotice(ctx context.Context, sessionID string, expected *session.Goal) *session.Goal {
	latest := rt.loadGoal(ctx, sessionID)
	if latest == nil || !latest.Exists() {
		return latest
	}
	if expected != nil && !sameGoalRuntimeTarget(expected, latest) {
		return latest
	}
	if !latest.UpdatedNotice {
		return latest
	}
	latest.UpdatedNotice = false
	latest.UpdatedAt = time.Now()
	_ = rt.persistGoal(ctx, sessionID, latest)
	return latest
}

func (rt *serveRuntime) updateCurrentGoal(ctx context.Context, sessionID string, expected *session.Goal, allowBudgetLimited bool, mutate func(*session.Goal)) *session.Goal {
	latest := rt.loadGoal(ctx, sessionID)
	if latest == nil || !latest.Exists() {
		return latest
	}
	if !sameGoalRuntimeTarget(expected, latest) {
		return latest
	}
	if !latest.IsActive() && !(allowBudgetLimited && latest.Status == session.GoalStatusBudgetLimited) {
		return latest
	}
	mutate(latest)
	latest.UpdatedAt = time.Now()
	_ = rt.persistGoal(ctx, sessionID, latest)
	return latest
}

func sameGoalRuntimeTarget(a, b *session.Goal) bool {
	if a == nil || b == nil || !a.Exists() || !b.Exists() {
		return false
	}
	if strings.TrimSpace(a.Objective) != strings.TrimSpace(b.Objective) {
		return false
	}
	if !a.CreatedAt.IsZero() && !b.CreatedAt.IsZero() && !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	return true
}

func goalUsageTokens(usage llm.Usage) int {
	total := usage.InputTokens + usage.CachedInputTokens + usage.CacheWriteTokens + usage.OutputTokens
	if total < 0 {
		return 0
	}
	return total
}

func (rt *serveRuntime) persistGoal(ctx context.Context, sessionID string, goal *session.Goal) error {
	store := rt.goalStateStore()
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), goalPersistTimeout)
	defer cancel()
	if err := session.UpdateGoal(persistCtx, store, sessionID, goal); err != nil {
		log.Printf("[goal] UpdateGoal failed for %s: %v", sessionID, err)
		return err
	}
	rt.syncSessionMetaGoal(goal)
	return nil
}

func (rt *serveRuntime) syncSessionMetaGoal(goal *session.Goal) {
	if rt == nil || !rt.mu.TryLock() {
		return
	}
	defer rt.mu.Unlock()
	if rt.sessionMeta != nil {
		rt.sessionMeta.Goal = goal.Clone()
	}
}

func goalPromptData(goal *session.Goal) prompt.GoalPromptData {
	if goal == nil {
		return prompt.GoalPromptData{}
	}
	return prompt.GoalPromptData{
		Objective:       goal.Objective,
		TokenBudget:     goal.TokenBudget,
		TokensUsed:      goal.TokensUsed,
		TimeUsedSeconds: goal.TimeUsedSeconds,
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
