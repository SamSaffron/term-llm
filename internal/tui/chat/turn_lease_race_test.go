package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestTurnLeaseReleaseDuringSessionRefresh(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	ctx := context.Background()
	lease, err := m.acquireTurnLease(ctx, m.SessionID())
	if err != nil || lease == nil {
		t.Fatalf("acquire turn: lease=%v err=%v", lease, err)
	}
	if err := store.AddMessage(lease.context(ctx), m.SessionID(), session.NewMessage(m.SessionID(), core.AssistantText("finished"), -1)); err != nil {
		t.Fatal(err)
	}

	// StreamDone can reach Update before the manager finishes releasing the
	// lease. Pause just before the revision callback, then let it run alongside
	// the UI's session refresh. Neither operation is ordered before the other.
	ready, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	noteRev := lease.noteRev
	lease.noteRev = func(sessionID string, rev int64) {
		close(ready)
		<-resume
		noteRev(sessionID, rev)
	}
	// Load the replacement before unblocking finalization so SQLite's internal
	// locks cannot accidentally order the callback and the UI's pointer write.
	refreshed, err := store.Get(ctx, m.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(done)
		m.releaseTurnLease(lease, session.ResponseRunCompleted)
	}()
	<-ready
	close(resume)
	m.sess = refreshed // The UI-owned write in refreshSessionFromStore.
	<-done

	rev, err := store.TranscriptRev(ctx, m.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	assertReloadTranscriptRev(t, m, rev)
}

func TestTurnLeaseReleaseAfterSessionSwitch(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	ctx := context.Background()
	oldID := m.SessionID()
	lease, err := m.acquireTurnLease(ctx, oldID)
	if err != nil || lease == nil {
		t.Fatalf("acquire turn: lease=%v err=%v", lease, err)
	}
	if err := store.AddMessage(lease.context(ctx), oldID, session.NewMessage(oldID, core.AssistantText("old answer"), -1)); err != nil {
		t.Fatal(err)
	}
	m.cmdNew()
	if m.SessionID() == oldID {
		t.Fatal("new session did not replace the old session")
	}

	m.releaseTurnLease(lease, session.ResponseRunCompleted)
	// The new session has observed revision zero, not the old turn's final rev.
	assertReloadTranscriptRev(t, m, 0)
	lease, err = m.acquireTurnLease(ctx, m.SessionID())
	if err != nil || lease == nil {
		t.Fatalf("new session turn: lease=%v err=%v", lease, err)
	}
	m.releaseTurnLease(lease, session.ResponseRunCancelled)
}

func assertReloadTranscriptRev(t *testing.T, m *Model, want int64) {
	t.Helper()
	reply := make(chan ReloadInspection, 1)
	m.inspectReload(ReloadInspectMsg{Context: context.Background(), Reply: reply})
	result := <-reply
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.State.TranscriptRev != want {
		t.Fatalf("reload transcript revision = %d, want %d", result.State.TranscriptRev, want)
	}
}

// Embedding only Store deliberately hides the backing store's optional indexer.
type turnLeaseUnindexedStore struct{ session.Store }

type turnLeaseFailingIndexStore struct {
	session.Store
	session.TranscriptIndexer
}

func (turnLeaseFailingIndexStore) TranscriptRev(context.Context, string) (int64, error) {
	return 0, errors.New("revision unavailable")
}

func TestSessionSwitchWithoutObservedTranscriptRevision(t *testing.T) {
	for _, change := range []string{"no store", "no indexer", "revision error", "no session", "reload metadata"} {
		t.Run(change, func(t *testing.T) {
			m, store := newTurnLeaseTestModel(t)
			oldID := m.SessionID()
			ctx := context.Background()
			lease, err := m.acquireTurnLease(ctx, oldID)
			if err != nil || lease == nil {
				t.Fatalf("acquire turn: lease=%v err=%v", lease, err)
			}
			m.sess = &session.Session{ID: "replacement"}
			switch change {
			case "no store":
				m.store = nil
			case "no indexer":
				m.store = turnLeaseUnindexedStore{Store: store}
			case "revision error":
				m.store = turnLeaseFailingIndexStore{Store: store}
			case "no session":
				m.sess = nil
			case "reload metadata":
				m.store = nil
				m.RestoreReloadState(ReloadState{Meta: &session.Session{ID: "restored"}})
			}
			if change != "reload metadata" {
				m.noteTranscriptRev(ctx)
			}
			m.releaseTurnLease(lease, session.ResponseRunCompleted)
			if rev, observed := m.transcriptRevision(oldID); observed {
				t.Fatalf("retained old session revision %d after switch", rev)
			}
			if rev, observed := m.transcriptRevision(m.SessionID()); observed {
				t.Fatalf("replacement unexpectedly observed revision %d", rev)
			}
			assertReloadTranscriptRev(t, m, 0)
		})
	}
}

func TestFailedTranscriptRevisionRefreshRetainsSameSessionRevision(t *testing.T) {
	m, store := newTurnLeaseTestModel(t)
	want, observed := m.transcriptRevision(m.SessionID())
	if !observed || want == 0 {
		t.Fatal("fixture should have observed a nonzero revision")
	}
	m.store = turnLeaseFailingIndexStore{Store: store}
	m.noteTranscriptRev(context.Background())
	assertReloadTranscriptRev(t, m, want)
}
