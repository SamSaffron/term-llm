package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	memorydb "github.com/term-llm/term-llm/internal/memory"
	"github.com/term-llm/term-llm/internal/session"
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

func TestStatusExcludesChildSessionsFromPending(t *testing.T) {
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
		id       string
		parentID string
		agent    string
		status   session.SessionStatus
	}{
		{id: "root", agent: "developer", status: session.StatusComplete},
		{id: "child", parentID: "root", agent: "developer", status: session.StatusComplete},
		{id: "active-child", parentID: "root", agent: "developer", status: session.StatusActive},
		{id: "grandchild", parentID: "child", agent: "child-only", status: session.StatusComplete},
		{id: "historical-child", parentID: "root", agent: "history", status: session.StatusComplete},
	} {
		sess := &session.Session{
			ID: fixture.id, ParentID: fixture.parentID, IsSubagent: fixture.parentID != "",
			Agent: fixture.agent, Status: fixture.status,
		}
		if err := sessStore.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := sessStore.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("synthetic child status fixture"), -1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Existing child-derived memory stays visible even if its session has new
	// unmined messages. Excluding it from pending must not erase its history.
	if err := memStore.UpsertState(ctx, &memorydb.MiningState{
		SessionID: "historical-child", Agent: "history", LastMinedOffset: 1, MinedAt: minedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := memStore.CreateFragment(ctx, &memorydb.Fragment{
		Agent: "history", Path: "fragments/child.md", Content: "synthetic historical memory",
	}); err != nil {
		t.Fatal(err)
	}

	developerRow := memoryStatusRow{Agent: "developer", SessionsPending: 1}
	historyRow := memoryStatusRow{Agent: "history", FragmentCount: 1, LastMinedAt: &minedAt}
	for _, tc := range []struct {
		name  string
		agent string
		want  []memoryStatusRow
	}{
		{name: "unfiltered", want: []memoryStatusRow{developerRow, historyRow}},
		{name: "parent and children share an agent", agent: "developer", want: []memoryStatusRow{developerRow}},
		{name: "child-only agent has no pending row", agent: "child-only", want: []memoryStatusRow{}},
		{name: "child memory history is retained", agent: "history", want: []memoryStatusRow{historyRow}},
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
