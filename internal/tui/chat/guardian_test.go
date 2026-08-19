package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestGuardianReviewPersistsSessionMetrics(t *testing.T) {
	store := &mockStore{}
	m := newTestChatModel(true)
	m.store = store
	m.sess = &session.Session{ID: "session-1"}
	usage := llm.Usage{InputTokens: 41, OutputTokens: 9, CachedInputTokens: 30, CacheWriteTokens: 5}

	m.Update(GuardianReviewMsg{Event: tools.GuardianEvent{Model: "guardian-model", Usage: usage, Message: "guardian: denied", Outcome: tools.GuardianDenied}})

	if len(store.metricUpdates) != 1 || store.metricUpdates[0] != (metricUpdate{id: "session-1", input: 41, output: 9, cached: 30, cacheWrite: 5}) {
		t.Fatalf("persisted guardian metrics = %+v", store.metricUpdates)
	}
	if m.sess.InputTokens != 41 || m.sess.OutputTokens != 9 || m.sess.CachedInputTokens != 30 || m.sess.CacheWriteTokens != 5 {
		t.Fatalf("session guardian metrics = %+v", m.sess)
	}
}

func TestSubagentGuardianReviewAccountsUsage(t *testing.T) {
	m := newTestChatModel(true)
	usage := llm.Usage{InputTokens: 17, OutputTokens: 6, CachedInputTokens: 9, CacheWriteTokens: 3}

	m.Update(SubagentProgressMsg{CallID: "spawn-1", Event: tools.SubagentEvent{Type: tools.SubagentEventGuardian, Guardian: &tools.GuardianEvent{Model: "child-guardian", Usage: usage}}})

	calls, _ := m.stats.UsageCalls()
	if m.stats.InputTokens != 17 || m.stats.OutputTokens != 6 || len(calls) != 1 || !calls[0].Guardian || calls[0].Model != "child-guardian" {
		t.Fatalf("subagent guardian usage not accounted: stats=%+v calls=%+v", m.stats, calls)
	}
}

func TestGuardianReviewAccountsUsage(t *testing.T) {
	m := newTestChatModel(true)
	usage := llm.Usage{InputTokens: 41, OutputTokens: 9, CachedInputTokens: 30, CacheWriteTokens: 5}

	m.Update(GuardianReviewMsg{Event: tools.GuardianEvent{Model: "guardian-model", Usage: usage, Message: "guardian: approved", Outcome: tools.GuardianApproved}})

	if m.stats.InputTokens != 41 || m.stats.OutputTokens != 9 || m.stats.CachedInputTokens != 30 || m.stats.CacheWriteTokens != 5 || m.stats.LLMCallCount != 1 {
		t.Fatalf("guardian usage not accounted: %+v", m.stats)
	}
	calls, _ := m.stats.UsageCalls()
	if len(calls) != 1 || !calls[0].Guardian || calls[0].Model != "guardian-model" {
		t.Fatalf("guardian call not retained for pricing: %+v", calls)
	}
}

func TestGuardianReviewAttachesByToolCallIDOutOfOrder(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.tracker.HandleToolStart("one", "shell", "echo one", nil)
	m.tracker.HandleToolStart("two", "shell", "echo two", nil)
	m.tracker.HandleToolEnd("one", true)
	m.tracker.HandleToolEnd("two", false)

	m.Update(GuardianReviewMsg{Event: tools.GuardianEvent{ToolCallID: "two", Message: "guardian: denied: no two", Outcome: tools.GuardianDenied}})
	m.Update(GuardianReviewMsg{Event: tools.GuardianEvent{ToolCallID: "one", Message: "guardian: approved (low risk)", Outcome: tools.GuardianApproved}})

	plain := ui.StripANSI(m.tracker.RenderUnflushed(80, ui.RenderMarkdown, false))
	if !strings.Contains(plain, "shell echo one") || !strings.Contains(plain, "shell echo two") ||
		!strings.Contains(plain, "\n  Guardian: approved") || !strings.Contains(plain, "\n  Guardian: denied: no two") {
		t.Fatalf("guardian decisions not adjacent to matching tools: %q", plain)
	}
	for _, seg := range m.tracker.Segments {
		if seg.Type == ui.SegmentAskUserResult {
			t.Fatalf("guardian was rendered as free-floating segment: %#v", seg)
		}
	}
}

func TestGuardianReviewBeforeToolStartIsBuffered(t *testing.T) {
	m := newTestChatModel(true)
	event := tools.GuardianEvent{ToolCallID: "later", Message: "guardian: approved (reviewed risk)", Outcome: tools.GuardianApproved}
	m.Update(GuardianReviewMsg{Event: event})
	m.tracker.HandleToolStart("later", "shell", "echo later", nil)
	m.tracker.HandleToolEnd("later", true)
	plain := ui.StripANSI(m.tracker.RenderUnflushed(80, ui.RenderMarkdown, false))
	if !strings.Contains(plain, "shell echo later") || !strings.Contains(plain, "\n  Guardian: approved") {
		t.Fatalf("buffered guardian missing from tool: %q", plain)
	}
}

func TestLiveToolTurnsRemainVisibleAndGuardianAttachesToMatchingCall(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = true

	for _, event := range []ui.StreamEvent{
		ui.ToolStartEvent("call-first", "write_file", "(bob)", nil),
		ui.ToolEndEvent("call-first", "write_file", "(bob)", true),
		ui.ToolStartEvent("call-second", "shell", "(Sleep 3 seconds)", nil),
	} {
		updated, _ := m.Update(streamEventMsg{event: event, generation: m.streamGeneration})
		m = updated.(*Model)
	}
	updated, _ := m.Update(GuardianReviewMsg{Event: tools.GuardianEvent{
		ToolCallID: "call-second",
		Message:    "guardian: approved (low risk)",
		Outcome:    tools.GuardianApproved,
	}})
	m = updated.(*Model)
	updated, _ = m.Update(streamEventMsg{
		event:      ui.ToolEndEvent("call-second", "shell", "(Sleep 3 seconds)", true),
		generation: m.streamGeneration,
	})
	m = updated.(*Model)

	if len(m.tracker.Segments) != 2 {
		t.Fatalf("live tool segments = %d, want 2: %#v", len(m.tracker.Segments), m.tracker.Segments)
	}
	if m.tracker.Segments[0].Guardian != nil {
		t.Fatalf("guardian attached to first tool: %#v", m.tracker.Segments[0].Guardian)
	}
	if guardian := m.tracker.Segments[1].Guardian; guardian == nil || guardian.ToolCallID != "call-second" {
		t.Fatalf("guardian on second tool = %#v, want call-second", guardian)
	}
	plain := ui.StripANSI(m.tracker.RenderUnflushed(80, ui.RenderMarkdown, false))
	if !strings.Contains(plain, "write_file (bob)") || !strings.Contains(plain, "shell (Sleep 3 seconds)") ||
		strings.Count(plain, "Guardian: approved") != 1 {
		t.Fatalf("live tool rendering or guardian placement is wrong: %q", plain)
	}
}

func TestUncorrelatedGuardianStatusRemainsDurable(t *testing.T) {
	m := newTestChatModel(true)
	message := "guardian: circuit breaker tripped after repeated denials; auto mode disabled"
	m.Update(GuardianReviewMsg{Event: tools.GuardianEvent{Message: message, Outcome: tools.GuardianWarning}})
	plain := ui.StripANSI(m.tracker.RenderUnflushed(80, ui.RenderMarkdown, false))
	if !strings.Contains(plain, message) {
		t.Fatalf("session-level guardian status was not retained: %q", plain)
	}
}

func TestGuardianFooterToneDenialBeatsApprovedInRationale(t *testing.T) {
	if got := guardianFooterTone("guardian: denied: user never approved this command"); got != "warning" {
		t.Fatalf("tone = %q, want warning", got)
	}
}
