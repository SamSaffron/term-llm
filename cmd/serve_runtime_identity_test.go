package cmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

// Regression for 8232: a runtime warmed by an endpoint that carries no
// requested provider (status, shell, approvals, skills) was created with the
// global default provider and then reused for chat turns, so the session's
// model ran against the wrong provider.
func TestRuntimeWarmingBindsPersistedSessionProvider(t *testing.T) {
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })

	ctx := context.Background()
	store := inputTestStore(t, filepath.Join(t.TempDir(), "sessions.db"))
	cfg := &config.Config{
		DefaultProvider: "other",
		Providers: map[string]config.ProviderConfig{
			"other": {Model: "other-model"},
			"mock":  {Model: "mock-default"},
		},
	}
	sess := &session.Session{
		ID: session.NewID(), Provider: "Mock CLI (pinned)", ProviderKey: "mock", Model: "pinned-model",
		Mode: session.ModeChat, Origin: session.OriginWeb, CWD: t.TempDir(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	var requests []serveRuntimeRequest
	factory := func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		requests = append(requests, request)
		runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store}).(*cmdRunner)
		env, err := runner.prepare(ctx, runpkg.Request{
			SessionID: request.SessionID, Platform: runpkg.PlatformWeb, Cwd: request.RuntimeDir,
			DeferSession: true, ProviderInstance: llm.NewMockProvider("mock"),
		}, nil)
		if err != nil {
			return nil, err
		}
		return env.runtime, nil
	}
	srv := &serveServer{store: store, cfgRef: cfg}
	srv.agentRuntimeFactory = factory
	srv.runtimeFactory = factory
	srv.sessionMgr = newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return factory(ctx, serveRuntimeRequest{})
	})
	t.Cleanup(srv.sessionMgr.Close)

	if _, _, err := srv.runtimeForRequest(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("runtime requests = %d, want 1", len(requests))
	}
	if requests[0].Provider != "mock" || requests[0].Model != "pinned-model" {
		t.Fatalf("warmed runtime identity = %q/%q, want mock/pinned-model", requests[0].Provider, requests[0].Model)
	}

	// An idle runtime left on the default provider — the shape the bug produced,
	// since a runtime warmed without a requested identity is built from
	// cfg.DefaultProvider — is replaced rather than reused.
	stale, _ := newCloseTrackingServeRuntime()
	stale.providerKey, stale.defaultModel = "other", "other-model"
	putTestSession(srv.sessionMgr, sess.ID, stale)
	requests = nil
	rt, _, err := srv.runtimeForRequest(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rt == stale {
		t.Fatal("reused a runtime bound to another provider")
	}
	if len(requests) != 1 || requests[0].Provider != "mock" {
		t.Fatalf("replacement identity = %+v", requests)
	}

	srv.sessionMgr.mu.Lock()
	delete(srv.sessionMgr.sessions, sess.ID)
	srv.sessionMgr.mu.Unlock()
	requests = nil
	if _, err := srv.metadataRuntime(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("metadata runtime requests = %d, want 1", len(requests))
	}
	if requests[0].Provider != "mock" || requests[0].Model != "pinned-model" {
		t.Fatalf("metadata runtime identity = %q/%q, want mock/pinned-model", requests[0].Provider, requests[0].Model)
	}
}

// The predicate decides whether a cached runtime may keep serving a session.
func TestRuntimeReplacementPredicate(t *testing.T) {
	for _, tc := range []struct {
		name             string
		existingProvider string
		sessionProvider  string
		want             bool
	}{
		{name: "same provider is reused", existingProvider: "mock", sessionProvider: "mock"},
		{name: "other provider is replaced", existingProvider: "other", sessionProvider: "mock", want: true},
		// Keys only have to name the same provider, not spell it identically:
		// a persisted key and a resolved one may differ in case.
		{name: "case-insensitive match is reused", existingProvider: "Mock", sessionProvider: "mock"},
		// A runtime that cannot name its provider is kept: rebuilding on every
		// unknown would thrash runtimes that can serve the request perfectly
		// well, and a configured runtime always carries a key.
		{name: "unknown runtime provider is kept", existingProvider: "", sessionProvider: "mock"},
		// Without a durable provider there is nothing to disagree with, so a
		// session whose provider cannot be resolved keeps its runtime.
		{name: "unresolved session keeps its runtime", existingProvider: "mock", sessionProvider: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			existing := &serveRuntime{providerKey: tc.existingProvider}
			if got := runtimeReplacementPredicate("", nil, tc.sessionProvider)(existing); got != tc.want {
				t.Fatalf("replace = %v, want %v", got, tc.want)
			}
		})
	}
}

// Regression for the UI path: a first-party turn prepares its runtime through
// prepareUIRuntime, which reuses whatever is installed when the prepared inputs
// match. A runtime left on another provider must not be reused by that
// shortcut, or the session keeps running its model against the wrong provider.
func TestPrepareUIRuntimeRejectsForeignProviderRuntime(t *testing.T) {
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })

	ctx := context.Background()
	dir := t.TempDir()
	store := &session.LoggingStore{Store: inputTestStore(t, filepath.Join(t.TempDir(), "sessions.db"))}
	cfg := &config.Config{
		DefaultProvider: "mock",
		Providers:       map[string]config.ProviderConfig{"mock": {Model: "mock-model"}, "other": {Model: "other-model"}},
	}
	sess := &session.Session{
		ID: session.NewID(), Provider: "mock", ProviderKey: "mock", Model: "mock-model",
		Mode: session.ModeChat, Origin: session.OriginWeb, CWD: dir,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	factory := func(ctx context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		runner := newCmdRunner(cfg, cmdRunnerOptions{Store: store, Inputs: request.Inputs, RestoreSettings: request.settings, RestoreAgentSkills: request.agentSkills}).(*cmdRunner)
		env, err := runner.prepare(ctx, runpkg.Request{
			SessionID: request.SessionID, Platform: runpkg.PlatformWeb, Cwd: request.RuntimeDir,
			// The requested provider reaches the runtime exactly as it does in
			// production, so the identity checks are exercised against a
			// runtime built the ordinary way rather than a hand-set field.
			Provider: request.Provider, Model: request.Model,
			DeferSession: true, ProviderInstance: llm.NewMockProvider("mock"),
		}, nil)
		if err != nil {
			return nil, err
		}
		return env.runtime, nil
	}
	srv := &serveServer{store: store, cfgRef: cfg, agentRuntimeFactory: factory, runtimeFactory: factory}
	srv.sessionMgr = newServeSessionManager(time.Minute, 10, func(ctx context.Context) (*serveRuntime, error) {
		return factory(ctx, serveRuntimeRequest{})
	})
	t.Cleanup(srv.sessionMgr.Close)

	request := serveRuntimeRequest{SessionID: sess.ID, Provider: "mock", Model: "mock-model", RefreshInputs: true}
	binding := serveWorkspaceBinding{RuntimeDir: dir}
	prepared, err := srv.prepareUIRuntime(ctx, request, binding)
	if err != nil {
		t.Fatal(err)
	}
	// The same prepared inputs are reused rather than rebuilt every turn.
	if again, err := srv.prepareUIRuntime(ctx, request, binding); err != nil || again != prepared {
		t.Fatalf("prepared runtime was not reused: %v", err)
	}

	// The session moves to another provider (a /model switch on another
	// surface writes the row). The installed runtime is now the wrong one.
	sess.ProviderKey, sess.Provider, sess.Model = "other", "other", "other-model"
	if err := store.Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	request.Provider, request.Model = "other", "other-model"
	replacement, err := srv.prepareUIRuntime(ctx, request, binding)
	if err != nil {
		t.Fatal(err)
	}
	if replacement == prepared {
		t.Fatal("reused a runtime bound to another provider")
	}
	if got := runtimeProviderKey(replacement); got != "other" {
		t.Fatalf("replacement provider = %q, want the session's other", got)
	}
}

// A model-swap candidate runs ahead of the durable row it will write: until its
// turn persists, the session still names the previous provider. A status poll in
// that window must not "correct" the candidate away and strand the swap.
func TestRuntimeForRequestKeepsAModelSwapCandidate(t *testing.T) {
	oldCoordinator := processSessionInputs
	processSessionInputs = newSessionInputCoordinator()
	t.Cleanup(func() { processSessionInputs = oldCoordinator })

	ctx := context.Background()
	store := inputTestStore(t, filepath.Join(t.TempDir(), "sessions.db"))
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {}, "swapped": {}}}
	sess := &session.Session{
		ID: session.NewID(), Provider: "mock", ProviderKey: "mock", Model: "mock-model",
		Mode: session.ModeChat, Origin: session.OriginWeb,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store, cfgRef: cfg}
	srv.sessionMgr = newServeSessionManager(time.Minute, 10, nil)
	t.Cleanup(srv.sessionMgr.Close)
	srv.runtimeFactory = func(_ context.Context, request serveRuntimeRequest) (*serveRuntime, error) {
		rt, _ := newCloseTrackingServeRuntime()
		rt.providerKey = request.Provider
		return rt, nil
	}
	srv.agentRuntimeFactory = srv.runtimeFactory

	candidate, err := srv.createRequestRuntime(ctx, serveRuntimeRequest{SessionID: sess.ID, Provider: "swapped", Model: "swapped-model", swapCandidate: true})
	if err != nil {
		t.Fatal(err)
	}
	putTestSession(srv.sessionMgr, sess.ID, candidate)

	got, _, err := srv.runtimeForRequest(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != candidate {
		t.Fatal("status poll retired the swap candidate mid-swap")
	}
	// Once the swap is no longer in flight, the durable provider rules again.
	candidate.swapCandidate.Store(false)
	if replaced, _, err := srv.runtimeForRequest(ctx, sess.ID); err != nil || replaced == candidate {
		t.Fatalf("unowned runtime on the wrong provider was kept: %v", err)
	}
}
