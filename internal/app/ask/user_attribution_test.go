package ask

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/session"
)

func TestAskUserPersistsAcrossAgentAndContinuation(t *testing.T) {
	for _, agent := range []bool{false, true} {
		t.Run(fmt.Sprintf("agent=%t", agent), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home)
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			path := filepath.Join(t.TempDir(), "sessions.db")
			configDir := filepath.Join(home, "term-llm")
			if err := os.MkdirAll(configDir, 0700); err != nil {
				t.Fatal(err)
			}
			data := fmt.Sprintf("default_provider: mock\nproviders:\n  mock:\n    model: mock-model\nsessions:\n  enabled: true\n  path: %q\nskills:\n  enabled: false\n", path)
			if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			provider := mockprovider.NewMockProvider("mock").AddTextResponse("first answer").AddTextResponse("second answer").AddTextResponse("anonymous answer")
			var output bytes.Buffer
			deps := testDependencies(&output, &output)
			deps.SessionStore.DBPath = path
			deps.NewProvider = func(*config.Config, bool) (core.Provider, error) { return provider, nil }
			opts := Options{User: "  canary nick kroll  ", Text: true, Porcelain: true, MaxTurns: 5}
			if agent {
				dir := filepath.Join(t.TempDir(), "attribution-agent")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: attribution-agent\n"), 0600); err != nil {
					t.Fatal(err)
				}
				opts.Agent = dir
			}
			ctx := context.Background()
			if err := Run(ctx, []string{"first question"}, opts, deps); err != nil {
				t.Fatalf("first ask: %v\n%s", err, output.String())
			}
			store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			listed, err := store.List(ctx, session.ListOptions{Limit: 10})
			if err != nil || len(listed) != 1 {
				t.Fatalf("sessions=%+v %v", listed, err)
			}
			id := listed[0].ID
			if agent {
				sess, err := store.Get(ctx, id)
				if err != nil || sess.Agent != opts.Agent {
					t.Fatalf("agent lost: %+v %v", sess, err)
				}
			}
			opts.Resume = id
			opts.Changed.Resume = true
			opts.User = "canary second user"
			if err := Run(ctx, []string{"second question"}, opts, deps); err != nil {
				t.Fatalf("continued ask: %v\n%s", err, output.String())
			}
			opts.User = ""
			if err := Run(ctx, []string{"anonymous question"}, opts, deps); err != nil {
				t.Fatalf("anonymous resume: %v\n%s", err, output.String())
			}
			rows, err := store.GetMessages(ctx, id, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			var authors []string
			for _, row := range rows {
				if row.Role == core.RoleUser {
					authors = append(authors, row.AuthorID)
				} else if row.AuthorID != "" {
					t.Fatalf("non-user stamped: %+v", row)
				}
			}
			want := []string{"canary nick kroll", "canary second user", ""}
			if len(authors) != len(want) {
				t.Fatalf("authors=%q", authors)
			}
			for i := range want {
				if authors[i] != want[i] {
					t.Fatalf("authors=%q", authors)
				}
			}
			sess, err := store.Get(ctx, id)
			if err != nil || sess.CreatedBy != want[0] {
				t.Fatalf("starter=%+v %v", sess, err)
			}
			participants, err := store.SessionParticipants(ctx, []string{id})
			if err != nil || participants[id].Count != 2 {
				t.Fatalf("participants=%+v %v", participants, err)
			}
			directory, err := store.GetAuthors(ctx, want[:2])
			if err != nil || directory[want[0]].Name != want[0] || directory[want[1]].Name != want[1] {
				t.Fatalf("directory=%+v %v", directory, err)
			}
			requests := provider.RecordedRequests()
			if len(requests) != 3 {
				t.Fatalf("requests=%d", len(requests))
			}
			for _, request := range requests {
				encoded, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(encoded), "canary ") {
					t.Fatalf("identity leaked to provider: %s", encoded)
				}
				for _, message := range request.Messages {
					if message.AuthorID != "" {
						t.Fatal("AuthorID reached provider")
					}
				}
			}
		})
	}
}
