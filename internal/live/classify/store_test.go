package liveclassify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecisionStoreRoundTripHonorsSinceAndOptionalState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	store, err := OpenDecisionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, row := range []DecisionRecord{
		{CreatedAt: now.Add(-time.Hour), LiveID: "old", BoundSession: "a", Probabilities: map[string]float64{"steer": 1}, GatedLabel: "steer", ActedLabel: "steer", Latency: 3 * time.Millisecond},
		{CreatedAt: now, LiveID: "new", BoundSession: "b", StateJSON: []byte(`{"message":"private speech"}`), Probabilities: map[string]float64{"status": 0.9}, GatedLabel: "status", ActedLabel: "status", ResolverOutcome: "", Latency: 17 * time.Millisecond},
	} {
		if !store.Enqueue(row) {
			t.Fatal("decision queue unexpectedly full")
		}
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		info, statErr := os.Stat(sidecar)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("sidecar %s mode = %o, want 600", sidecar, got)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenDecisionStoreReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.List(context.Background(), now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].LiveID != "new" || string(rows[0].StateJSON) != `{"message":"private speech"}` || rows[0].Probabilities["status"] != 0.9 || rows[0].Latency != 17*time.Millisecond {
		t.Fatalf("rows = %+v", rows)
	}
	all, err := reader.List(context.Background(), time.Time{}, 10)
	if err != nil || len(all) != 2 || len(all[1].StateJSON) != 0 {
		t.Fatalf("all rows = %+v, %v", all, err)
	}
}

func TestDecisionStoreEnqueueDoesNotWaitForSlowWriter(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	store := newDecisionStore(nil, func(context.Context, DecisionRecord) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	})
	store.writeTimeout = time.Second
	store.closeTimeout = time.Second
	go store.runWriter()

	if !store.Enqueue(DecisionRecord{LiveID: "first"}) {
		t.Fatal("first enqueue failed")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	started := time.Now()
	for i := 0; i < decisionQueueCapacity; i++ {
		if !store.Enqueue(DecisionRecord{LiveID: "later"}) {
			t.Fatalf("later enqueue %d failed while capacity remained", i)
		}
	}
	if store.Enqueue(DecisionRecord{LiveID: "dropped"}) {
		t.Fatal("enqueue unexpectedly exceeded fixed queue capacity")
	}
	if got := store.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("enqueue blocked for %v", elapsed)
	}
	close(release)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionStoreCloseReturnsWithinBoundWhenWriterIsStuck(t *testing.T) {
	entered := make(chan struct{})
	store := newDecisionStore(nil, func(context.Context, DecisionRecord) error {
		close(entered)
		select {}
	})
	store.writeTimeout = 10 * time.Millisecond
	store.closeTimeout = 40 * time.Millisecond
	go store.runWriter()
	if !store.Enqueue(DecisionRecord{}) {
		t.Fatal("enqueue failed")
	}
	<-entered
	started := time.Now()
	err := store.Close()
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Close took %v", elapsed)
	}
}

func TestDecisionStoreCreatesPrivateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	store, err := OpenDecisionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal mode = %q, want WAL", journalMode)
	}
	var busyTimeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy timeout = %d, want 5000", busyTimeout)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %o, want 600", got)
	}
}
