package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type rootContextBlockingProvider struct {
	started chan context.Context
}

func TestMaybeRenameHandoverUsesRootContextCancellation(t *testing.T) {
	provider := newRootContextBlockingProvider()
	m := newTestChatModel(false)
	m.fastProvider = provider
	m.currentAgent = &agents.Agent{EnableHandover: true}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	m.SetRootContext(rootCtx)

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "xdg-data"))

	dir, err := session.GetHandoverDir(".")
	if err != nil {
		t.Fatalf("GetHandoverDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "2026-05-23-amber-anchor-apple.md")
	if err := os.WriteFile(path, []byte(strings.Repeat("handover content ", 100)), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cmd := m.maybeRenameHandoverCmd()
	if cmd == nil {
		t.Fatal("expected handover rename command")
	}

	resultCh := make(chan tea.Msg, 1)
	go func() {
		resultCh <- cmd()
	}()

	providerCtx := waitForProviderContext(t, provider.started)
	cancelRoot()
	waitForContextCancellation(t, providerCtx)

	select {
	case msg := <-resultCh:
		if _, ok := msg.(handoverRenameDoneMsg); !ok {
			t.Fatalf("handover rename command returned %T, want handoverRenameDoneMsg", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handover rename command did not return after root context cancellation")
	}
}

func newRootContextBlockingProvider() *rootContextBlockingProvider {
	return &rootContextBlockingProvider{started: make(chan context.Context, 1)}
}

func (p *rootContextBlockingProvider) Name() string { return "root-context-blocking" }

func (p *rootContextBlockingProvider) Credential() string { return "test" }

func (p *rootContextBlockingProvider) Capabilities() llm.Capabilities { return llm.Capabilities{} }

func (p *rootContextBlockingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	select {
	case p.started <- ctx:
	default:
	}
	return rootContextBlockingStream{ctx: ctx}, nil
}

type rootContextBlockingStream struct {
	ctx context.Context
}

func (s rootContextBlockingStream) Recv() (llm.Event, error) {
	<-s.ctx.Done()
	return llm.Event{}, s.ctx.Err()
}

func (s rootContextBlockingStream) Close() error { return nil }

func waitForProviderContext(t *testing.T, started <-chan context.Context) context.Context {
	t.Helper()

	select {
	case ctx := <-started:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("provider stream did not start")
		return nil
	}
}

func waitForContextCancellation(t *testing.T, ctx context.Context) {
	t.Helper()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("provider context was not cancelled by root context")
	}
}

func TestStartStreamUsesRootContextCancellation(t *testing.T) {
	provider := newRootContextBlockingProvider()
	m := newTestChatModel(false)
	m.provider = provider
	m.engine = llm.NewEngine(provider, nil)
	m.providerName = provider.Name()
	m.modelName = "root-context-model"
	m.sess = &session.Session{ID: "root-context-stream"}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	m.SetRootContext(rootCtx)

	cmd := m.startStream("hello")
	resultCh := make(chan tea.Msg, 1)
	go func() {
		resultCh <- cmd()
	}()

	providerCtx := waitForProviderContext(t, provider.started)
	cancelRoot()
	waitForContextCancellation(t, providerCtx)

	select {
	case <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("startStream command did not return after root context cancellation")
	}

	m.WaitStreamDone()
	if !errors.Is(providerCtx.Err(), context.Canceled) {
		t.Fatalf("provider context err = %v, want %v", providerCtx.Err(), context.Canceled)
	}
}

func TestCmdCompressUsesRootContextCancellation(t *testing.T) {
	provider := newRootContextBlockingProvider()
	m := newTestChatModel(false)
	m.provider = provider
	m.modelName = "root-context-model"
	m.sess = &session.Session{ID: "root-context-compact"}
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("hello"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("hi"), 1),
	}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	m.SetRootContext(rootCtx)

	_, cmd := m.cmdCompress()
	if cmd == nil {
		t.Fatal("expected compaction start command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("expected compaction batch command, got %T", cmd())
	}

	resultCh := make(chan tea.Msg, 1)
	go func() {
		resultCh <- batch[0]()
	}()

	providerCtx := waitForProviderContext(t, provider.started)
	cancelRoot()
	waitForContextCancellation(t, providerCtx)

	select {
	case msg := <-resultCh:
		done, ok := msg.(compactDoneMsg)
		if !ok {
			t.Fatalf("compaction command returned %T, want compactDoneMsg", msg)
		}
		if !errors.Is(done.err, context.Canceled) {
			t.Fatalf("compaction err = %v, want context canceled", done.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("compaction command did not return after root context cancellation")
	}
}

func TestCmdHandoverUsesRootContextCancellation(t *testing.T) {
	provider := newRootContextBlockingProvider()
	m := newTestChatModel(false)
	m.store = &mockStore{}
	m.provider = provider
	m.modelName = "root-context-model"
	m.config = &config.Config{}
	m.sess = &session.Session{ID: "root-context-handover", CreatedAt: time.Now()}
	m.messages = []session.Message{
		*session.NewMessage(m.sess.ID, llm.UserText("please continue"), 0),
		*session.NewMessage(m.sess.ID, llm.AssistantText("Working on it."), 1),
	}
	m.agentResolver = func(name string, cfg *config.Config) (*agents.Agent, error) {
		return &agents.Agent{Name: name, SystemPrompt: "You are target."}, nil
	}

	rootCtx, cancelRoot := context.WithCancel(context.Background())
	m.SetRootContext(rootCtx)

	_, cmd := m.cmdHandover([]string{"@target"})
	if cmd == nil {
		t.Fatal("expected handover start command")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) == 0 {
		t.Fatalf("expected handover batch command, got %T", cmd())
	}

	resultCh := make(chan tea.Msg, 1)
	go func() {
		resultCh <- batch[0]()
	}()

	providerCtx := waitForProviderContext(t, provider.started)
	cancelRoot()
	waitForContextCancellation(t, providerCtx)

	select {
	case msg := <-resultCh:
		done, ok := msg.(handoverDoneMsg)
		if !ok {
			t.Fatalf("handover command returned %T, want handoverDoneMsg", msg)
		}
		if !errors.Is(done.err, context.Canceled) {
			t.Fatalf("handover err = %v, want context canceled", done.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handover command did not return after root context cancellation")
	}
}
