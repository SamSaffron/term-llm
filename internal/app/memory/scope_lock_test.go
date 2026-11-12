package memory

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/modelpolicy"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/session/scopedb"
)

func TestMemorySourcesExcludeLockedSessions(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	old := time.Now().Add(-48 * time.Hour)
	locked := &session.Session{ID: session.NewID(), Provider: "debug", Model: "allowed", Agent: "private", Status: session.StatusComplete, CreatedAt: old, UpdatedAt: old}
	plain := &session.Session{ID: session.NewID(), Provider: "debug", Model: "allowed", Agent: "plain", Status: session.StatusComplete, CreatedAt: old, UpdatedAt: old}
	for _, s := range []*session.Session{locked, plain} {
		if err := store.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	policy := modelpolicy.Lock(nil, modelpolicy.Policy{}.With("private", []string{"debug:allowed"}))
	if _, err := store.AdmitSessionScope(ctx, scopedb.Admission{SessionID: locked.ID, Agent: "private", Policy: policy}); err != nil {
		t.Fatal(err)
	}
	sources, err := listMemorySourceSessions(ctx, store, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		if s.ID == locked.ID {
			t.Fatal("locked session listed as a memory source")
		}
	}
	// Defence in depth for stores that ignore the list filter.
	_, ok, err := (MineOptions{}).buildMemoryMineCandidate(ctx, store, session.SessionSummary{ID: locked.ID, UpdatedAt: old, Status: session.StatusComplete}, "", time.Time{})
	if err != nil || ok {
		t.Fatalf("locked candidate built: ok=%v err=%v", ok, err)
	}
}
