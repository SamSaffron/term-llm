package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	memorydb "github.com/samsaffron/term-llm/internal/memory"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestMemoryMinePersistedSubagentFiltering(t *testing.T) {
	ctx := context.Background()
	sessionPath := filepath.Join(t.TempDir(), "sessions.db")
	writer, err := session.NewStore(session.Config{Enabled: true, Path: sessionPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })

	quietAt := time.Now().Add(-time.Hour)
	sessions := []session.Session{
		{ID: "root", Status: session.StatusComplete},
		{ID: "web", Status: session.StatusActive, Origin: session.OriginWeb},
		{ID: "child", ParentID: "root", IsSubagent: true, Status: session.StatusComplete},
		{ID: "active-child", ParentID: "root", IsSubagent: true, Status: session.StatusActive},
		{ID: "grandchild", ParentID: "child", IsSubagent: true, Status: session.StatusComplete},
		{ID: "skill-child", Name: "/skill @developer: task", ParentID: "root", IsSubagent: true, Status: session.StatusComplete},
	}
	for i := range sessions {
		sess := &sessions[i]
		sess.Provider = "test"
		sess.Model = "test-model"
		sess.Agent = "developer"
		sess.CreatedAt = quietAt
		sess.UpdatedAt = quietAt
		if err := writer.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s): %v", sess.ID, err)
		}
		for range 4 {
			if err := writer.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, core.UserText("synthetic activity"), -1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Backdate synthetic activity without sleeping through the quiet period.
	db, err := sql.Open("sqlite", sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "UPDATE sessions SET updated_at = ?", quietAt); err != nil {
		t.Fatal(err)
	}

	// Mine reads sessions back from SQLite, where IsSubagent is not persisted.
	reader, err := session.NewStore(session.Config{Enabled: true, Path: sessionPath, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	memStore, err := memorydb.NewStore(memorydb.Config{Path: filepath.Join(t.TempDir(), "memory.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer memStore.Close()
	summaries, err := listMemorySourceSessions(ctx, reader, "developer")
	if err != nil {
		t.Fatal(err)
	}

	for _, include := range []bool{false, true} {
		name := "exclude"
		want := []string{"root", "web"}
		if include {
			name = "include"
			want = []string{"active-child", "child", "grandchild", "root", "skill-child", "web"}
		}
		t.Run(name, func(t *testing.T) {
			opts := MineOptions{Env: testEnv("", "developer"), IncludeSubagents: include}
			var mined []string
			_, err := opts.walkMineCandidates(ctx, reader, memStore, summaries, "", func(_, _ int, candidate memoryMineCandidate) (memoryMineCandidateVisitResult, error) {
				mined = append(mined, candidate.Session.ID)
				return memoryMineCandidateVisitResult{Attempted: true, Successful: true}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(mined)
			if !slices.Equal(mined, want) {
				t.Errorf("mining candidates = %v, want %v", mined, want)
			}

			candidates, err := opts.collectInsightCandidates(ctx, reader, memStore, summaries, "")
			if err != nil {
				t.Fatal(err)
			}
			var insights []string
			for _, candidate := range candidates {
				insights = append(insights, candidate.Session.ID)
			}
			slices.Sort(insights)
			if !slices.Equal(insights, want) {
				t.Errorf("insight candidates = %v, want %v", insights, want)
			}
		})
	}
}

func TestMemoryMineSubagentFlagWithoutParent(t *testing.T) {
	store := &candidateDiscoveryStore{sessions: map[string]*session.Session{
		"child": {ID: "child", IsSubagent: true},
	}}
	for _, include := range []bool{false, true} {
		opts := MineOptions{Env: testEnv("", ""), IncludeSubagents: include}
		_, eligible, err := opts.buildMemoryMineCandidate(context.Background(), store, session.SessionSummary{ID: "child"}, "", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if eligible != include {
			t.Errorf("IncludeSubagents=%v: eligible=%v", include, eligible)
		}
	}
}
