package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

// Regression for the saved-prompt/retired-local-tools shape seen in 7835.
// This uses synthetic SQLite only, through the production logging decorator.
func TestSessionInputUIWrappedSQLiteRealRegistry(t *testing.T) {
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })
	ctx := context.Background()
	dir := t.TempDir()
	raw := inputTestStore(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := &session.LoggingStore{Store: raw}
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	sess := &session.Session{ID: session.NewID(), Provider: "mock", ProviderKey: "mock", Model: "mock-model", Mode: session.ModeChat, CWD: dir, Tools: "read_file", Search: true, LastTotalTokens: 321, LastMessageCount: 2}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	for i, msg := range []llm.Message{llm.SystemText("old prompt"), llm.UserText("history"), llm.AssistantText("preserve answer")} {
		if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, msg, i)); err != nil {
			t.Fatal(err)
		}
	}
	stateStore, _ := raw.(session.ProviderStateStore)
	for _, key := range []string{"mock", "other"} {
		if err := stateStore.SaveProviderState(ctx, sess.ID, key, []byte("old state")); err != nil {
			t.Fatal(err)
		}
	}
	prompt, toolSetting := "current prompt", "shell,view_image,spawn_agent"
	failFactory := false
	srv := &serveServer{store: store, cfgRef: cfg}
	factory := func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		if failFactory {
			return nil, errors.New("construction failed")
		}
		runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store, Inputs: request.Inputs, RestoreSettings: request.settings, RestoreAgentSkills: request.agentSkills, Tools: toolSetting, ToolsSet: true, SystemMessage: prompt, SystemMessageSet: true, WireSpawn: WireSpawnAgentRunner}).(*cmdRunner)
		env, err := runner.prepare(ctx, runpkg.Request{SessionID: request.SessionID, Platform: runpkg.PlatformWeb, Cwd: request.RuntimeDir, DeferSession: true, ProviderInstance: llm.NewMockProvider("mock")}, nil)
		if err != nil {
			return nil, err
		}
		return env.runtime, nil
	}
	srv.agentRuntimeFactory = factory
	srv.runtimeFactory = factory
	srv.sessionMgr = newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) { return factory(ctx, serveRuntimeRequest{}) })
	t.Cleanup(srv.sessionMgr.Close)
	// A compatibility request may warm the runtime but must not repair durable
	// prompt rows or establish once-per-process readiness.
	compat, _, err := srv.runtimeForRequest(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if processSessionInputs.ready(store, sess.ID) != nil {
		t.Fatal("compat initiated refresh")
	}
	rows, _ := store.GetMessages(ctx, sess.ID, 0, 0)
	if rows[0].TextContent != "old prompt" {
		t.Fatal("compat changed prompt")
	}
	request := serveRuntimeRequest{SessionID: sess.ID, RefreshInputs: true}
	binding := serveWorkspaceBinding{RuntimeDir: dir}
	failFactory = true
	if _, err := srv.prepareUIRuntime(ctx, request, binding); err == nil {
		t.Fatal("expected construction failure")
	}
	if current, _ := srv.sessionMgr.Get(sess.ID); current != compat {
		t.Fatal("failed candidate displaced runtime")
	}
	if processSessionInputs.ready(store, sess.ID) != nil {
		t.Fatal("failed preparation marked ready")
	}
	failFactory = false
	prepared, err := srv.prepareUIRuntime(ctx, request, binding)
	if err != nil {
		t.Fatal(err)
	}
	if total, _ := prepared.engine.ContextEstimateBaseline(); total != 321 {
		t.Fatalf("candidate lost context estimate: %d", total)
	}
	if prepared == compat {
		t.Fatal("stale hydrated prompt runtime was reused")
	}
	for _, name := range []string{"shell", "view_image", "spawn_agent"} {
		if _, ok := prepared.engine.Tools().Get(name); !ok {
			t.Fatalf("%s not executable", name)
		}
	}
	if _, ok := prepared.engine.Tools().Get("read_file"); ok {
		t.Fatal("retired configured tool remains executable")
	}
	rows, _ = store.GetMessages(ctx, sess.ID, 0, 0)
	if rows[0].TextContent != prompt || rows[1].TextContent != "history" || rows[2].TextContent != "preserve answer" {
		t.Fatalf("wrong history: %+v", rows)
	}
	persisted, _ := store.Get(ctx, sess.ID)
	if persisted.Tools != toolSetting || persisted.Model != sess.Model || persisted.Search != sess.Search || persisted.LastTotalTokens != 321 {
		t.Fatalf("unrelated settings changed: %+v", persisted)
	}
	for _, key := range []string{"mock", "other"} {
		blob, err := stateStore.LoadProviderState(ctx, sess.ID, key)
		if err != nil || len(blob) > 0 {
			t.Fatalf("old continuation survived: %q %v", blob, err)
		}
	}
	if _, err := srv.sessionMgr.pinCurrentRuntime(sess.ID, compat); !errors.Is(err, errServeSessionBusy) {
		t.Fatal("retired runtime admitted")
	}
	// Config changes do not cause another selection on subsequent UI or compat.
	prompt, toolSetting = "later config", "read_file"
	again, err := srv.prepareUIRuntime(ctx, request, binding)
	if err != nil || again != prepared {
		t.Fatalf("cache hit: %v", err)
	}
	again, _, err = srv.runtimeForRequest(ctx, sess.ID)
	if err != nil || again != prepared {
		t.Fatalf("UI then compat: %v", err)
	}
	srv.syncPersistedSessionRuntime(ctx, sess.ID, prepared, "mock-model", "", "", false, "", false)
	persisted, _ = store.Get(ctx, sess.ID)
	if persisted.Tools != "shell,view_image,spawn_agent" {
		t.Fatal("metadata save reverted Tools")
	}
	exec, swapErr := srv.beginResponseModelSwap(ctx, sess.ID, responseModelSwapPlan{enabled: true, previousProvider: "mock", previousModel: "mock-model", requestedProvider: "mock", requestedModel: "next-model"}, nil)
	if swapErr != nil {
		t.Fatal(swapErr)
	}
	if exec.candidate.inputs.Load() != prepared.inputs.Load() || exec.candidate.toolsSetting != prepared.toolsSetting {
		t.Fatal("model swap reselected inputs")
	}
	exec.markRolledBack()
	srv.restoreModelSwapRollback(ctx, sess.ID, exec, exec.candidate, "failed", "naive")
	persisted, _ = store.Get(ctx, sess.ID)
	if persisted.Tools != prepared.toolsSetting {
		t.Fatal("model-swap rollback reverted tools")
	}
	if err := stateStore.SaveProviderState(ctx, sess.ID, "mock", []byte("valid fresh state")); err != nil {
		t.Fatal(err)
	}
	// Eviction/model-only recreation must use the same selection before setup.
	replacement, err := srv.createRequestRuntime(ctx, serveRuntimeRequest{SessionID: sess.ID, RuntimeDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if replacement.systemPrompt != "current prompt" {
		t.Fatalf("recreated prompt: %q", replacement.systemPrompt)
	}
	if _, ok := replacement.engine.Tools().Get("spawn_agent"); !ok {
		t.Fatal("recreated registry lost cached tools")
	}
	valid, err := stateStore.LoadProviderState(ctx, sess.ID, "mock")
	if err != nil || string(valid) != "valid fresh state" {
		t.Fatal("recreation invalidated fresh continuation")
	}
}

// Failing hydration occurs after the narrow transaction has committed. The
// obsolete provider/history must not be restored, even for a fresh replacement.
type inputRefreshHistoryFailure struct{ session.Store }

func (s inputRefreshHistoryFailure) GetMessages(context.Context, string, int, int) ([]session.Message, error) {
	return nil, errors.New("hydrate failed")
}

func TestSessionInputPostCommitFailureRetiresOldRuntime(t *testing.T) {
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })
	ctx := context.Background()
	store := inputTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	sess := &session.Session{ID: session.NewID(), Tools: "", Provider: "mock", ProviderKey: "mock", Model: "mock"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.SystemText("obsolete"), 0)); err != nil {
		t.Fatal(err)
	}
	srv := newTestServeServer()
	srv.store = store
	t.Cleanup(srv.sessionMgr.Close)
	old, _, err := srv.runtimeForRequest(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	broken := true
	srv.agentRuntimeFactory = func(ctx context.Context, req serveRuntimeRequest) (*serveRuntime, error) {
		provider := llm.NewMockProvider("mock")
		rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), store: store, systemPrompt: "selected", baseSystemPrompt: "selected"}
		if broken {
			rt.store = inputRefreshHistoryFailure{Store: store}
		}
		return rt, nil
	}
	req := serveRuntimeRequest{SessionID: sess.ID, RefreshInputs: true}
	if _, err := srv.prepareUIRuntime(ctx, req, serveWorkspaceBinding{}); !errors.Is(err, errServeSessionPersistence) {
		t.Fatalf("preparation = %v", err)
	}
	if _, ok := srv.sessionMgr.Get(sess.ID); ok {
		t.Fatal("obsolete runtime survived committed refresh failure")
	}
	if processSessionInputs.ready(store, sess.ID) != nil {
		t.Fatal("failed publication marked ready")
	}
	rows, _ := store.GetMessages(ctx, sess.ID, 0, 0)
	if rows[0].TextContent != "selected" {
		t.Fatal("test did not reach durable commit")
	}
	if _, err := srv.sessionMgr.pinCurrentRuntime(sess.ID, old); !errors.Is(err, errServeSessionBusy) {
		t.Fatal("obsolete runtime admitted")
	}
	broken = false
	rt, err := srv.prepareUIRuntime(ctx, req, serveWorkspaceBinding{})
	if err != nil || rt.inputs.Load() == nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
}

func TestSessionInputBusyAndCancelledPreparationDoesNotDisplaceRuntime(t *testing.T) {
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })
	ctx := context.Background()
	store := inputTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	srv := newTestServeServer()
	srv.store = store
	t.Cleanup(srv.sessionMgr.Close)
	old, _, err := srv.runtimeForRequest(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := serveRuntimeRequest{SessionID: sess.ID, RefreshInputs: true}
	for _, busy := range []string{"admission", "compaction", "runtime", "side"} {
		t.Run(busy, func(t *testing.T) {
			release := func() {}
			switch busy {
			case "admission":
				var err error
				release, err = srv.sessionMgr.pinCurrentRuntime(sess.ID, old)
				if err != nil {
					t.Fatal(err)
				}
			case "compaction":
				old.compacting.Store(true)
				release = func() { old.compacting.Store(false) }
			case "runtime":
				old.mu.Lock()
				release = old.mu.Unlock
			case "side":
				old.sideQuestion.mu.Lock()
				old.sideQuestion.running = true
				old.sideQuestion.mu.Unlock()
				release = func() { old.sideQuestion.mu.Lock(); old.sideQuestion.running = false; old.sideQuestion.mu.Unlock() }
			}
			defer release()
			if _, err := srv.prepareUIRuntime(ctx, req, serveWorkspaceBinding{}); !errors.Is(err, errServeSessionBusy) {
				t.Fatalf("busy preparation = %v", err)
			}
			if current, _ := srv.sessionMgr.Get(sess.ID); current != old {
				t.Fatal("busy runtime displaced")
			}
			if processSessionInputs.ready(store, sess.ID) != nil {
				t.Fatal("busy session marked ready")
			}
		})
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := srv.prepareUIRuntime(cancelled, req, serveWorkspaceBinding{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSessionInputNoopKeepsLiveProviderAndEstimate(t *testing.T) {
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })
	ctx := context.Background()
	store := inputTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	srv := newTestServeServer()
	srv.store = store
	t.Cleanup(srv.sessionMgr.Close)
	old, _, err := srv.runtimeForRequest(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	old.store = store
	old.engine.SetContextEstimateBaseline(12345, 2)
	rt, err := srv.prepareUIRuntime(ctx, serveRuntimeRequest{SessionID: sess.ID, RefreshInputs: true}, serveWorkspaceBinding{})
	if err != nil {
		t.Fatal(err)
	}
	if rt != old || rt.inputs.Load() == nil {
		t.Fatal("no-op reset a correctly configured live runtime")
	}
	total, count := rt.engine.ContextEstimateBaseline()
	if total != 12345 || count != 2 {
		t.Fatal("refresh erased context estimates")
	}
}
