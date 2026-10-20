package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	appserve "github.com/samsaffron/term-llm/internal/app/serve"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestChatRuntimeResumeKeepsActualModelHandoverAndCleanup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(`default_provider: debug
providers:
  debug:
    model: fast
sessions:
  enabled: true
skills:
  enabled: false
chat:
  terminal_title: off
`), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "override.db")
	store, err := session.NewStore(session.Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: session.NewID(), Provider: "debug:fast", ProviderKey: "debug", Model: "fast", CWD: t.TempDir(), Mode: session.ModeChat, ApprovalMode: session.ApprovalModePrompt, Tools: tools.ReadFileToolName}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appserve.SwapProcessSessionInputs(appserve.NewSessionInputCoordinator()))
	r := testInvocation()
	r.opts.Provider = "debug:slow" // Resume must use the actual persisted model instead.
	r.opts.Tools, r.opts.Changed.Tools = tools.ReadFileToolName, true
	r.deps.SessionStore.DBPath = dbPath
	r.deps.MemoryPaths.DBPath = filepath.Join(t.TempDir(), "memory.db")
	r.deps.LoadConfig = bootstrap.LoadConfigWithSetup
	r.deps.CreateDebugLogger = bootstrap.CreateDebugLogger
	r.deps.StdoutIsTerminal = func() bool { return false }
	r.deps.ResolveApproval = func(cfg *config.Config, persisted *session.SessionApprovalMode, carried *tools.ApprovalMode) (approval.ResolvedMode, error) {
		return approval.Resolve(approval.ResolutionInput{Surface: approval.SurfaceChat, Config: cfg, Session: persisted, CLI: carried})
	}
	carried := tools.ModeYolo
	r.handoverApproval = &carried
	rt, err := r.buildChatSessionRuntime(context.Background(), chatSessionLaunch{resumeRequested: true, resumeID: sess.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.cleanupResources)
	if rt.sess.ID != sess.ID || rt.sess.Model != "fast" || rt.sess.ProviderKey != "debug" || rt.model.SessionID() != sess.ID {
		t.Fatalf("actual resume=%+v", rt.sess)
	}
	if rt.model.ApprovalModeRequested() != carried || r.opts.Yolo != true {
		t.Fatalf("handover approval=%v options=%+v", rt.model.ApprovalModeRequested(), r.opts)
	}
	if rt.toolMgr == nil || rt.toolMgr.BaseDir() != sess.CWD || rt.model.CurrentAgentMentionEngine() == nil {
		t.Fatalf("runtime lost workspace or live engine: manager=%v base=%q want=%q engine=%v", rt.toolMgr != nil, rt.toolMgr.BaseDir(), sess.CWD, rt.model.CurrentAgentMentionEngine() != nil)
	}
	rt.cleanupResources()
	rt.cleanupResources() // Ownership handoffs can converge; cleanup remains once-guarded.
	if _, err := rt.store.Get(context.Background(), sess.ID); err == nil {
		t.Fatal("runtime cleanup did not close its store")
	}
}
