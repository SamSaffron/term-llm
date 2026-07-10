package chat

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestShowContentInitializesContentDialog(t *testing.T) {
	d := NewDialogModel(nil)
	d.ShowContent("Stats", "line one\nline two")
	if !d.IsOpen() || d.Type() != DialogContent {
		t.Fatalf("expected content dialog open, got open=%v type=%v", d.IsOpen(), d.Type())
	}
	if d.Content() != "line one\nline two" {
		t.Fatalf("content = %q", d.Content())
	}
	if d.contentScroll != 0 {
		t.Fatalf("initial scroll = %d, want 0", d.contentScroll)
	}
}

func TestDialogCloseResetsContentState(t *testing.T) {
	d := NewDialogModel(nil)
	d.ShowContent("Help", "body")
	d.contentScroll = 2
	d.Close()
	if d.IsOpen() || len(d.contentLines) != 0 || d.contentScroll != 0 {
		t.Fatalf("Close did not reset content state: open=%v lines=%d scroll=%d", d.IsOpen(), len(d.contentLines), d.contentScroll)
	}
}

func TestContentDialogScrollKeys(t *testing.T) {
	d := NewDialogModel(nil)
	d.SetSize(80, 12)
	d.ShowContent("Long", strings.Join([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}, "\n"))
	d.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if d.contentScroll != 1 {
		t.Fatalf("down scroll = %d, want 1", d.contentScroll)
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if d.contentScroll <= 1 {
		t.Fatalf("pgdown did not advance scroll: %d", d.contentScroll)
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	if d.contentScroll != 0 {
		t.Fatalf("home scroll = %d, want 0", d.contentScroll)
	}
}

func TestContentDialogMouseWheelScrolls(t *testing.T) {
	d := NewDialogModel(nil)
	d.SetSize(80, 12)
	d.ShowContent("Long", strings.Join([]string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}, "\n"))
	d.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if d.contentScroll == 0 {
		t.Fatal("expected mouse wheel down to scroll content dialog")
	}
	d.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if d.contentScroll != 0 {
		t.Fatalf("mouse wheel up scroll = %d, want 0", d.contentScroll)
	}
}

func TestContentDialogViewIncludesTitleAndFooter(t *testing.T) {
	d := NewDialogModel(nil)
	d.SetSize(80, 20)
	d.ShowContent("Help", "body")
	view := d.View()
	if !strings.Contains(view, "Help") || !strings.Contains(view, "body") || !strings.Contains(view, "esc close") {
		t.Fatalf("content dialog view missing title/body/footer:\n%s", view)
	}
}

func TestContentDialogWrapsLongLinesAndReflowsOnResize(t *testing.T) {
	d := NewDialogModel(nil)
	content := strings.Repeat("x", 120)
	d.SetSize(60, 12)
	d.ShowContent("Diff", content)
	if got := len(d.renderedContentLines()); got < 3 {
		t.Fatalf("narrow rendered lines = %d, want at least 3", got)
	}
	narrowScroll := d.maxContentScroll()
	d.SetSize(100, 12)
	if got := len(d.renderedContentLines()); got >= 3 {
		t.Fatalf("wide rendered lines = %d, want fewer than 3", got)
	}
	if d.maxContentScroll() > narrowScroll {
		t.Fatalf("wide max scroll = %d, narrow = %d", d.maxContentScroll(), narrowScroll)
	}
	if d.Content() != content {
		t.Fatal("render wrapping changed raw dialog content")
	}
}

func TestContentDialogStaysWithinAvailableWidth(t *testing.T) {
	d := NewDialogModel(nil)
	d.SetSize(30, 20)
	d.ShowContent("Diff", strings.Repeat("x", 100))
	if got := lipgloss.Width(d.View()); got > 30 {
		t.Fatalf("dialog width = %d, available = 30", got)
	}
}

func TestContentDialogScrollUsesWrappedLines(t *testing.T) {
	d := NewDialogModel(nil)
	d.SetSize(44, 12)
	d.ShowContent("Diff", strings.Repeat("long-path/", 30))
	if d.maxContentScroll() == 0 {
		t.Fatal("expected wrapped content to have a scroll range")
	}
	d.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
	if d.contentScroll != d.maxContentScroll() {
		t.Fatalf("end scroll = %d, want %d", d.contentScroll, d.maxContentScroll())
	}
}

func TestShowSessionListInitializesFilteredAndSelection(t *testing.T) {
	d := NewDialogModel(nil)
	items := []DialogItem{
		{ID: "sess-a", Label: "#1"},
		{ID: "sess-b", Label: "#2"},
	}

	d.ShowSessionList(items, "sess-b")

	if len(d.filtered) != len(items) {
		t.Fatalf("expected filtered list to have %d items, got %d", len(items), len(d.filtered))
	}
	selected := d.Selected()
	if selected == nil {
		t.Fatal("expected selected item to be available")
	}
	if selected.ID != "sess-b" {
		t.Fatalf("expected selected ID %q, got %q", "sess-b", selected.ID)
	}
}

func TestDialogCloseResetsTransientState(t *testing.T) {
	d := NewDialogModel(nil)
	d.ShowSessionList([]DialogItem{{ID: "sess-a", Label: "#1"}}, "")
	d.query = "stale"

	d.Close()

	if d.dialogType != DialogNone {
		t.Fatalf("expected dialog type %v, got %v", DialogNone, d.dialogType)
	}
	if len(d.items) != 0 {
		t.Fatalf("expected items to be cleared, got %d", len(d.items))
	}
	if len(d.filtered) != 0 {
		t.Fatalf("expected filtered items to be cleared, got %d", len(d.filtered))
	}
	if d.query != "" {
		t.Fatalf("expected query to be reset, got %q", d.query)
	}
}

func testProviders() []ProviderInfo {
	return []ProviderInfo{
		{Name: "anthropic", Models: []string{"claude-sonnet", "claude-opus"}},
		{Name: "openai", Models: []string{"gpt-4o", "gpt-5"}},
	}
}

func TestShowModelPicker_NoHistory(t *testing.T) {
	d := NewDialogModel(nil)
	d.ShowModelPicker("anthropic:claude-sonnet", testProviders(), nil)

	if len(d.filtered) != 4 {
		t.Fatalf("expected 4 items, got %d", len(d.filtered))
	}
	// Default order: anthropic models first, then openai
	if d.filtered[0].ID != "anthropic:claude-sonnet" {
		t.Fatalf("expected first item anthropic:claude-sonnet, got %s", d.filtered[0].ID)
	}
	// Cursor should land on selected item
	sel := d.Selected()
	if sel == nil || sel.ID != "anthropic:claude-sonnet" {
		t.Fatalf("expected selected anthropic:claude-sonnet, got %v", sel)
	}
}

func TestShowModelPicker_MRUReorder(t *testing.T) {
	d := NewDialogModel(nil)
	recent := []string{"openai:gpt-5", "anthropic:claude-opus"}
	d.ShowModelPicker("anthropic:claude-sonnet", testProviders(), recent)

	// Recent models should be first, in MRU order
	if d.filtered[0].ID != "openai:gpt-5" {
		t.Fatalf("expected first item openai:gpt-5, got %s", d.filtered[0].ID)
	}
	if d.filtered[1].ID != "anthropic:claude-opus" {
		t.Fatalf("expected second item anthropic:claude-opus, got %s", d.filtered[1].ID)
	}
	// Rest in original order
	if d.filtered[2].ID != "anthropic:claude-sonnet" {
		t.Fatalf("expected third item anthropic:claude-sonnet, got %s", d.filtered[2].ID)
	}
	if d.filtered[3].ID != "openai:gpt-4o" {
		t.Fatalf("expected fourth item openai:gpt-4o, got %s", d.filtered[3].ID)
	}
	// Cursor should still land on the selected model
	sel := d.Selected()
	if sel == nil || sel.ID != "anthropic:claude-sonnet" {
		t.Fatalf("expected selected anthropic:claude-sonnet, got %v", sel)
	}
}

func TestShowModelPicker_HistoryEntryNotAvailable(t *testing.T) {
	d := NewDialogModel(nil)
	// Include a model in history that doesn't exist in providers
	recent := []string{"google:gemini-pro", "openai:gpt-4o"}
	d.ShowModelPicker("openai:gpt-4o", testProviders(), recent)

	// Only gpt-4o should float up; gemini-pro is ignored
	if d.filtered[0].ID != "openai:gpt-4o" {
		t.Fatalf("expected first item openai:gpt-4o, got %s", d.filtered[0].ID)
	}
	if len(d.filtered) != 4 {
		t.Fatalf("expected 4 items, got %d", len(d.filtered))
	}
}

func TestShowModelPicker_DuplicateModelName(t *testing.T) {
	// Two providers expose the same model name
	providers := []ProviderInfo{
		{Name: "acme", Models: []string{"shared-model"}},
		{Name: "beta", Models: []string{"shared-model"}},
	}
	d := NewDialogModel(nil)
	d.ShowModelPicker("beta:shared-model", providers, []string{"acme:shared-model"})

	// acme's version floats to top via MRU
	if d.filtered[0].ID != "acme:shared-model" {
		t.Fatalf("expected first item acme:shared-model, got %s", d.filtered[0].ID)
	}
	// Cursor should land on beta's version (the active model), not acme's
	sel := d.Selected()
	if sel == nil || sel.ID != "beta:shared-model" {
		t.Fatalf("expected selected beta:shared-model, got %v", sel)
	}
}
