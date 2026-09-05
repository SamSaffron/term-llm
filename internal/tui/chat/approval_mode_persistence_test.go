package chat

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestNewWithFastProviderAndApprovalPersistsRequestedModeImmediately(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	provider := llm.NewMockProvider("mock")
	model := NewWithFastProviderAndApproval(
		&config.Config{}, provider, nil, llm.NewEngine(provider, nil),
		"mock", "mock", mcp.NewManager(), 1,
		false, false, false, nil, "", "", false, "",
		store, nil, false, nil, false, true, "", "", false,
		tools.ModeAuto,
	)

	if got := model.ApprovalModeRequested(); got != tools.ModeAuto {
		t.Fatalf("requested mode = %v, want auto", got)
	}
	stored, err := store.Get(context.Background(), model.sess.ID)
	if err != nil {
		t.Fatalf("Get initial session: %v", err)
	}
	if stored.ApprovalMode != session.ApprovalModeAuto {
		t.Fatalf("initial session ApprovalMode = %q, want auto", stored.ApprovalMode)
	}
}

func TestNewChatSessionsPersistRequestedApprovalMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Model)
	}{
		{name: "clear", run: func(m *Model) { _, _ = m.cmdClear() }},
		{name: "new", run: func(m *Model) { _, _ = m.cmdNew() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
			if err != nil {
				t.Fatalf("NewSQLiteStore: %v", err)
			}
			defer store.Close()

			old := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeChat}
			if err := store.Create(context.Background(), old); err != nil {
				t.Fatalf("Create old session: %v", err)
			}
			m := newTestChatModel(false)
			m.store = store
			m.sess = old
			m.providerName = "mock"
			m.modelName = "mock"
			m.PersistApprovalMode(tools.ModeAuto)

			tc.run(m)
			stored, err := store.Get(context.Background(), m.sess.ID)
			if err != nil {
				t.Fatalf("Get new session: %v", err)
			}
			if stored.ApprovalMode != session.ApprovalModeAuto {
				t.Fatalf("new session ApprovalMode = %q, want auto", stored.ApprovalMode)
			}
		})
	}
}

func TestBuildHandoverSessionPersistsRequestedApprovalMode(t *testing.T) {
	m := newTestChatModel(false)
	m.PersistApprovalMode(tools.ModeAuto)
	got := m.buildHandoverSession(&handoverDoneMsg{}, nil)
	if got.ApprovalMode != session.ApprovalModeAuto {
		t.Fatalf("handover ApprovalMode = %q, want auto", got.ApprovalMode)
	}
}

func TestPersistRequestedApprovalModeSurvivesActualPromptFallback(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeChat}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	mgr := tools.NewApprovalManager(tools.NewToolPermissions())
	mgr.SetApprovalMode(tools.ModePrompt)
	m.SetApprovalManager(mgr)

	m.PersistApprovalMode(tools.ModeAuto)
	if m.ApprovalModeChanged() {
		t.Fatal("initial requested policy should not count as a user change")
	}
	if got := m.ApprovalModeActive(); got != tools.ModePrompt {
		t.Fatalf("active mode = %v, want prompt fallback", got)
	}
	if got := m.ApprovalModeRequested(); got != tools.ModeAuto {
		t.Fatalf("requested mode = %v, want auto", got)
	}
	stored, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.ApprovalMode != session.ApprovalModeAuto {
		t.Fatalf("stored ApprovalMode = %q, want auto", stored.ApprovalMode)
	}
}

func TestPersistApprovalModeSkipsUnchangedSession(t *testing.T) {
	store := &mockStore{}
	sess := &session.Session{ID: session.NewID(), ApprovalMode: session.ApprovalModeAuto}
	m := newTestChatModel(false)
	m.store = store
	m.sess = sess

	m.PersistApprovalMode(tools.ModeAuto)
	if store.updated != nil {
		t.Fatal("unchanged approval mode wrote the session")
	}
	if got := m.ApprovalModeRequested(); got != tools.ModeAuto {
		t.Fatalf("requested mode = %v, want auto", got)
	}

	m.PersistApprovalMode(tools.ModeYolo)
	if store.updated != sess {
		t.Fatal("changed approval mode did not write the session")
	}
	if sess.ApprovalMode != session.ApprovalModeYolo {
		t.Fatalf("session approval mode = %q, want yolo", sess.ApprovalMode)
	}
}

func TestNewChatSkipsScrollbackReadForFreshSession(t *testing.T) {
	store := &mockStore{}
	provider := llm.NewMockProvider("mock")
	model := NewWithFastProviderAndApproval(
		&config.Config{}, provider, nil, llm.NewEngine(provider, nil),
		"mock", "mock", mcp.NewManager(), 1,
		false, false, false, nil, "", "", false, "",
		store, nil, false, nil, false, true, "", "", false,
		tools.ModePrompt,
	)
	if model.sess == nil || len(store.created) != 1 {
		t.Fatal("fresh chat session was not created")
	}
	if store.getMessagesCalls != 0 {
		t.Fatalf("fresh chat read scrollback %d times, want 0", store.getMessagesCalls)
	}
}

func TestNewChatLoadsScrollbackForExistingSession(t *testing.T) {
	store := &mockStore{}
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeChat}
	provider := llm.NewMockProvider("mock")
	NewWithFastProviderAndApproval(
		&config.Config{}, provider, nil, llm.NewEngine(provider, nil),
		"mock", "mock", mcp.NewManager(), 1,
		false, false, false, nil, "", "", false, "",
		store, sess, false, nil, false, true, "", "", false,
		tools.ModePrompt,
	)
	if store.getMessagesCalls == 0 {
		t.Fatal("existing chat session did not load scrollback")
	}
}

func TestSetApprovalModePersistsToSession(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeChat}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	m.SetApprovalManager(tools.NewApprovalManager(tools.NewToolPermissions()))

	m.setApprovalMode(tools.ModeAuto)
	if !m.ApprovalModeChanged() {
		t.Fatal("runtime approval toggle should mark requested mode changed")
	}
	got, err := store.Get(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ApprovalMode != session.ApprovalModeAuto {
		t.Fatalf("ApprovalMode = %q, want auto", got.ApprovalMode)
	}
}
