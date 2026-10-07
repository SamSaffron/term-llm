package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func reviewSessionStore(t *testing.T) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSkillChildRunnerRecoversPersistedParentWhenRuntimeUnavailable(t *testing.T) {
	cfg, agentDir := modelAllowlistFixture(t, "debug:allowed")
	store := reviewSessionStore(t)
	sess := &session.Session{ID: session.NewID(), Agent: agentDir, Provider: "debug", ProviderKey: "debug", Model: "allowed", ModelPolicy: modelpolicy.Policy{}.With("ancestor", []string{"debug:*"})}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	s := &serveServer{cfgRef: cfg, store: store}
	child, err := s.serveSkillChildRunner(context.Background(), sess.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner, ok := child.(*SpawnAgentRunner)
	if !ok {
		t.Fatalf("child runner = %T", child)
	}
	defer runner.Wait()
	state := runner.modelPolicyState()
	if state.Provider != "debug" || state.Model != "allowed" || len(state.Policy.Rules) != 2 {
		t.Fatalf("parent state = %+v", state)
	}
	if err := state.Policy.CheckWithConfig(cfg, "grandchild", "other", "denied"); err == nil || !strings.Contains(err.Error(), "ancestor") {
		t.Fatalf("missing parent denial: %v", err)
	}
	if _, err := s.serveSkillChildRunner(context.Background(), "missing-session", nil); err == nil {
		t.Fatal("missing parent was launched unrestricted")
	}
}

func TestSessionModelPolicyMissingAgentVersusInvalidDefinition(t *testing.T) {
	cfg, dir := modelAllowlistFixture(t, "debug:allowed")
	s := &serveServer{cfgRef: cfg}
	persisted := modelpolicy.Policy{}.With("ancestor", []string{"debug:*"})
	sess := &session.Session{Agent: dir, ModelPolicy: persisted}
	policy, err := s.sessionModelPolicy(sess)
	if err != nil || len(policy.Rules) != 2 {
		t.Fatalf("valid agent = %+v, %v", policy, err)
	}
	if err := os.Remove(filepath.Join(dir, "agent.yaml")); err != nil {
		t.Fatal(err)
	}
	policy, err = s.sessionModelPolicy(sess)
	if err != nil || len(policy.Rules) != 1 {
		t.Fatalf("deleted agent = %+v, %v", policy, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: reviewer\nallowed_models: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sessionModelPolicy(sess); err == nil {
		t.Fatal("invalid definition lifted agent policy")
	}
}

func TestLiveControlStoreFailureDoesNotFallOpen(t *testing.T) {
	store := reviewSessionStore(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{cfgRef: &config.Config{DefaultProvider: "debug"}, store: store}
	if _, err := srv.newLiveControlProvider(context.Background(), "missing"); err == nil {
		t.Fatal("store failure permitted live control")
	}
}

func TestRunnerGrandchildInheritsRootRuleThroughUnrestrictedMiddle(t *testing.T) {
	cfg, rootAgent := modelAllowlistFixture(t, "debug:allowed")
	middle := filepath.Join(t.TempDir(), "middle")
	if err := os.MkdirAll(middle, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(middle, "agent.yaml"), []byte("name: middle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store := reviewSessionStore(t)
	runner := &cmdRunner{baseCfg: cfg, defaults: cmdRunnerOptions{Store: store}}
	root, err := runner.prepare(context.Background(), runpkg.Request{AgentName: rootAgent, Platform: runpkg.PlatformJob, Persist: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	middleRun, err := runner.prepare(context.Background(), runpkg.Request{AgentName: middle, Platform: runpkg.PlatformJob, Persist: true, IsSubagent: true, ModelPolicy: root.runtime.modelPolicy, ParentModel: runpkg.ParentModel{Provider: root.runtime.providerKey, Model: root.modelName}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer middleRun.Close()
	saved, err := store.Get(context.Background(), middleRun.req.SessionID)
	if err != nil || saved == nil || len(saved.ModelPolicy.Rules) != 1 {
		t.Fatalf("middle policy = %+v, %v", saved, err)
	}
	parent := runpkg.ParentModel{Provider: middleRun.runtime.providerKey, Model: middleRun.modelName}
	grandchild := runpkg.Request{AgentName: middle, Platform: runpkg.PlatformJob, Persist: true, IsSubagent: true, ModelPolicy: middleRun.runtime.modelPolicy, ParentModel: parent}
	_, err = runner.prepare(context.Background(), func() runpkg.Request { denied := grandchild; denied.Model = "other:denied"; return denied }(), nil)
	if err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("grandchild escaped root rule: %v", err)
	}
	admitted, err := runner.prepare(context.Background(), grandchild, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer admitted.Close()
	saved, err = store.Get(context.Background(), admitted.req.SessionID)
	if err != nil || saved == nil || len(saved.ModelPolicy.Rules) != 1 {
		t.Fatalf("grandchild inherited policy = %+v, %v", saved, err)
	}
}

func TestJobsExecutorParentSelectionAndPolicyPersistence(t *testing.T) {
	old := serveProvider
	serveProvider = "other"
	t.Cleanup(func() { serveProvider = old })
	cfg := &config.Config{DefaultProvider: "other", Providers: map[string]config.ProviderConfig{"other": {Model: "wrong"}, "debug": {Model: "allowed"}}, Sessions: config.SessionsConfig{Enabled: true, Path: filepath.Join(t.TempDir(), "jobs.db")}}
	policy := modelpolicy.Policy{}.With("boss", []string{"debug:allowed"})
	parent := runpkg.ParentModel{Provider: "debug", Model: "allowed"}
	executor := newServeJobsExecutor(cfg, resolvedApprovalMode{Mode: tools.ModePrompt})
	id := session.NewID()
	request := jobsV2LLMConfig{Instructions: "hello", Cwd: t.TempDir(), ParentModel: &parent, ModelPolicy: &policy, SessionID: id}
	if _, err := executor(context.Background(), request, nil); err != nil {
		t.Fatalf("parent selection was overridden by serve flag: %v", err)
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: cfg.Sessions.Path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saved, err := store.Get(context.Background(), id)
	if err != nil || saved == nil || saved.ProviderKey != "debug" || saved.Model != "allowed" || len(saved.ModelPolicy.Rules) != 1 {
		t.Fatalf("job session = %+v, %v", saved, err)
	}
	request.SessionID = session.NewID()
	request.Model = "other:wrong"
	if _, err := executor(context.Background(), request, nil); err == nil || !strings.Contains(err.Error(), "boss") {
		t.Fatalf("denied job = %v", err)
	}
	denied, err := store.Get(context.Background(), request.SessionID)
	if err != nil && err != session.ErrNotFound {
		t.Fatal(err)
	}
	if denied != nil {
		t.Fatalf("denied job created a session: %+v", denied)
	}
}

func TestUnrestrictedAuxiliaryCandidateParity(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "fallback", Providers: map[string]config.ProviderConfig{"debug": {Model: "main"}, "fallback": {Model: "main", FastModel: "fast"}}}
	rt := &serveRuntime{providerKey: "debug", defaultModel: "main"}
	got, err := interruptFastProvider(cfg, rt)
	if err != nil || got != nil {
		t.Fatalf("unrestricted interrupt candidate = %v, %v; must not use default provider", got, err)
	}
	rt.modelPolicy = modelpolicy.Policy{}.With("boss", []string{"debug:main", "fallback:fast"})
	got, err = interruptFastProvider(cfg, rt)
	if err != nil || got == nil {
		t.Fatalf("restricted interrupt candidate = %v, %v", got, err)
	}
}

// The legacy tool-setup helper is test-only; admission belongs to the real
// cmdRunner.prepare path, not a second selection using missing CLI flags.
func TestLegacySetupAgentToolsDoesNotRunSeparateModelAdmission(t *testing.T) {
	cfg := &config.Config{}
	runner := &SpawnAgentRunner{cfg: cfg}
	runner.SetModelPolicySource(func() ParentModelState {
		return ParentModelState{Policy: modelpolicy.Policy{}.With("boss", []string{"debug:allowed"}), Provider: "other", Model: "denied"}
	})
	engine := llm.NewEngine(llm.NewMockProvider("mock"), nil)
	agent := &agents.Agent{Name: "parent", Tools: agents.ToolsConfig{Enabled: []string{tools.SpawnAgentToolName}}}
	if _, err := runner.setupAgentTools(cfg, engine, agent, 0, "child-session"); err != nil {
		t.Fatalf("legacy setup rejected an unrelated model: %v", err)
	}
}

func TestUnrestrictedTitleRefinePreservesConfiguredProviderError(t *testing.T) {
	store := newTitleRefineTestStore(t, "title-parity")
	addTitleRefineMessage(t, store, "title-parity", llm.UserText("name this conversation"))
	calls := 0
	s := &serveServer{cfgRef: &config.Config{}, store: store, titleProviderFactory: func(*config.Config) (llm.Provider, error) { calls++; return nil, errors.New("legacy title failure") }}
	w := httptest.NewRecorder()
	s.handleSessionTitleRefine(w, httptest.NewRequest(http.MethodPost, "/v1/sessions/title-parity/title/refine", strings.NewReader(`{"preview":true}`)), "title-parity")
	if calls != 1 || w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "failed to create fast title provider: legacy title failure") {
		t.Fatalf("unrestricted title calls=%d status=%d body=%s", calls, w.Code, w.Body.String())
	}
}

func TestUnrestrictedAutoTitleFastProviderPreservesBothErrors(t *testing.T) {
	cfg := &config.Config{DefaultProvider: "fallback", Providers: map[string]config.ProviderConfig{
		"primary":  {FastProvider: "missing-primary", FastModel: "fast"},
		"fallback": {FastProvider: "missing-fallback", FastModel: "fast"},
	}}
	s := &serveServer{cfgRef: cfg}
	_, err := s.fastProvider("primary")
	if err == nil || !strings.Contains(err.Error(), `"primary" and "fallback"`) {
		t.Fatalf("unrestricted fast fallback error = %v", err)
	}
	store := reviewSessionStore(t)
	sess := &session.Session{ID: session.NewID(), Provider: "primary", ProviderKey: "primary", Model: "main"}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	s.store = store
	_, err = s.newLiveControlProvider(context.Background(), sess.ID)
	if err == nil || !strings.Contains(err.Error(), `"primary" and "fallback"`) {
		t.Fatalf("unrestricted live control fast fallback error = %v", err)
	}
}

func TestInvalidPersistedAgentBlocksAuxiliaryWork(t *testing.T) {
	store := newServeAutoTitleTestStore(t, "invalid-agent-title")
	cfg, agentDir := modelAllowlistFixture(t, "debug:allowed")
	cfg.Serve.AutoTitle = true
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte("name: reviewer\nallowed_models: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sess, err := store.Get(context.Background(), "invalid-agent-title")
	if err != nil {
		t.Fatal(err)
	}
	sess.Agent = agentDir
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s := &serveServer{cfgRef: cfg, store: store, autoTitleProviderFactory: func(string) (llm.Provider, error) { calls++; return llm.NewMockProvider("title"), nil }, liveControlProviderFactory: func(string) (llm.Provider, error) { calls++; return llm.NewMockProvider("control"), nil }}
	s.scheduleAutoTitle(sess.ID, "debug")
	s.autoTitleWG.Wait()
	if calls != 0 {
		t.Fatalf("invalid agent allowed auto-title provider: %d calls", calls)
	}
	if _, err := s.newLiveControlProvider(context.Background(), sess.ID); err == nil {
		t.Fatal("invalid agent allowed live control")
	}
	if calls != 0 {
		t.Fatalf("invalid agent allowed live provider: %d calls", calls)
	}
}

func TestDisallowedExplicitLiveControlTargetRechecksFallback(t *testing.T) {
	store := reviewSessionStore(t)
	cfg := &config.Config{DefaultProvider: "debug", Providers: map[string]config.ProviderConfig{"debug": {Model: "allowed"}, "other": {Model: "denied"}}}
	cfg.Live.ControlProvider = "other"
	cfg.Live.ControlModel = "denied"
	sess := &session.Session{ID: session.NewID(), Provider: "debug", ProviderKey: "debug", Model: "allowed", ModelPolicy: modelpolicy.Policy{}.With("boss", []string{"debug:allowed"})}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	s := &serveServer{cfgRef: cfg, store: store}
	provider, err := s.newLiveControlProvider(context.Background(), sess.ID)
	if err != nil || provider == nil {
		t.Fatalf("checked main fallback = %v, %v", provider, err)
	}
	sess.Model = "denied"
	if err := store.Update(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if _, err := s.newLiveControlProvider(context.Background(), sess.ID); err == nil || !strings.Contains(err.Error(), "boss") {
		t.Fatalf("unrestricted fallback was created: %v", err)
	}
}
