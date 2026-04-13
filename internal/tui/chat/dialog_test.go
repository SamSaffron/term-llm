package chat

import (
	"testing"
)

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
