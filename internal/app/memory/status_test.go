package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestStatusSessionAgentAttribution(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "sessions.db")
	memoryPath := filepath.Join(dir, "memory.db")
	sessStore, err := session.NewStore(session.Config{Enabled: true, Path: sessionPath})
	if err != nil {
		t.Fatal(err)
	}
	defer sessStore.Close()
	memStore := openTestMemoryStore(t, memoryPath)
	defer memStore.Close()

	minedAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	for _, fixture := range []struct {
		agent  string
		status session.SessionStatus
		mined  bool
	}{
		{agent: "developer", status: session.StatusComplete},
		{agent: " developer ", status: session.StatusComplete},
		{agent: "developer", status: session.StatusComplete, mined: true},
		{agent: "other", status: session.StatusActive},
		{agent: "", status: session.StatusComplete},
		{agent: " \t ", status: session.StatusComplete},
		{agent: "default", status: session.StatusComplete},
	} {
		sess := &session.Session{ID: session.NewID(), Agent: fixture.agent, Status: fixture.status}
		if err := sessStore.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		if err := sessStore.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("synthetic status fixture"), -1)); err != nil {
			t.Fatal(err)
		}
		if fixture.mined {
			if err := memStore.UpsertState(ctx, &memorydb.MiningState{
				SessionID: sess.ID, Agent: fixture.agent, LastMinedOffset: 1, MinedAt: minedAt,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, agent := range []string{"developer", "other", "default"} {
		if err := memStore.CreateFragment(ctx, &memorydb.Fragment{
			Agent: agent, Path: "fragments/status.md", Content: "synthetic fragment",
		}); err != nil {
			t.Fatal(err)
		}
	}

	defaultRow := memoryStatusRow{Agent: "default", FragmentCount: 1, SessionsPending: 3}
	developerRow := memoryStatusRow{Agent: "developer", FragmentCount: 1, LastMinedAt: &minedAt, SessionsPending: 2}
	otherRow := memoryStatusRow{Agent: "other", FragmentCount: 1, SessionsPending: 1}
	for _, tc := range []struct {
		name  string
		agent string
		want  []memoryStatusRow
	}{
		{name: "developer filter excludes other agents", agent: " developer ", want: []memoryStatusRow{developerRow}},
		{name: "default filter includes empty and whitespace agents", agent: "default", want: []memoryStatusRow{defaultRow}},
		{name: "unfiltered keeps per-agent counts", want: []memoryStatusRow{defaultRow, developerRow, otherRow}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{
				Agent: tc.agent, DBPath: memoryPath, DefaultDBPath: memoryPath,
				Deps: Deps{
					LoadConfig: func() (*config.Config, error) { return &config.Config{}, nil },
					SessionStoreConfig: func(*config.Config) session.Config {
						return session.Config{Enabled: true, Path: sessionPath}
					},
				},
			}
			got := captureStatusJSON(t, StatusOptions{Env: env, JSON: true})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("status rows = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// These tests must not run in parallel because Status writes to os.Stdout.
func captureStatusJSON(t *testing.T, opts StatusOptions) []memoryStatusRow {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "status-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	oldStdout := os.Stdout
	defer func() { os.Stdout = oldStdout }()
	os.Stdout = out
	err = Status(opts)
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var rows []memoryStatusRow
	if err := json.NewDecoder(out).Decode(&rows); err != nil {
		t.Fatalf("decode status JSON: %v", err)
	}
	return rows
}
