package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

type skillRunState struct {
	ID                  string
	Name                string
	Agent               string
	Status              string
	Phase               string
	Preview             strings.Builder
	Output              string
	ChildSessionID      string
	StartedAt           time.Time
	CompletedAt         time.Time
	Cancel              context.CancelFunc
	TrackerCallID       string
	progressEvents      chan tools.SubagentEvent
	progressDone        chan struct{}
	Activation          *skills.Activation
	DisplayInvocation   string
	InvocationPersisted bool
}

type skillRunProgressMsg struct {
	RunID  string
	Event  tools.SubagentEvent
	Closed bool
}

type skillRunDoneMsg struct {
	RunID  string
	Result runpkg.ChildRunResult
	Err    error
}

func (m *Model) startIsolatedSkillActivation(activation *skills.Activation, displayInvocation string) (tea.Model, tea.Cmd) {
	if m.childRunner == nil {
		return m.showFooterError(fmt.Sprintf("isolated skill %q cannot start: child runner is not configured", activation.Skill.Name))
	}
	if m.activeSkillRunCount() >= 1 {
		return m.showFooterWarning("Another isolated skill run is already active. Use /skills active or /skills cancel <run-id>.")
	}
	if m.skillRuns == nil {
		m.skillRuns = make(map[string]*skillRunState)
	}

	m.skillRunSeq++
	runID := fmt.Sprintf("skill-%d", m.skillRunSeq)
	trackerCallID := "isolated-" + runID
	ctx, cancel := context.WithCancel(m.rootContext())
	progressEvents := make(chan tools.SubagentEvent, 256)
	progressDone := make(chan struct{})
	startedAt := time.Now()
	childPrompt := skills.RenderActivationInstructions(activation)
	state := &skillRunState{
		ID:                runID,
		Name:              activation.Skill.Name,
		Agent:             activation.Metadata.Agent,
		Status:            "running",
		Phase:             "Starting",
		StartedAt:         startedAt,
		Cancel:            cancel,
		TrackerCallID:     trackerCallID,
		progressEvents:    progressEvents,
		progressDone:      progressDone,
		Activation:        activation,
		DisplayInvocation: displayInvocation,
	}
	m.skillRuns[runID] = state
	if !m.streaming {
		// A standalone isolated run owns the transient tool stream. Drop the
		// retained tracker from the previous parent turn so the child renders as a
		// single spawn_agent-style segment without replaying stale tools.
		m.resetTracker()
		if m.viewCache.completedStream != "" {
			m.viewCache.completedStream = ""
			m.invalidateHistoryCache()
		}
	}
	if m.tracker == nil {
		m.resetTracker()
	}
	trackerArgs, _ := json.Marshal(map[string]string{"agent_name": state.Agent, "prompt": childPrompt})
	m.tracker.HandleToolStart(trackerCallID, tools.SpawnAgentToolName, state.Agent, trackerArgs)
	ui.HandleSubagentProgress(m.tracker, m.subagentTracker, trackerCallID, tools.SubagentEvent{Type: tools.SubagentEventInit})
	waveCmd := m.tracker.StartWave()
	if !m.streaming {
		m.persistSkillRunInvocation(state)
	}
	m.setTextareaValue("")
	m.completions.Hide()
	m.clearFooterMessage()

	request := runpkg.ChildRunRequest{
		Kind:            runpkg.ChildRunIsolatedSkill,
		RunID:           runID,
		AgentName:       activation.Metadata.Agent,
		Prompt:          childPrompt,
		ModelOverride:   activation.Metadata.Model,
		ParentSessionID: m.currentSessionID(),
		BaseDir:         m.effectiveWorkingDir(),
		Skill: &runpkg.SkillRunMetadata{
			Name:                activation.Skill.Name,
			Source:              activation.Skill.Source.SourceName(),
			SourcePath:          activation.BaseDir,
			RawArguments:        activation.RawArgs,
			AllowedTools:        append([]string(nil), activation.AllowedTools...),
			AllowedToolsPresent: activation.AllowedToolsPresent,
			ToolDefs:            append([]skills.SkillToolDef(nil), activation.ToolDefs...),
			Resources:           append([]string(nil), activation.Resources...),
		},
	}
	callback := func(_ string, event tools.SubagentEvent) {
		select {
		case progressEvents <- event:
		case <-ctx.Done():
		default:
			// Text progress is best-effort; final output and status are delivered by
			// the terminal result even if the UI event buffer is saturated.
		}
	}
	runCmd := func() tea.Msg {
		defer close(progressDone)
		defer cancel()
		result, err := m.childRunner.RunChild(ctx, request, callback)
		return skillRunDoneMsg{RunID: runID, Result: result, Err: err}
	}
	listenCmd := m.listenForSkillRunProgress(runID, progressEvents, progressDone)
	commands := []tea.Cmd{runCmd, listenCmd, waveCmd}
	if !m.altScreen {
		commands = append(commands, tea.Println("❯ "+displayInvocation+"\n  ↳ isolated skill run "+runID+" · @"+activation.Metadata.Agent))
	}
	return m, tea.Batch(commands...)
}

func (m *Model) currentSessionID() string {
	if m == nil || m.sess == nil {
		return ""
	}
	return m.sess.ID
}

func (m *Model) activeSkillRunCount() int {
	count := 0
	for _, state := range m.skillRuns {
		if state != nil && (state.Status == "running" || state.Status == "cancelling") {
			count++
		}
	}
	return count
}

func (m *Model) listenForSkillRunProgress(runID string, events <-chan tools.SubagentEvent, done <-chan struct{}) tea.Cmd {
	if m == nil || events == nil || done == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case event := <-events:
			return skillRunProgressMsg{RunID: runID, Event: event}
		case <-done:
			return skillRunProgressMsg{RunID: runID, Closed: true}
		}
	}
}

func (m *Model) handleSkillRunProgress(message skillRunProgressMsg) tea.Cmd {
	state := m.skillRuns[message.RunID]
	if state == nil || message.Closed || (state.Status != "running" && state.Status != "cancelling") {
		return nil
	}
	event := message.Event
	m.recordChildEventUsage(state.TrackerCallID, event)
	if state.TrackerCallID != "" {
		ui.HandleSubagentProgress(m.tracker, m.subagentTracker, state.TrackerCallID, event)
	}
	switch event.Type {
	case tools.SubagentEventInit:
		state.Phase = "Thinking"
	case tools.SubagentEventText:
		state.Preview.WriteString(event.Text)
		if state.Preview.Len() > 4096 {
			preview := state.Preview.String()
			state.Preview.Reset()
			state.Preview.WriteString(preview[len(preview)-4096:])
		}
	case tools.SubagentEventPhase:
		state.Phase = event.Phase
	case tools.SubagentEventToolStart:
		state.Phase = event.ToolName
	case tools.SubagentEventDone:
		if state.Phase == "" {
			state.Phase = "Finishing"
		}
	}
	if state.Status == "running" || state.Status == "cancelling" {
		return m.listenForSkillRunProgress(state.ID, state.progressEvents, state.progressDone)
	}
	return nil
}

func (m *Model) handleSkillRunDone(message skillRunDoneMsg) tea.Cmd {
	state := m.skillRuns[message.RunID]
	if state == nil {
		return nil
	}
	state.Output = message.Result.Output
	state.ChildSessionID = message.Result.ChildSessionID
	state.CompletedAt = message.Result.CompletedAt
	if state.CompletedAt.IsZero() {
		state.CompletedAt = time.Now()
	}
	state.Cancel = nil
	switch {
	case errors.Is(message.Err, context.Canceled):
		state.Status = "cancelled"
	case message.Err != nil:
		state.Status = "failed"
	default:
		state.Status = "complete"
	}
	if state.TrackerCallID != "" {
		ui.HandleSubagentProgress(m.tracker, m.subagentTracker, state.TrackerCallID, tools.SubagentEvent{Type: tools.SubagentEventDone})
		if m.tracker != nil {
			m.tracker.HandleToolEnd(state.TrackerCallID, message.Err == nil)
		}
		if m.subagentTracker != nil {
			m.subagentTracker.Remove(state.TrackerCallID)
		}
	}
	state.Phase = ""
	if m.streaming && !m.quitAfterSkillRuns {
		m.pendingSkillResults = append(m.pendingSkillResults, message)
	} else {
		m.persistCompletedSkillRun(message)
	}
	m.invalidateHistoryCache()

	if m.quitAfterSkillRuns && m.activeSkillRunCount() == 0 {
		m.flushPendingSkillResults()
		m.quitAfterSkillRuns = false
		if !m.reloadRequested {
			if summary := m.exitStatsSummary(); summary != "" {
				return m.quitCmd(tea.Println(summary))
			}
		}
		return m.quitCmd()
	}

	notice := m.skillRunResultText(state, message.Err)
	if m.altScreen {
		return nil
	}
	return tea.Println(notice)
}

func (m *Model) persistSkillRunInvocation(state *skillRunState) {
	if state == nil || state.InvocationPersisted || state.Activation == nil || m.sess == nil {
		return
	}
	provenance := skillActivationProvenance(state.Activation)
	provenance.RunID = state.ID
	provenance.Status = state.Status
	provenance.StartedAt = state.StartedAt.UTC().Format(time.RFC3339Nano)
	message := &session.Message{
		SessionID: stateSessionID(m),
		Role:      llm.RoleEvent,
		Parts: []llm.Part{
			{Type: llm.PartSkillActivation, SkillActivation: provenance},
			{Type: llm.PartText, Text: state.DisplayInvocation},
		},
		TextContent: "↳ Skill invocation " + state.DisplayInvocation + " · isolated · " + state.ID,
		CreatedAt:   state.StartedAt,
		Sequence:    -1,
	}
	m.appendSkillSessionMessage(message)
	state.InvocationPersisted = true
}

func stateSessionID(m *Model) string {
	if m == nil || m.sess == nil {
		return ""
	}
	return m.sess.ID
}

func (m *Model) persistCompletedSkillRun(done skillRunDoneMsg) {
	state := m.skillRuns[done.RunID]
	if state == nil || m.sess == nil {
		return
	}
	m.persistSkillRunInvocation(state)
	provenance := skillActivationProvenance(state.Activation)
	provenance.RunID = state.ID
	provenance.ChildSessionID = state.ChildSessionID
	provenance.Status = state.Status
	provenance.StartedAt = state.StartedAt.UTC().Format(time.RFC3339Nano)
	provenance.CompletedAt = state.CompletedAt.UTC().Format(time.RFC3339Nano)

	resultText := m.skillRunResultText(state, done.Err)
	visible := &session.Message{
		SessionID: m.sess.ID,
		Role:      llm.RoleEvent,
		Parts: []llm.Part{
			{Type: llm.PartSkillActivation, SkillActivation: provenance},
			{Type: llm.PartText, Text: resultText},
		},
		TextContent: resultText,
		CreatedAt:   state.CompletedAt,
		Sequence:    -1,
	}
	m.appendSkillSessionMessage(visible)

	if strings.TrimSpace(state.Output) != "" {
		contextText := fmt.Sprintf("<isolated_skill_result name=%q run_id=%q child_session_id=%q status=%q>\n%s\n</isolated_skill_result>", state.Name, state.ID, state.ChildSessionID, state.Status, state.Output)
		developer := &session.Message{
			SessionID: m.sess.ID,
			Role:      llm.RoleDeveloper,
			Parts: []llm.Part{
				{Type: llm.PartSkillActivation, SkillActivation: provenance},
				{Type: llm.PartText, Text: contextText},
			},
			TextContent: contextText,
			CreatedAt:   state.CompletedAt,
			Sequence:    -1,
		}
		m.appendSkillSessionMessage(developer)
	}
	state.Activation = nil
}

func (m *Model) appendSkillSessionMessage(message *session.Message) {
	if message == nil || m.sess == nil {
		return
	}
	m.messages = append(m.messages, *message)
	if m.store != nil {
		_ = m.store.AddMessage(context.Background(), m.sess.ID, message)
		m.messages[len(m.messages)-1].ID = message.ID
	}
	m.invalidateHistoryCache()
}

func (m *Model) skillRunResultText(state *skillRunState, runErr error) string {
	if state == nil {
		return ""
	}
	duration := state.CompletedAt.Sub(state.StartedAt).Round(time.Millisecond)
	header := fmt.Sprintf("↳ Skill /%s · @%s · %s · %s · run %s", state.Name, state.Agent, state.Status, duration, state.ID)
	if state.ChildSessionID != "" {
		header += " · child " + state.ChildSessionID
	}
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		header += "\n" + runErr.Error()
	}
	if strings.TrimSpace(state.Output) != "" {
		header += "\n\n" + state.Output
	}
	return header
}

func (m *Model) flushPendingSkillResults() {
	for _, state := range m.skillRuns {
		if state != nil && !state.InvocationPersisted {
			m.persistSkillRunInvocation(state)
		}
	}
	if len(m.pendingSkillResults) == 0 {
		return
	}
	sort.SliceStable(m.pendingSkillResults, func(i, j int) bool {
		return m.pendingSkillResults[i].Result.CompletedAt.Before(m.pendingSkillResults[j].Result.CompletedAt)
	})
	pending := m.pendingSkillResults
	m.pendingSkillResults = nil
	for _, done := range pending {
		m.persistCompletedSkillRun(done)
	}
}

func (m *Model) activeSkillRunsMarkdown() string {
	var states []*skillRunState
	for _, state := range m.skillRuns {
		if state != nil && (state.Status == "running" || state.Status == "cancelling") {
			states = append(states, state)
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].StartedAt.Before(states[j].StartedAt) })
	if len(states) == 0 {
		return "No active isolated skill runs."
	}
	var builder strings.Builder
	builder.WriteString("## Active isolated skill runs\n\n")
	for _, state := range states {
		builder.WriteString(fmt.Sprintf("- `%s` · `/%s` · @%s · %s · %s\n", state.ID, state.Name, state.Agent, state.Status, time.Since(state.StartedAt).Round(time.Second)))
	}
	builder.WriteString("\nUse `/skills cancel <run-id>` to cancel a run.\n")
	return builder.String()
}

func (m *Model) cancelActiveSkillRuns() bool {
	cancelled := false
	for _, state := range m.skillRuns {
		if state == nil || state.Status != "running" {
			continue
		}
		cancelled = true
		state.Status = "cancelling"
		state.Phase = "Stopping"
		if state.Cancel != nil {
			state.Cancel()
		}
	}
	return cancelled
}

func (m *Model) cancelSkillRun(runID string) error {
	state := m.skillRuns[strings.TrimSpace(runID)]
	if state == nil || (state.Status != "running" && state.Status != "cancelling") {
		return fmt.Errorf("active isolated skill run %q not found", runID)
	}
	if state.Status == "cancelling" {
		return nil
	}
	state.Status = "cancelling"
	state.Phase = "Stopping"
	if state.Cancel != nil {
		state.Cancel()
	}
	return nil
}
