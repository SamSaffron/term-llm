package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

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
