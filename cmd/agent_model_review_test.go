package cmd

import (
	"context"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"

	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"

	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestJobsExecutorParentSelectionAndPolicyPersistence(t *testing.T) {
	old := serveFlags.Provider
	serveFlags.Provider = "other"
	t.Cleanup(func() { serveFlags.Provider = old })
	cfg := &config.Config{DefaultProvider: "other", Providers: map[string]config.ProviderConfig{"other": {Model: "wrong"}, "debug": {Model: "allowed"}}, Sessions: config.SessionsConfig{Enabled: true, Path: filepath.Join(t.TempDir(), "jobs.db")}}
	policy := modelpolicy.Policy{}.With("boss", []string{"debug:allowed"})
	parent := runpkg.ParentModel{Provider: "debug", Model: "allowed"}
	executor := newServeJobsExecutor(cfg, resolvedApprovalMode{Mode: tools.ModePrompt}, appruntime.SessionStoreOverrides{}, appmemory.Paths{}, serveRuntimeFixtureOptions(), false)
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
