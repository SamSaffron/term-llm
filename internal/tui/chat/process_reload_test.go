package chat

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestReloadCheckpointPreservesDraftAndOriginalUserTurn(t *testing.T) {
	m := newTestChatModel(false)
	m.EnableProcessReload()
	m.textarea.SetValue("unsent draft")
	saved := &llm.Continuation{Turn: 2, Request: llm.Request{Messages: []llm.Message{llm.SystemText("system"), llm.UserText("original request"), llm.AssistantText("Hello")}}, Pending: []llm.ToolCall{{ID: "next", Name: "shell", Arguments: json.RawMessage(`{"command":"true"}`)}}}
	m.suspendForReload(saved)
	if m.streaming || m.reloadContinuation != saved {
		t.Fatal("checkpoint not parked")
	}
	reply := make(chan ReloadInspection, 1)
	m.inspectReload(ReloadInspectMsg{Context: context.Background(), Reply: reply})
	inspected := <-reply
	if inspected.Err != nil {
		t.Fatal(inspected.Err)
	}
	raw, err := json.Marshal(inspected.State)
	if err != nil {
		t.Fatal(err)
	}
	var state ReloadState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	next := newTestChatModel(false)
	next.RestoreReloadState(state)
	if next.textarea.Value() != "unsent draft" || next.reloadContinuation == nil || len(next.reloadContinuation.Pending) != 1 {
		t.Fatal("lost composer or execution cursor")
	}
	users := 0
	for _, message := range next.messages {
		if message.Role == llm.RoleUser {
			users++
		}
	}
	if users != 1 || len(next.messages) != 2 {
		t.Fatalf("restoration inserted or lost messages: %+v", next.messages)
	}
	if next.reloadContinuation.Turn != 2 {
		t.Fatal("turn budget reset")
	}
}

func TestReloadContinuationDoesNotHideCancelledToolOnDone(t *testing.T) {
	m := newTestChatModel(true)
	m.width, m.height = 100, 30
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m.store = store
	ctx := context.Background()
	if err := store.Create(ctx, m.sess); err != nil {
		t.Fatal(err)
	}
	history := []llm.Message{
		llm.UserText("sleep for 60 seconds"),
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "sleep-call", Name: "shell", Arguments: json.RawMessage(`{"command":"sleep 60","description":"Sleep for 60 seconds"}`)}}}},
		llm.ToolErrorMessage("sleep-call", "shell", "Error: context canceled", nil),
	}
	for i, msg := range history {
		if err := store.AddMessage(ctx, m.sess.ID, session.NewMessage(m.sess.ID, msg, i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.reloadMessagesFromStore(ctx); err != nil {
		t.Fatal(err)
	}
	m.RestoreReloadState(ReloadState{Continuation: &llm.Continuation{Request: llm.Request{Messages: history}, Turn: 1}})
	// Preparing the continuation must not claim its suffix-only tracker covers
	// the persisted tool batch. No provider call is needed to exercise rendering.
	m.resumeAfterReload()
	defer m.releaseStreamCancelFunc()
	const answer = "The sleep was interrupted before completion."
	m.tracker.AddTextSegment(answer, m.width)
	if err := store.AddMessage(ctx, m.sess.ID, session.NewMessage(m.sess.ID, llm.AssistantText(answer), len(history))); err != nil {
		t.Fatal(err)
	}
	m.Update(streamEventMsg{event: ui.DoneEvent(0), generation: m.streamGeneration})
	rendered := ui.StripANSI(m.renderHistory() + m.viewCache.completedStream)
	if !strings.Contains(rendered, "shell") || !strings.Contains(rendered, answer) {
		t.Fatalf("hot reload hid saved tool or answer: %s", rendered)
	}
	if strings.Count(rendered, answer) != 1 {
		t.Fatalf("duplicated resumed answer: %s", rendered)
	}
	if m.viewCache.completedStream != "" {
		t.Fatal("suffix tracker replaced the persisted turn")
	}
}
