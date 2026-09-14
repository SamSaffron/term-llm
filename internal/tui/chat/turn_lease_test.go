package chat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func newTurnLeaseTestModel(t *testing.T) (*Model, *session.SQLiteStore) {
	t.Helper()
	m, store, _ := newTurnLeaseTestModelWithHistory(t, true)
	return m, store
}

// newTurnLeaseTestModelWithHistory builds a model that loaded its session the
// way a resumed TUI does. The returned path lets a test open a second store
// instance, which is how another process's writes look to this one.
func newTurnLeaseTestModelWithHistory(t *testing.T, withHistory bool) (*Model, *session.SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "mock", ProviderKey: "mock", Model: "mock-model",
		Mode: session.ModeChat, Origin: session.OriginTUI, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if withHistory {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText("earlier"), -1)); err != nil {
			t.Fatal(err)
		}
	}
	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	if loaded, err := store.GetMessages(ctx, sess.ID, 0, 0); err == nil {
		m.messages = loaded
	}
	m.noteTranscriptRev(ctx)
	return m, store, path
}

// otherProcessStore opens the same database the way a web server would.
func otherProcessStore(t *testing.T, path string) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func admitForeignTurn(t *testing.T, store *session.SQLiteStore, sessionID string) {
	t.Helper()
	if _, err := store.AdmitResponseRun(context.Background(), session.ResponseRunAdmission{ResponseID: "web_turn",
		SessionID: sessionID, RunEpoch: 1, OwnerInstanceID: "web-owner", StartedAt: time.Now(), LeaseDuration: time.Minute}); err != nil {
		t.Fatal(err)
	}
}

func runningTurnOwners(t *testing.T, store *session.SQLiteStore, sessionID string) []string {
	t.Helper()
	rows, err := store.ListAttention(context.Background(), session.AttentionListOptions{Kind: session.AttentionKindRunning})
	if err != nil {
		t.Fatal(err)
	}
	owners := make([]string, 0, len(rows.Items))
	for _, item := range rows.Items {
		if item.SessionID == sessionID {
			owners = append(owners, item.ResponseID)
		}
	}
	return owners
}

func TestSendMessageRefusedWhileAnotherProcessOwnsTheTurn(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	admitForeignTurn(t, store, m.SessionID())
	m.setTextareaValue("please answer")

	updated, _ := m.sendMessage("please answer")
	model, ok := updated.(*Model)
	if !ok {
		t.Fatalf("sendMessage returned %T", updated)
	}
	if !strings.Contains(model.footerMessage, "another process") {
		t.Fatalf("footer message = %q", model.footerMessage)
	}
	if model.streaming {
		t.Fatal("refused send started a stream")
	}
	if model.textarea.Value() != "please answer" {
		t.Fatalf("composer = %q, want the unsent draft", model.textarea.Value())
	}
	if len(model.messages) != 1 {
		t.Fatalf("in-memory messages = %d, want the loaded transcript only", len(model.messages))
	}

	ctx := context.Background()
	messages, err := store.GetMessages(ctx, m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 {
		t.Fatalf("refused send persisted %d messages", len(messages))
	}
	stored, err := store.Get(ctx, m.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if stored.UserTurns != 0 {
		t.Fatalf("refused send counted %d user turns", stored.UserTurns)
	}
	if owners := runningTurnOwners(t, store, m.SessionID()); len(owners) != 1 || owners[0] != "web_turn" {
		t.Fatalf("running turns = %v, want only the foreign one", owners)
	}
}

func TestSendMessageRefusedWhenTranscriptChangedElsewhere(t *testing.T) {
	m, store, path := newTurnLeaseTestModelWithHistory(t, true)
	ctx := context.Background()
	// A web turn appended after this process loaded the conversation.
	web := otherProcessStore(t, path)
	if err := web.AddMessage(ctx, m.SessionID(), session.NewMessage(m.SessionID(), llm.AssistantText("web answer"), -1)); err != nil {
		t.Fatal(err)
	}
	m.setTextareaValue("continue")

	updated, _ := m.sendMessage("continue")
	model := updated.(*Model)
	if !strings.Contains(model.footerMessage, "/resume") {
		t.Fatalf("footer message = %q", model.footerMessage)
	}
	if model.streaming || model.textarea.Value() != "continue" {
		t.Fatalf("stale refusal streaming=%v composer=%q", model.streaming, model.textarea.Value())
	}
	messages, err := store.GetMessages(ctx, m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("stale refusal persisted %d messages, want 2", len(messages))
	}
	// The refusal must not leave this process's own claim behind.
	if owners := runningTurnOwners(t, store, m.SessionID()); len(owners) != 0 {
		t.Fatalf("stale refusal left running turns %v", owners)
	}
}

func TestSendMessageRefusedWhenFreshSessionWasWrittenElsewhere(t *testing.T) {
	// A brand-new session has revision 0; that is still an observed revision, so
	// a web turn that lands first must not be silently dropped from the request.
	m, _, path := newTurnLeaseTestModelWithHistory(t, false)
	web := otherProcessStore(t, path)
	if err := web.AddMessage(context.Background(), m.SessionID(), session.NewMessage(m.SessionID(), llm.UserText("from the web"), -1)); err != nil {
		t.Fatal(err)
	}
	m.setTextareaValue("hello")

	updated, _ := m.sendMessage("hello")
	model := updated.(*Model)
	if !strings.Contains(model.footerMessage, "/resume") || model.streaming {
		t.Fatalf("fresh-session refusal footer=%q streaming=%v", model.footerMessage, model.streaming)
	}
}

func TestSendMessageAcceptsThisProcessesOwnWritesBetweenTurns(t *testing.T) {
	// Skill activation, undo, shell turns and similar write between turns through
	// this process's store. They move the revision but are not another process.
	m, store := newTurnLeaseTestModel(t)
	ctx := context.Background()
	if err := store.AddMessage(ctx, m.SessionID(), session.NewMessage(m.SessionID(), llm.AssistantText("skill activated"), -1)); err != nil {
		t.Fatal(err)
	}
	m.setTextareaValue("continue")

	updated, _ := m.sendMessage("continue")
	model := updated.(*Model)
	lease := model.pendingTurnLease.Load()
	if lease == nil {
		t.Fatalf("own write refused the turn: footer=%q", model.footerMessage)
	}
	t.Cleanup(func() { model.releaseTurnLease(lease, session.ResponseRunCancelled) })
	if owners := runningTurnOwners(t, store, m.SessionID()); len(owners) != 1 {
		t.Fatalf("running turns = %v, want this process's claim", owners)
	}
}

func TestNewSessionResetsTranscriptSyncPoint(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	if m.knownTranscriptRev.Load() == 0 {
		t.Fatal("loaded session should have a non-zero revision")
	}
	updated, _ := m.cmdNew()
	model := updated.(*Model)
	model.setTextareaValue("first message")

	updated, _ = model.sendMessage("first message")
	model = updated.(*Model)
	lease := model.pendingTurnLease.Load()
	if lease == nil {
		t.Fatalf("/new session refused its first turn: footer=%q", model.footerMessage)
	}
	t.Cleanup(func() { model.releaseTurnLease(lease, session.ResponseRunCancelled) })
	if owners := runningTurnOwners(t, store, model.SessionID()); len(owners) != 1 {
		t.Fatalf("running turns = %v, want one claim on the new session", owners)
	}
}

func TestLostLeaseCancelsTheRun(t *testing.T) {
	m, _ := newTurnLeaseTestModel(t)
	cancelled := false
	lease := &turnLease{stop: make(chan struct{})}
	lease.startRenewals(context.Background(), func() { cancelled = true })
	defer close(lease.stop)
	m.activeTurnLease.Store(lease)

	m.handleFencedWriteError(errors.New("unrelated"))
	if cancelled {
		t.Fatal("unrelated persistence error cancelled the run")
	}
	m.handleFencedWriteError(session.ErrResponseRunLeaseLost)
	if !cancelled {
		t.Fatal("lost lease did not cancel the run")
	}
}

func TestTurnLeaseRunsAndReleasesForCompletedTurn(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	provider := llm.NewMockProvider("mock").AddTextResponse("an answer")
	m.provider = provider
	m.engine = llm.NewEngine(provider, nil)
	manager := NewMainRunManager(context.Background())
	t.Cleanup(func() { manager.Close(time.Second) })
	m.SetMainRunManager(manager)

	if _, _ = m.sendMessage("hello"); m.pendingTurnLease.Load() == nil {
		t.Fatal("sendMessage did not claim the turn")
	}
	if owners := runningTurnOwners(t, store, m.SessionID()); len(owners) != 1 {
		t.Fatalf("running turns after claim = %v, want one", owners)
	}

	msg := m.startStream("hello")()
	if _, ok := msg.(mainRunStartedMsg); !ok {
		t.Fatalf("startStream message = %#v", msg)
	}
	manager.mu.RLock()
	run := manager.runs[m.SessionID()]
	manager.mu.RUnlock()
	select {
	case <-run.done:
	case <-time.After(5 * time.Second):
		t.Fatal("main run did not finish")
	}

	ctx := context.Background()
	if owners := runningTurnOwners(t, store, m.SessionID()); len(owners) != 0 {
		t.Fatalf("completed turn left running turns %v", owners)
	}
	attention, err := store.GetAttention(ctx, m.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if attention.Outcome != session.ResponseRunCompleted || attention.FinalRev == 0 {
		t.Fatalf("terminal lifecycle state = %+v", attention)
	}
	// The terminal user watched the turn finish; no unseen badge elsewhere.
	if attention.Unseen || attention.SeenThroughSeq < attention.LatestAttentionSeq {
		t.Fatalf("completed terminal turn left unseen attention: %+v", attention)
	}
	messages, err := store.GetMessages(ctx, m.SessionID(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var assistant bool
	for _, message := range messages {
		if message.Role == llm.RoleAssistant && strings.Contains(message.TextContent, "an answer") {
			assistant = true
		}
	}
	if !assistant {
		t.Fatalf("fenced run did not persist its assistant turn: %+v", messages)
	}
	if rev, err := store.TranscriptRev(ctx, m.SessionID()); err != nil || m.knownTranscriptRev.Load() != rev {
		t.Fatalf("known revision = %d, store revision = %d (%v)", m.knownTranscriptRev.Load(), rev, err)
	}
}

func TestStartStreamReleasesTurnLeaseWhenRunIsRejected(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	manager := NewMainRunManager(context.Background())
	t.Cleanup(func() { manager.Close(time.Second) })
	m.SetMainRunManager(manager)
	// The session already owns an active run, so Start rejects the next one.
	if _, err := manager.Start(m.SessionID(), MainRunExecution{Execute: func(ctx context.Context, _ func(ui.StreamEvent)) error {
		<-ctx.Done()
		return ctx.Err()
	}}); err != nil {
		t.Fatal(err)
	}

	msg := m.startStream("hello")()
	if _, ok := msg.(streamEventMsg); !ok {
		t.Fatalf("rejected start message = %#v", msg)
	}
	if owners := runningTurnOwners(t, store, m.SessionID()); len(owners) != 0 {
		t.Fatalf("rejected start left running turns %v", owners)
	}
	if m.pendingTurnLease.Load() != nil || m.activeTurnLease.Load() != nil {
		t.Fatal("rejected start retained a turn claim")
	}
}
