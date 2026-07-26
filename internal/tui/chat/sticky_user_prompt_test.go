package chat

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/llm"
	renderchat "github.com/samsaffron/term-llm/internal/render/chat"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestRenderStickyUserPromptIsExactlyOneDisplayLine(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 24
	got := m.renderStickyUserPrompt("  a very long prompt\nwith\tmore context and emoji 🙂🙂  ")
	if strings.Contains(got, "\n") {
		t.Fatalf("sticky prompt wrapped: %q", got)
	}
	if width := ansi.StringWidth(got); width != m.width {
		t.Fatalf("sticky prompt width = %d, want %d: %q", width, m.width, got)
	}
	plain := ui.StripANSI(got)
	if !strings.HasPrefix(plain, "❯ a very long") || !strings.Contains(plain, "…") {
		t.Fatalf("sticky prompt was not normalized and truncated: %q", plain)
	}
}

func TestViewAltScreenShowsStickyPromptForLongCurrentTurn(t *testing.T) {
	m := newTestChatModel(true)
	m.messages = []session.Message{
		{ID: 1, Role: llm.RoleUser, TextContent: "the initiating prompt", Parts: []llm.Part{{Type: llm.PartText, Text: "the initiating prompt"}}},
		{ID: 2, Role: llm.RoleAssistant, TextContent: strings.Repeat("answer line\n", 80), Parts: []llm.Part{{Type: llm.PartText, Text: strings.Repeat("answer line\n", 80)}}},
	}

	view := ui.StripANSI(m.viewAltScreen())
	firstLine := strings.Split(view, "\n")[0]
	if !strings.HasPrefix(firstLine, "❯ the initiating prompt") {
		t.Fatalf("first viewport line = %q, want sticky prompt", firstLine)
	}
}

func TestStickyUserPromptAppearsOnlyAfterPromptScrollsOffscreen(t *testing.T) {
	m := newTestChatModel(true)
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 2, EndLine: 5, Preview: "first prompt"},
		{MessageID: 2, StartLine: 20, EndLine: 23, Preview: "second prompt"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 60))

	m.viewport.SetYOffset(4)
	if _, ok := m.stickyUserPromptAnchor(); ok {
		t.Fatal("breadcrumb appeared while part of the prompt remained visible")
	}
	m.viewport.SetYOffset(5)
	anchor, ok := m.stickyUserPromptAnchor()
	if !ok || anchor.MessageID != 1 {
		t.Fatalf("anchor at offset 5 = %#v, %v", anchor, ok)
	}
	m.viewport.SetYOffset(20)
	if anchor, ok := m.stickyUserPromptAnchor(); ok {
		t.Fatalf("breadcrumb %#v remained while the next prompt was visible", anchor)
	}
	m.viewport.SetYOffset(23)
	anchor, ok = m.stickyUserPromptAnchor()
	if !ok || anchor.MessageID != 2 {
		t.Fatalf("anchor at offset 23 = %#v, %v", anchor, ok)
	}
}

func TestStickyUserPromptHoverUsesAllMotionMouseTracking(t *testing.T) {
	m := newTestChatModel(true)
	m.mouseMode = true
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{{MessageID: 1, StartLine: 1, InteractiveEndLine: 2, EndLine: 3}}
	if got := m.newView("").MouseMode; got != tea.MouseModeAllMotion {
		t.Fatalf("mouse mode = %v, want all motion for hover events", got)
	}
}

func TestStickyUserPromptHoverEntersAndLeaves(t *testing.T) {
	m := newTestChatModel(true)
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 8, EndLine: 11, Preview: "hover target"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 80))
	m.viewport.SetYOffset(30)
	normal := m.renderStickyUserPrompt("hover target")

	_, _ = m.Update(tea.MouseMotionMsg{X: 3, Y: 0})
	if !m.stickyUserPromptHovered {
		t.Fatal("sticky prompt did not enter hover state")
	}
	hovered := m.renderStickyUserPrompt("hover target")
	if hovered == normal {
		t.Fatal("hover state did not change sticky prompt styling")
	}

	_, _ = m.Update(tea.MouseMotionMsg{X: 3, Y: 1})
	if m.stickyUserPromptHovered {
		t.Fatal("sticky prompt remained hovered after pointer left its row")
	}
}

func TestUserPromptHoverIgnoresDragMotionAndResize(t *testing.T) {
	m := newTestChatModel(true)
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 8, InteractiveEndLine: 11, EndLine: 13, Preview: "hover target"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 80))
	m.viewport.SetYOffset(8)

	_, _ = m.Update(tea.MouseMotionMsg{X: 2, Y: 0, Button: tea.MouseLeft})
	if m.stickyUserPromptHovered {
		t.Fatal("drag motion activated user prompt hover")
	}

	m.resizeReflowPending = true
	_, _ = m.Update(tea.MouseMotionMsg{X: 2, Y: 0})
	if m.stickyUserPromptHovered {
		t.Fatal("resize frame activated an invisible user prompt hover target")
	}
	before := m.viewport.YOffset()
	if m.handleStickyUserPromptMouseClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 0}) {
		t.Fatal("resize frame consumed an invisible user prompt click")
	}
	if got := m.viewport.YOffset(); got != before {
		t.Fatalf("resize click moved viewport from %d to %d", before, got)
	}
}

func TestStickyUserPromptHoverControlsStayOnOneLineAtNarrowWidths(t *testing.T) {
	for _, width := range []int{4, 8, 12, 20} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			m := newTestChatModel(true)
			m.width = width
			m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
				{MessageID: 1, StartLine: 2, EndLine: 5, Preview: "first"},
				{MessageID: 2, StartLine: 20, EndLine: 23, Preview: "second"},
			}
			m.viewport.SetContent(strings.Repeat("line\n", 60))
			m.viewport.SetYOffset(30)
			m.stickyUserPromptHovered = true

			got := m.renderStickyUserPrompt("a long prompt that must truncate")
			if strings.Contains(got, "\n") {
				t.Fatalf("hovered prompt wrapped: %q", got)
			}
			if gotWidth := ansi.StringWidth(got); gotWidth != width {
				t.Fatalf("hovered prompt width = %d, want %d: %q", gotWidth, width, got)
			}
		})
	}
}

func TestStickyUserPromptHoverShowsContextualControls(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, EndLine: 8, Preview: "one"},
		{MessageID: 2, StartLine: 35, EndLine: 38, Preview: "two"},
		{MessageID: 3, StartLine: 70, EndLine: 73, Preview: "three"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 120))
	m.stickyUserPromptHovered = true

	m.viewport.SetYOffset(50)
	middle := ui.StripANSI(m.renderStickyUserPrompt(strings.Repeat("long prompt words ", 8)))
	if !strings.Contains(middle, " ↑ prev") || !strings.Contains(middle, "↓ next") || strings.Contains(middle, "…↑") || strings.Contains(middle, "↓ end") {
		t.Fatalf("middle-turn controls = %q", middle)
	}

	m.viewport.SetYOffset(90)
	last := ui.StripANSI(m.renderStickyUserPrompt("three"))
	if !strings.Contains(last, "↑ prev") || !strings.Contains(last, "↓ end") || strings.Contains(last, "↓ next") {
		t.Fatalf("last-turn controls = %q", last)
	}

	m.viewport.SetYOffset(8)
	first := ui.StripANSI(m.renderStickyUserPrompt("one"))
	if strings.Contains(first, "↑ prev") || !strings.Contains(first, "↓ next") {
		t.Fatalf("first-turn controls = %q", first)
	}
}

func TestStickyUserPromptHighlightsOnlyHoveredAction(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, EndLine: 8, Preview: "one"},
		{MessageID: 2, StartLine: 35, EndLine: 38, Preview: "two"},
		{MessageID: 3, StartLine: 70, EndLine: 73, Preview: "three"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 120))
	m.viewport.SetYOffset(50)
	controls := m.stickyUserPromptControls(1)

	var renders []string
	for _, x := range []int{1, controls.previousFrom, controls.forwardFrom} {
		_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: 0})
		rendered := m.renderStickyUserPrompt("two")
		if backgrounds := strings.Count(rendered, "48;"); backgrounds != 1 {
			t.Fatalf("x=%d background runs = %d, want 1: %q", x, backgrounds, rendered)
		}
		renders = append(renders, rendered)
	}
	if renders[0] == renders[1] || renders[1] == renders[2] || renders[0] == renders[2] {
		t.Fatal("moving between prompt, previous, and next did not move the action highlight")
	}
}

func TestStickyUserPromptHoverControlsNavigate(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, InteractiveEndLine: 8, EndLine: 10, Preview: "one"},
		{MessageID: 2, StartLine: 35, InteractiveEndLine: 38, EndLine: 40, Preview: "two"},
		{MessageID: 3, StartLine: 70, InteractiveEndLine: 73, EndLine: 75, Preview: "three"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 120))

	clickControl := func(label string) {
		t.Helper()
		surface, ok := m.userPromptSurfaceAt(0, 0)
		if !ok {
			t.Fatalf("no user prompt surface for %q at offset %d", label, m.viewport.YOffset())
		}
		controls := m.stickyUserPromptControls(surface.anchorIndex)
		x := controls.forwardFrom
		if label == "prev" {
			x = controls.previousFrom
		}
		_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: 0})
		_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0})
	}

	m.viewport.SetYOffset(50)
	clickControl("prev")
	if got := m.viewport.YOffset(); got != 5 {
		t.Fatalf("prev offset = %d, want 5", got)
	}

	m.viewport.SetYOffset(50)
	clickControl("next")
	if got := m.viewport.YOffset(); got != 70 {
		t.Fatalf("next offset = %d, want 70", got)
	}
	if anchor, ok := m.stickyUserPromptAnchor(); ok {
		t.Fatalf("sticky anchor %#v obscured the next prompt after navigation", anchor)
	}

	m.viewport.SetYOffset(90)
	clickControl("end")
	if !m.viewport.AtBottom() {
		t.Fatalf("end control offset = %d, want bottom", m.viewport.YOffset())
	}
}

func TestVisibleUserMessageHoverShowsControls(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, InteractiveEndLine: 8, EndLine: 10, Preview: "one"},
		{MessageID: 2, StartLine: 35, InteractiveEndLine: 38, EndLine: 40, Preview: "two"},
		{MessageID: 3, StartLine: 70, InteractiveEndLine: 73, EndLine: 75, Preview: "three"},
	}
	m.viewport.SetContent(strings.Repeat(strings.Repeat("x", 50)+"\n", 120))
	m.viewport.SetYOffset(70)
	_, _ = m.Update(tea.MouseMotionMsg{X: 2, Y: 1})

	rawView := m.applyUserPromptNavigation(m.viewport.View())
	firstRawLine := strings.Split(rawView, "\n")[0]
	firstLine := ui.StripANSI(firstRawLine)
	if !strings.Contains(firstLine, " ↑ prev") || !strings.Contains(firstLine, "↓ end") {
		t.Fatalf("visible user prompt did not show spaced controls: %q", firstLine)
	}
	if strings.Contains(firstRawLine, "48;2;184;187;38") {
		t.Fatalf("hovering user-message text applied the primary background to its first line: %q", firstRawLine)
	}
}

func TestVisibleUserMessageTextClickStillStartsSelection(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, InteractiveEndLine: 8, EndLine: 10, Preview: "one"},
	}
	m.viewport.SetContent(strings.Repeat("user text\n", 40))
	m.viewport.SetYOffset(5)
	_, _ = m.Update(tea.MouseMotionMsg{X: 4, Y: 0})

	_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 4, Y: 0})
	if !m.selection.Dragging {
		t.Fatal("clicking visible user-message text did not start drag selection")
	}
	if got := m.viewport.YOffset(); got != 5 {
		t.Fatalf("visible message text click moved viewport to %d", got)
	}
}

func TestVisibleUserMessageControlsSupportRepeatedNextClicks(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, InteractiveEndLine: 8, EndLine: 10, Preview: "one"},
		{MessageID: 2, StartLine: 35, InteractiveEndLine: 38, EndLine: 40, Preview: "two"},
		{MessageID: 3, StartLine: 70, InteractiveEndLine: 73, EndLine: 75, Preview: "three"},
		{MessageID: 4, StartLine: 105, InteractiveEndLine: 108, EndLine: 110, Preview: "four"},
	}
	m.viewport.SetContent(strings.Repeat("user line\n", 150))
	m.viewport.SetYOffset(50)
	controls := m.stickyUserPromptControls(1)
	x := controls.forwardFrom
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: 0})

	_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0})
	if got := m.viewport.YOffset(); got != 70 {
		t.Fatalf("first next offset = %d, want 70", got)
	}
	_ = m.applyUserPromptNavigation(m.viewport.View())
	_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0})
	if got := m.viewport.YOffset(); got != 105 {
		t.Fatalf("second next offset = %d, want 105", got)
	}
}

func TestVisibleUserMessageControlsSupportRepeatedPreviousClicks(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, InteractiveEndLine: 8, EndLine: 10, Preview: "one"},
		{MessageID: 2, StartLine: 35, InteractiveEndLine: 38, EndLine: 40, Preview: "two"},
		{MessageID: 3, StartLine: 70, InteractiveEndLine: 73, EndLine: 75, Preview: "three"},
		{MessageID: 4, StartLine: 105, InteractiveEndLine: 108, EndLine: 110, Preview: "four"},
	}
	m.viewport.SetContent(strings.Repeat("user line\n", 150))
	m.viewport.SetYOffset(105)
	x := m.stickyUserPromptControls(3).previousFrom
	_, _ = m.Update(tea.MouseMotionMsg{X: x, Y: 0})

	_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0})
	if got := m.viewport.YOffset(); got != 70 {
		t.Fatalf("first previous offset = %d, want 70", got)
	}
	_ = m.applyUserPromptNavigation(m.viewport.View())
	_, _ = m.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: 0})
	if got := m.viewport.YOffset(); got != 35 {
		t.Fatalf("second previous offset = %d, want 35", got)
	}
}

func TestStickyUserPromptClickJumpsToPrompt(t *testing.T) {
	m := newTestChatModel(true)
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 8, EndLine: 11, Preview: "jump target"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 80))
	m.viewport.SetYOffset(30)
	m.selection.Active = true

	if !m.handleStickyUserPromptMouseClick(tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: 0}) {
		t.Fatal("sticky prompt click was not handled")
	}
	if got := m.viewport.YOffset(); got != 8 {
		t.Fatalf("viewport offset = %d, want 8", got)
	}
	if m.selection.Active {
		t.Fatal("sticky prompt click did not clear selection")
	}
}

func TestForwardNavigationTreatsClampedNextPromptAsEnd(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 50
	m.viewport.SetHeight(10)
	m.viewport.SetContent(strings.Repeat("line\n", 100))
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, InteractiveEndLine: 8, EndLine: 10, Preview: "one"},
		{MessageID: 2, StartLine: 98, InteractiveEndLine: 100, EndLine: 101, Preview: "two"},
	}
	m.viewport.SetYOffset(50)

	controls := m.stickyUserPromptControls(0)
	if controls.forwardText != "↓ end" {
		t.Fatalf("forward control = %q, want end for unreachable next prompt", controls.forwardText)
	}
	if !m.jumpUserMessage(1) || !m.viewport.AtBottom() {
		t.Fatalf("forward navigation offset = %d, want bottom", m.viewport.YOffset())
	}
	if m.jumpUserMessage(1) {
		t.Fatal("forward navigation reported movement while already at bottom")
	}
}

func TestCtrlPromptNavigationWorksWithActiveSelection(t *testing.T) {
	m := newTestChatModel(true)
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, EndLine: 8, Preview: "one"},
		{MessageID: 2, StartLine: 35, EndLine: 38, Preview: "two"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 80))
	m.viewport.SetYOffset(50)
	m.selection = Selection{Active: true, Anchor: ContentPos{Line: 1}, Cursor: ContentPos{Line: 2}}

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl})
	if got := m.viewport.YOffset(); got != 35 {
		t.Fatalf("Ctrl+Up offset = %d, want 35", got)
	}
	if m.selection.Active {
		t.Fatal("Ctrl+Up did not clear active selection")
	}
	if m.copyStatus != "" {
		t.Fatalf("Ctrl+Up copied selection instead of navigating: %q", m.copyStatus)
	}
}

func TestCtrlPromptNavigationDisabledDuringResizeReflow(t *testing.T) {
	m := newTestChatModel(true)
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{{MessageID: 1, StartLine: 5, EndLine: 8, Preview: "one"}}
	m.viewport.SetContent(strings.Repeat("line\n", 40))
	m.viewport.SetYOffset(20)
	m.resizeReflowPending = true
	if m.jumpUserMessage(-1) {
		t.Fatal("prompt navigation used stale anchors during resize reflow")
	}
	if got := m.viewport.YOffset(); got != 20 {
		t.Fatalf("resize navigation changed offset to %d", got)
	}
}

func TestCtrlUpDownNavigateUserMessages(t *testing.T) {
	m := newTestChatModel(true)
	m.viewCache.userMessageAnchors = []renderchat.UserMessageAnchor{
		{MessageID: 1, StartLine: 5, EndLine: 8, Preview: "one"},
		{MessageID: 2, StartLine: 35, EndLine: 38, Preview: "two"},
		{MessageID: 3, StartLine: 70, EndLine: 73, Preview: "three"},
	}
	m.viewport.SetContent(strings.Repeat("line\n", 120))
	m.viewport.SetYOffset(50)

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModCtrl})
	if got := m.viewport.YOffset(); got != 35 {
		t.Fatalf("Ctrl+Up offset = %d, want 35", got)
	}
	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	if got := m.viewport.YOffset(); got != 70 {
		t.Fatalf("Ctrl+Down offset = %d, want 70", got)
	}
	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModCtrl})
	if !m.viewport.AtBottom() {
		t.Fatalf("Ctrl+Down at last prompt offset = %d, want bottom", m.viewport.YOffset())
	}
}
