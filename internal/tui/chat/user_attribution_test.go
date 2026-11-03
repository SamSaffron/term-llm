package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/authoridentity"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestChatUserSwitchPersistsMultipleAuthorsWithoutProviderLeak(t *testing.T) {
	m, store, _ := newTurnLeaseTestModelWithHistory(t, false)
	provider := mockprovider.NewMockProvider("mock").AddTextResponse("one").AddTextResponse("two").AddTextResponse("three")
	m.provider = provider
	m.engine = engine.NewEngine(provider, nil)
	manager := NewMainRunManager(context.Background())
	defer manager.Close(time.Second)
	m.SetMainRunManager(manager)
	if err := m.SetUser("  canary nick kroll  "); err != nil {
		t.Fatal(err)
	}
	for i, prompt := range []string{"first question", "second question", "anonymous question"} {
		if i == 1 {
			_, _ = m.ExecuteCommand("/user canary second user")
			if !strings.Contains(m.footerMessage, "canary second user") {
				t.Fatal(m.footerMessage)
			}
		}
		if i == 2 {
			_, _ = m.ExecuteCommand("/user off")
			if !strings.Contains(m.footerMessage, "unattributed") {
				t.Fatal(m.footerMessage)
			}
		}
		_, _ = m.sendMessage(prompt)
		if msg := m.startStream(prompt)(); msg == nil {
			t.Fatal("no stream admission")
		}
		waitBarrierRun(t, manager, m.SessionID())
		// Deliver completion and reload the durable transcript, as a visible model
		// attaching after a completed background run does. No provider sleeps/UI loop.
		_, _ = m.Update(streamEventMsg{event: ui.DoneEvent(0), generation: m.streamGeneration})
		if err := m.reloadMessagesFromStore(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.GetMessages(context.Background(), m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var authors []string
	for _, row := range rows {
		if row.Role == core.RoleUser {
			authors = append(authors, row.AuthorID)
		} else if row.AuthorID != "" {
			t.Fatalf("non-user author: %+v", row)
		}
	}
	want := []string{"canary nick kroll", "canary second user", ""}
	if len(authors) != len(want) {
		t.Fatalf("authors=%q", authors)
	}
	for i := range want {
		if authors[i] != want[i] {
			t.Fatalf("authors=%q", authors)
		}
	}
	sess, err := store.Get(context.Background(), m.SessionID())
	if err != nil || sess.CreatedBy != want[0] {
		t.Fatalf("starter=%+v %v", sess, err)
	}
	participants, err := store.SessionParticipants(context.Background(), []string{m.SessionID()})
	if err != nil || participants[m.SessionID()].Count != 2 {
		t.Fatalf("participants=%+v %v", participants, err)
	}
	for _, request := range provider.RecordedRequests() {
		data, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "canary ") {
			t.Fatalf("user leaked into provider request: %s", data)
		}
		for _, message := range request.Messages {
			if message.AuthorID != "" {
				t.Fatal("provider received AuthorID")
			}
		}
	}
	if len(provider.RecordedRequests()) != 3 {
		t.Fatalf("provider requests=%d", len(provider.RecordedRequests()))
	}
}

func TestChatUserCommandStatusValidationAndSharedSelection(t *testing.T) {
	m := newTestChatModel(false)
	_, _ = m.ExecuteCommand("/user")
	if !strings.Contains(m.footerMessage, "unattributed") {
		t.Fatal(m.footerMessage)
	}
	selected := &authoridentity.Selection{}
	m.SetUserSelection(selected)
	m.setTextareaValue("/user nick kroll")
	_, _ = m.ExecuteCommand("/user nick kroll")
	if m.textarea.Value() != "" {
		t.Fatal("/user did not clear the composer")
	}
	if selected.Name() != "nick kroll" {
		t.Fatal(selected.Name())
	}
	_, _ = m.ExecuteCommand("/user")
	if !strings.Contains(m.footerMessage, "nick kroll") {
		t.Fatal(m.footerMessage)
	}
	_, _ = m.ExecuteCommand("/user bad\nname")
	if selected.Name() != "nick kroll" || !strings.Contains(m.footerMessage, "control") {
		t.Fatalf("invalid change: %q %s", selected.Name(), m.footerMessage)
	}
	next := newTestChatModel(false)
	next.SetUserSelection(selected)
	_, _ = next.ExecuteCommand("/user off")
	if m.user.Name() != "" {
		t.Fatal("replacement models did not share invocation user")
	}
	if !isStreamingLocalSlashCommand("/user nick kroll") {
		t.Fatal("/user must be safe while streaming")
	}
}

func TestChatSteeringKeepsAdmissionAuthorAfterSwitch(t *testing.T) {
	m, store, _ := newTurnLeaseTestModelWithHistory(t, false)
	_, _ = m.ExecuteCommand("/user first user")
	m.applyInterruptActionWithParts("guidance", "please check", nil, engine.InterruptSteer)
	pending := m.engine.ListPendingSteering()
	if len(pending) != 1 || pending[0].Message.AuthorID != "first user" {
		t.Fatalf("queued=%+v", pending)
	}
	_, _ = m.ExecuteCommand("/user second user")
	m.persistSteering(context.Background(), pending[0].DisplayText, pending[0].Message)
	rows, err := store.GetMessages(context.Background(), m.SessionID(), 0, 0)
	if err != nil || len(rows) != 1 || rows[0].AuthorID != "first user" {
		t.Fatalf("stored steering=%+v %v", rows, err)
	}
}

func TestChatUserSurvivesReload(t *testing.T) {
	m := newTestChatModel(false)
	_, _ = m.ExecuteCommand("/user nick kroll")
	reply := make(chan ReloadInspection, 1)
	m.inspectReload(ReloadInspectMsg{Context: context.Background(), Reply: reply})
	snapshot := <-reply
	if snapshot.Err != nil {
		t.Fatal(snapshot.Err)
	}
	data, err := json.Marshal(snapshot.State)
	if err != nil {
		t.Fatal(err)
	}
	var state ReloadState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	restored := newTestChatModel(false)
	restored.RestoreReloadState(state)
	if restored.user.Name() != "nick kroll" {
		t.Fatalf("restored user=%q", restored.user.Name())
	}
}

func TestChatUserResumeLeavesEarlierTurnsUnattributed(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	if err := m.SetUser("later user"); err != nil {
		t.Fatal(err)
	}
	_, _ = m.sendMessage("new question")
	defer m.releaseTurnLease(m.pendingTurnLease.Load(), session.ResponseRunCancelled)
	rows, err := store.GetMessages(context.Background(), m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var authors []string
	for _, row := range rows {
		if row.Role == core.RoleUser {
			authors = append(authors, row.AuthorID)
		}
	}
	if len(authors) != 2 || authors[0] != "" || authors[1] != "later user" {
		t.Fatalf("resume relabeled history: %q", authors)
	}
	sess, err := store.Get(context.Background(), m.SessionID())
	if err != nil || sess.CreatedBy != "" {
		t.Fatalf("anonymous starter changed: %+v %v", sess, err)
	}
}
