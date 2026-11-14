package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	runpkg "github.com/term-llm/term-llm/internal/run"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/skills"
	"github.com/term-llm/term-llm/internal/tools"
)

type fakeSkillChildRunner struct {
	request runpkg.ChildRunRequest
	result  runpkg.ChildRunResult
	err     error
}

func (r *fakeSkillChildRunner) RunChild(ctx context.Context, request runpkg.ChildRunRequest, callback runpkg.ChildRunEventCallback) (runpkg.ChildRunResult, error) {
	r.request = request
	if callback != nil {
		callback(request.RunID, tools.SubagentEvent{Type: tools.SubagentEventInit, Provider: "mock", Model: "review-model"})
		callback(request.RunID, tools.SubagentEvent{Type: tools.SubagentEventText, Text: "reviewing..."})
		callback(request.RunID, tools.SubagentEvent{Type: tools.SubagentEventDone})
	}
	result := r.result
	result.RunID = request.RunID
	if result.ChildSessionID == "" {
		result.ChildSessionID = "child-session"
	}
	if result.Output == "" {
		result.Output = "No findings."
	}
	if result.StartedAt.IsZero() {
		result.StartedAt = time.Now().Add(-time.Second)
	}
	if result.CompletedAt.IsZero() {
		result.CompletedAt = time.Now()
	}
	return result, r.err
}

func TestActiveIsolatedSkillRunStaysOutOfComposerFooter(t *testing.T) {
	m := newTestChatModel(false)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.skillRuns = map[string]*skillRunState{
		"skill-1": {
			ID:     "skill-1",
			Name:   "commit-message",
			Agent:  "commit-message",
			Status: "running",
			Phase:  "Thinking",
		},
	}

	footer := m.buildFooterLayout().view
	if strings.Contains(footer, "commit-message") || strings.Contains(footer, "skill-1") {
		t.Fatalf("running isolated skill leaked into composer footer: %q", footer)
	}
}

func TestIsolatedSkillUsesSpawnAgentProgressUI(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"review": `---
name: review
description: Review changes
context: fork
agent: reviewer
---
Review.
`,
	})
	m := newTestChatModel(false)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.SetChildRunner(&fakeSkillChildRunner{})

	updated, cmd := m.ExecuteCommand("/review")
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("isolated skill returned no run command")
	}
	view := m.View().Content
	for _, want := range []string{"@reviewer", "starting", "0s"} {
		if !strings.Contains(view, want) {
			t.Fatalf("isolated skill does not use spawn_agent progress UI; missing %q in %q", want, view)
		}
	}
	if footer := m.buildFooterLayout().view; strings.Contains(footer, "@reviewer") {
		t.Fatalf("isolated skill progress leaked into composer footer: %q", footer)
	}
}

func TestIsolatedSkillRunsDirectlyWithoutParentStream(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"review": `---
name: review
description: Review changes
context: fork
agent: reviewer
model: fast
allowed-tools: read_file grep
---
Review $ARGUMENTS.
`,
	})
	runner := &fakeSkillChildRunner{}
	m := newTestChatModel(false)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.SetChildRunner(runner)

	updated, cmd := m.ExecuteCommand("/review internal/config")
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("isolated skill returned no run command")
	}
	if m.streaming {
		t.Fatal("isolated skill incorrectly started the parent model stream")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) < 2 {
		t.Fatalf("isolated skill command = %T, want batch", cmd())
	}
	done, ok := batch[0]().(skillRunDoneMsg)
	if !ok {
		t.Fatalf("first isolated command = %T, want skillRunDoneMsg", batch[0]())
	}
	updated, _ = m.Update(done)
	m = updated.(*Model)

	if runner.request.Kind != runpkg.ChildRunIsolatedSkill || runner.request.AgentName != "reviewer" || runner.request.ModelOverride != "fast" {
		t.Fatalf("child request routing = %#v", runner.request)
	}
	if !strings.Contains(runner.request.Prompt, "Review internal/config.") || !strings.Contains(runner.request.Prompt, "# Skill: review") || !strings.Contains(runner.request.Prompt, "**Description:** Review changes") {
		t.Fatalf("child prompt = %q", runner.request.Prompt)
	}
	if runner.request.ParentSessionID != m.sess.ID || runner.request.BaseDir != m.effectiveWorkingDir() {
		t.Fatalf("child parent/cwd = %q/%q", runner.request.ParentSessionID, runner.request.BaseDir)
	}
	if runner.request.Skill == nil || runner.request.Skill.Name != "review" || !runner.request.Skill.AllowedToolsPresent || len(runner.request.Skill.AllowedTools) != 2 {
		t.Fatalf("child skill metadata = %#v", runner.request.Skill)
	}

	state := m.skillRuns[done.RunID]
	if state == nil || state.Status != "complete" || state.ChildSessionID != "child-session" {
		t.Fatalf("skill run state = %#v", state)
	}
	var invocationEvent, resultEvent, resultContext bool
	for _, message := range m.messages {
		if message.Role == "event" && strings.Contains(message.TextContent, "Skill invocation") {
			invocationEvent = true
		}
		if message.Role == "event" && strings.Contains(message.TextContent, "No findings") {
			resultEvent = true
		}
		if message.Role == "developer" && strings.Contains(message.TextContent, "<isolated_skill_result") && strings.Contains(message.TextContent, "No findings") {
			resultContext = true
		}
	}
	if !invocationEvent || !resultEvent || !resultContext {
		t.Fatalf("persisted invocation/result/context = %v/%v/%v; messages %#v", invocationEvent, resultEvent, resultContext, m.messages)
	}
}

func TestIsolatedSkillCompletionQueuesAtParentTurnBoundary(t *testing.T) {
	registry := chatTestSkillRegistry(t, map[string]string{
		"review": `---
name: review
description: Review changes
context: fork
---
Review.
`,
	})
	runner := &fakeSkillChildRunner{}
	m := newTestChatModel(false)
	m.SetSkillsSetup(&skills.Setup{Registry: registry})
	m.SetChildRunner(runner)
	m.streaming = true
	before := len(m.messages)

	updated, cmd := m.ExecuteCommand("/review")
	m = updated.(*Model)
	if !m.streaming {
		t.Fatal("starting isolated skill interrupted parent stream")
	}
	batch := cmd().(tea.BatchMsg)
	done := batch[0]().(skillRunDoneMsg)
	m.handleSkillRunDone(done)
	if len(m.messages) != before {
		t.Fatalf("child result inserted during parent message: before=%d after=%d", before, len(m.messages))
	}
	if len(m.pendingSkillResults) != 1 {
		t.Fatalf("pendingSkillResults = %d, want 1", len(m.pendingSkillResults))
	}

	m.streaming = false
	m.flushPendingSkillResults()
	if len(m.messages) <= before || len(m.pendingSkillResults) != 0 {
		t.Fatalf("queued child result not inserted at boundary: messages=%d pending=%d", len(m.messages), len(m.pendingSkillResults))
	}
	if got := m.messages[before].Role; got != "event" {
		t.Fatalf("first boundary insertion role = %q, want invocation event", got)
	}
}

func TestCancelIsolatedSkillRunDoesNotCancelParent(t *testing.T) {
	parentCancelled := false
	m := newTestChatModel(false)
	m.streaming = true
	m.streamCancelFunc = func() { parentCancelled = true }
	childCancelled := false
	m.skillRuns = map[string]*skillRunState{
		"skill-1": {ID: "skill-1", Name: "review", Status: "running", Cancel: func() { childCancelled = true }},
	}

	if err := m.cancelSkillRun("skill-1"); err != nil {
		t.Fatalf("cancelSkillRun() error = %v", err)
	}
	if !childCancelled || parentCancelled {
		t.Fatalf("cancellation targets: child=%v parent=%v", childCancelled, parentCancelled)
	}
	if m.skillRuns["skill-1"].Status != "cancelling" {
		t.Fatalf("child status = %q", m.skillRuns["skill-1"].Status)
	}
}

func TestSkillRunProgressListenersAreRunScopedAndStop(t *testing.T) {
	m := newTestChatModel(false)
	eventsA := make(chan tools.SubagentEvent, 1)
	doneA := make(chan struct{})
	eventsB := make(chan tools.SubagentEvent, 1)
	doneB := make(chan struct{})
	eventsB <- tools.SubagentEvent{Type: tools.SubagentEventText, Text: "run b"}

	message, ok := m.listenForSkillRunProgress("skill-b", eventsB, doneB)().(skillRunProgressMsg)
	if !ok || message.RunID != "skill-b" || message.Event.Text != "run b" || message.Closed {
		t.Fatalf("run-scoped progress = %#v", message)
	}
	close(doneA)
	closed, ok := m.listenForSkillRunProgress("skill-a", eventsA, doneA)().(skillRunProgressMsg)
	if !ok || closed.RunID != "skill-a" || !closed.Closed {
		t.Fatalf("closed progress listener = %#v", closed)
	}
}

func TestCtrlCCancelsActiveIsolatedSkillBeforeArmingExit(t *testing.T) {
	m := newTestChatModel(false)
	cancelled := false
	m.skillRuns = map[string]*skillRunState{
		"skill-1": {ID: "skill-1", Name: "review", Status: "running", Cancel: func() { cancelled = true }},
	}

	updated, cmd := m.handleCtrlC()
	m = updated.(*Model)
	if !cancelled || m.skillRuns["skill-1"].Status != "cancelling" {
		t.Fatalf("isolated skill cancellation = %v state=%#v", cancelled, m.skillRuns["skill-1"])
	}
	if !m.ctrlCExitArmedUntil.IsZero() || m.quitting {
		t.Fatalf("Ctrl-C armed exit or quit while cancelling skill: armed=%v quitting=%v", m.ctrlCExitArmedUntil, m.quitting)
	}
	if cmd == nil {
		t.Fatal("Ctrl-C cancellation returned no feedback command")
	}
}

func TestCtrlCCanForceQuitCancellingIsolatedSkill(t *testing.T) {
	m := newTestChatModel(false)
	m.skillRuns = map[string]*skillRunState{
		"skill-1": {ID: "skill-1", Name: "review", Status: "cancelling"},
	}

	updated, _ := m.handleCtrlC()
	m = updated.(*Model)
	if m.ctrlCExitArmedUntil.IsZero() || m.quitting {
		t.Fatalf("first Ctrl-C on cancelling skill did not arm exit: armed=%v quitting=%v", m.ctrlCExitArmedUntil, m.quitting)
	}
	updated, _ = m.handleCtrlC()
	m = updated.(*Model)
	if !m.quitting {
		t.Fatal("second Ctrl-C could not force quit a stuck cancelling skill")
	}
}

var _ runpkg.ChildRunner = (*fakeSkillChildRunner)(nil)
var _ session.Store = (*mockStore)(nil)
