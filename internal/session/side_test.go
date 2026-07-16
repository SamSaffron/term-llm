package session

import (
	"context"
	"sync"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSQLiteSideForkLifecycleAndContextIsolation(t *testing.T) {
	store, err := NewSQLiteStore(Config{Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	parent := &Session{ID: "parent", Provider: "mock", ProviderKey: "mock", Model: "m", Mode: ModeChat, Origin: OriginWeb}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "done", Name: "read_file"}
	result := llm.ToolResult{ID: "done", Content: "ok"}
	for _, msg := range []llm.Message{
		llm.UserText("parent question"),
		{Role: llm.RoleAssistant, CacheAnchor: true, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &call}}},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &result}}},
	} {
		if err := store.AddMessage(ctx, parent.ID, NewMessage(parent.ID, msg, -1)); err != nil {
			t.Fatal(err)
		}
	}

	side, err := store.ForkSide(ctx, parent.ID, OriginWeb)
	if err != nil {
		t.Fatal(err)
	}
	if side.Kind != KindSide || side.ParentID != parent.ID || side.RootID != parent.ID || side.SideState != SideOpen {
		t.Fatalf("unexpected relationship: %+v", side)
	}
	base, err := store.GetSideContext(ctx, side.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(base) != 3 || base[1].CacheAnchor {
		t.Fatalf("fork context = %#v", base)
	}
	local, err := store.GetMessages(ctx, side.ID, 0, 0)
	if err != nil || len(local) != 0 {
		t.Fatalf("local transcript = %#v, %v", local, err)
	}
	listed, err := store.List(ctx, ListOptions{})
	if err != nil || len(listed) != 1 || listed[0].ID != parent.ID {
		t.Fatalf("root list = %#v, %v", listed, err)
	}

	if _, err := store.ForkSide(ctx, parent.ID, OriginWeb); err != ErrOpenSideExists {
		t.Fatalf("second fork error = %v", err)
	}
	if err := store.CloseSide(ctx, side.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReopenSide(ctx, side.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsumeSideContext(ctx, side.ID); err != nil {
		t.Fatal(err)
	}
	base, err = store.GetSideContext(ctx, side.ID)
	if err != nil || len(base) != 0 {
		t.Fatalf("consumed context = %#v, %v", base, err)
	}
}

func TestSQLiteSideForkConcurrentUniqueness(t *testing.T) {
	store, err := NewSQLiteStore(Config{Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	parent := &Session{ID: "parent", Provider: "mock", Model: "m"}
	if err := store.Create(ctx, parent); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ForkSide(ctx, parent.ID, OriginTUI)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		switch err {
		case nil:
			success++
		case ErrOpenSideExists:
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if success != 1 || conflicts != 7 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
}

func TestPrepareForkContextSanitizesDanglingToolsAndDeepCopies(t *testing.T) {
	call := &llm.ToolCall{ID: "dangling", Name: "shell", Arguments: []byte(`{"command":"true"}`), ThoughtSig: []byte("sig")}
	replay := &llm.ProviderReplayItem{Raw: []byte(`{"id":"x"}`)}
	input := []llm.Message{{Role: llm.RoleAssistant, CacheAnchor: true, Parts: []llm.Part{
		{Type: llm.PartText, Text: "partial"},
		{Type: llm.PartToolCall, ToolCall: call},
		{Type: llm.PartProviderReplay, ProviderReplay: replay},
	}}}
	got := PrepareForkContext(input)
	if len(got) != 1 || got[0].CacheAnchor || len(got[0].Parts) != 1 || got[0].Parts[0].Text != "partial" {
		t.Fatalf("sanitized = %#v", got)
	}
	got[0].Parts[0].Text = "changed"
	if input[0].Parts[0].Text != "partial" {
		t.Fatal("fork context aliases source")
	}
}
