package ask

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/term-llm/term-llm/internal/agents"
	appserve "github.com/term-llm/term-llm/internal/app/serve"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyAskLegacyResumePreservesStateDuringLoadFailure(t *testing.T) {
	for _, kind := range []string{"legacy", "legacy-explicit", "missing"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{})
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			search := t.TempDir()
			dir := filepath.Join(search, "foo")
			if kind != "missing" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: foo\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			db := filepath.Join(t.TempDir(), "sessions.db")
			body := fmt.Sprintf("default_provider: mock\nproviders: {mock: {model: mock-model}}\nsessions: {enabled: true, path: %q}\nskills: {enabled: false}\nagents: {search_paths: [%q]}\n", db, search)
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte("{temporary"), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: db})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			sess := &session.Session{ID: session.NewID(), Agent: "foo", MCP: "previous-selection", Provider: "mock", ProviderKey: "mock", Model: "mock-model", Mode: session.ModeAsk}
			if err := store.Create(context.Background(), sess); err != nil {
				t.Fatal(err)
			}
			provider := mockprovider.NewMockProvider("mock").AddTextResponse("resumed")
			var output bytes.Buffer
			deps := testDependencies(&output, &output)
			deps.SessionStore.DBPath = db
			deps.NewProvider = func(*config.Config, bool) (core.Provider, error) { return provider, nil }
			opts := Options{Resume: sess.ID, Changed: FlagState{Resume: true}, Text: true, Porcelain: true, MaxTurns: 5}
			if kind == "legacy-explicit" {
				opts.Agent = "foo"
			}
			if err := Run(context.Background(), []string{"continue"}, opts, deps); err != nil {
				t.Fatalf("resume failed: %v\n%s", err, output.String())
			}
			saved, err := store.Get(context.Background(), sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := dir
			if kind == "missing" {
				want = "foo"
			}
			if saved.Agent != want || saved.MCP != "previous-selection" {
				t.Fatalf("failed load erased state: %+v", saved)
			}
			if len(provider.RecordedRequests()) != 1 {
				t.Fatal("resume did not execute")
			}
		})
	}
}

type countingAskResumeStore struct {
	session.Store
	currentReads int
}

func (s *countingAskResumeStore) GetCurrent(ctx context.Context) (*session.Session, error) {
	s.currentReads++
	return s.Store.GetCurrent(ctx)
}

func TestMCPPolicyAskResolvesMostRecentSessionOnlyOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	first, second := &session.Session{ID: session.NewID()}, &session.Session{ID: session.NewID()}
	for _, sess := range []*session.Session{first, second} {
		if err := store.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetCurrent(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	counted := &countingAskResumeStore{Store: store}
	opts, cfg := Options{Changed: FlagState{Resume: true}}, &config.Config{}
	agent, captured, err := resolveAskRunAgent(ctx, opts, cfg, counted)
	if err != nil {
		t.Fatal(err)
	}
	if agents.PolicyIdentity(agent) != "" {
		t.Fatal("plain session gained identity")
	}
	if err := store.SetCurrent(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	settings := appserve.SessionSettings{}
	resumed, _, _, _, err := prepareAskResume(ctx, opts, &warnings, cfg, agent, counted, &settings, captured)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID != first.ID || counted.currentReads != 1 {
		t.Fatalf("resume changed sessions: id=%s reads=%d", resumed.ID, counted.currentReads)
	}
}
