package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyChatExplicitResumeRejectsBeforeAnyPersistence(t *testing.T) {
	for _, selected := range []string{"denied", "unknown"} {
		t.Run(selected, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("default_provider: debug\nproviders: {debug: {model: fast}}\nsessions: {enabled: true}\nskills: {enabled: false}\nmcp: {servers: {denied: {agents: []}}}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(t.TempDir(), "sessions.db")
			store, err := session.NewStore(session.Config{Enabled: true, Path: db})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			target := &session.Session{ID: session.NewID(), MCP: "saved-selection", Status: session.StatusComplete, Provider: "debug", Model: "fast"}
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
			r := testInvocation()
			r.opts.MCP, r.opts.Changed.MCP = selected, true
			r.deps.LoadConfig, r.deps.SessionStore.DBPath = bootstrap.LoadConfigWithSetup, db
			rt, err := r.buildChatSessionRuntime(ctx, chatSessionLaunch{resumeRequested: true, resumeID: target.ID}, nil)
			if rt != nil {
				rt.cleanupResources()
				t.Fatal("explicit rejection built a runtime")
			}
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
		})
	}
}
