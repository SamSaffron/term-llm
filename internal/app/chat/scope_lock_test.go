package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

// In-TUI /resume, /fork, /thread and /tree switches and quit-and-relaunch
// resumes all build their runtime here; a locked target is refused before any
// engine or provider exists.
func TestChatRuntimeRefusesLockedSessionResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("default_provider: debug\nproviders:\n  debug:\n    model: fast\nsessions:\n  enabled: true\nskills:\n  enabled: false\nchat:\n  terminal_title: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	locked := &session.Session{ID: session.NewID(), Provider: "debug:fast", ProviderKey: "debug", Model: "fast", CWD: t.TempDir(), Mode: session.ModeChat,
		ModelPolicy: modelpolicy.Policy{LockSession: true}.With("private", []string{"debug:fast"})}
	if err := store.Create(context.Background(), locked); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get(context.Background(), locked.ID); err != nil || !got.ModelPolicy.LockSession {
		t.Fatalf("fixture lock not persisted: %+v, %v", got, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appserve.SwapProcessSessionInputs(appserve.NewSessionInputCoordinator()))
	r := testInvocation()
	r.deps.SessionStore.DBPath = dbPath
	r.deps.MemoryPaths.DBPath = filepath.Join(t.TempDir(), "memory.db")
	r.deps.LoadConfig = bootstrap.LoadConfigWithSetup
	r.deps.CreateDebugLogger = bootstrap.CreateDebugLogger
	r.deps.StdoutIsTerminal = func() bool { return false }
	// The same launch the in-process session switcher builds.
	rt, err := r.buildChatSessionRuntime(context.Background(), chatSessionLaunch{
		resumeRequested: true, resumeID: locked.ID, relaunchHandoff: &chatRelaunchHandoff{branchPrefill: "draft"},
	}, nil)
	if rt != nil {
		rt.cleanupResources()
	}
	if !errors.Is(err, scopedb.ErrScope) {
		t.Fatalf("locked resume = %v, want scope refusal", err)
	}
}
