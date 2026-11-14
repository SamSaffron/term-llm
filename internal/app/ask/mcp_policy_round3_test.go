package ask

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyAskExplicitResumeRejectsWithoutPersisting(t *testing.T) {
	for _, selected := range []string{"denied", "unknown"} {
		t.Run(selected, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
			db := filepath.Join(t.TempDir(), "sessions.db")
			body := fmt.Sprintf("default_provider: mock\nproviders: {mock: {model: mock-model}}\nsessions: {enabled: true, path: %q}\nskills: {enabled: false}\nmcp: {servers: {denied: {agents: []}}}\n", db)
			if err := os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "config.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: db})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			target := &session.Session{ID: session.NewID(), MCP: "saved-selection", Status: session.StatusComplete, Provider: "mock", Model: "mock-model"}
			other := &session.Session{ID: session.NewID()}
			for _, sess := range []*session.Session{target, other} {
				if err := store.Create(ctx, sess); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.SetCurrent(ctx, other.ID); err != nil {
				t.Fatal(err)
			}
			before, err := store.Get(ctx, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			deps := testDependencies(&output, &output)
			deps.SessionStore.DBPath = db
			provider := mockprovider.NewMockProvider("mock").AddTextResponse("must not execute")
			deps.NewProvider = func(*config.Config, bool) (core.Provider, error) { return provider, nil }
			err = Run(ctx, []string{"continue"}, Options{Resume: target.ID, MCP: selected, Changed: FlagState{Resume: true, MCP: true}, Text: true, Porcelain: true}, deps)
			if selected == "denied" {
				var denied *mcp.PermissionError
				if !errors.As(err, &denied) {
					t.Fatalf("denied resume=%v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "unknown MCP server") {
				t.Fatalf("unknown resume=%v", err)
			}
			after, getErr := store.Get(ctx, target.ID)
			if getErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed explicit resume persisted metadata: before=%+v after=%+v err=%v", before, after, getErr)
			}
			current, err := store.GetCurrent(ctx)
			if err != nil || current.ID != other.ID {
				t.Fatalf("failed resume changed current session: %+v %v", current, err)
			}
			if len(provider.RecordedRequests()) != 0 {
				t.Fatal("failed explicit resume invoked provider")
			}
		})
	}
}
