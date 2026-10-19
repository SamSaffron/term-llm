package filetrack

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Hold a real SQLite writer until startup maintenance is canceled. Channels,
// rather than sleeps or a large synthetic database, make the overlap reliable.
func startLockedBackgroundGC(t *testing.T, store *Store) <-chan error {
	t.Helper()
	ready := make(chan error, 1)
	finished := make(chan error, 1)
	store.backgroundGC.start(func(ctx context.Context) {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			ready <- err
			finished <- err
			return
		}
		defer conn.Close()
		_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		ready <- err
		if err != nil {
			finished <- err
			return
		}
		<-ctx.Done()
		_, err = conn.ExecContext(context.Background(), "ROLLBACK")
		finished <- err
	})
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background GC did not acquire writer")
	}
	return finished
}

func TestForegroundRecordingPreemptsBackgroundGC(t *testing.T) {
	tests := []struct {
		name   string
		record func(context.Context, *Store) error
	}{
		{"run start", func(ctx context.Context, s *Store) error {
			return s.RecordRunStart(ctx, RunRecord{SessionID: "s", RunID: "r"})
		}},
		{"file change", func(ctx context.Context, s *Store) error {
			_, err := recordTestChange(ctx, s, ChangeRecord{SessionID: "s", RunID: "r", Path: "/work/a", BeforeMissing: true, After: []byte("new\n")})
			return err
		}},
		{"observation", func(ctx context.Context, s *Store) error {
			_, err := s.RecordFilesystemObservation(ctx, FilesystemObservation{SessionID: "s", RunID: "r", Classification: ObservationUnclaimed, CoverageStatus: CoverageComplete})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t, Options{})
			finished := startLockedBackgroundGC(t, store)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := tt.record(ctx, store); err != nil {
				t.Fatalf("foreground recording blocked by startup GC: %v", err)
			}
			if err := <-finished; err != nil {
				t.Fatalf("release maintenance writer: %v", err)
			}
			var runs int
			if err := store.db.QueryRow("SELECT COUNT(*) FROM filetrack_runs WHERE session_id='s' AND run_id='r'").Scan(&runs); err != nil {
				t.Fatal(err)
			}
			if runs != 1 {
				t.Fatalf("persisted runs = %d, want 1", runs)
			}
		})
	}
}

func TestBackgroundGCCannotStartAfterForegroundRecording(t *testing.T) {
	store := openTestStore(t, Options{})
	if err := store.RecordRunStart(context.Background(), RunRecord{SessionID: "s", RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	// A delayed startup must be rejected synchronously, not launch a worker
	// that might race the next foreground write.
	store.backgroundGC.start(func(context.Context) {
		t.Error("startup GC ran after foreground recording")
	})
	if err := store.backgroundGC.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCloseDrainsBackgroundGCBeforeClosingSQLite(t *testing.T) {
	store := openTestStore(t, Options{})
	finished := startLockedBackgroundGC(t, store)
	closed := make(chan error, 1)
	go func() { closed <- store.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel and drain startup GC")
	}
	if err := <-finished; err != nil {
		t.Fatalf("maintenance could not finish before SQLite closed: %v", err)
	}
}

func TestBackgroundGCStopHonorsCallerCancellation(t *testing.T) {
	var gc backgroundGC
	started := make(chan struct{})
	release := make(chan struct{})
	gc.start(func(ctx context.Context) {
		close(started)
		<-release
	})
	<-started
	defer func() {
		close(release)
		if err := gc.stop(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gc.stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop error = %v, want context.Canceled", err)
	}
}

func TestStartBackgroundGCCompletesSweep(t *testing.T) {
	store := openTestStore(t, Options{})
	if _, err := store.db.Exec("INSERT INTO blobs(hash,size,data) VALUES('orphan',1,X'00')"); err != nil {
		t.Fatal(err)
	}
	store.StartBackgroundGC("", 0)
	select {
	case <-store.backgroundGC.done:
	case <-time.After(2 * time.Second):
		t.Fatal("startup GC did not finish")
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM blobs").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphan blobs after startup GC = %d, want 0", count)
	}
}
