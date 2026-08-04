package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/filetrack"
)

func installShutdownFileTrackStore(t *testing.T) (*filetrack.Store, string) {
	t.Helper()

	closeFileTrackingStore()
	path := filepath.Join(t.TempDir(), "file_history.db")
	store, err := filetrack.Open(path, filetrack.Options{})
	if err != nil {
		t.Fatal(err)
	}

	fileTrackMu.Lock()
	fileTrackStore = store
	fileTrackKey = "shutdown-test"
	fileTrackMu.Unlock()
	t.Cleanup(closeFileTrackingStore)
	return store, path
}

func startBlockingFileRecordServer(t *testing.T, store *filetrack.Store) (*serveServer, chan struct{}, <-chan error, <-chan error) {
	t.Helper()

	started := make(chan struct{})
	release := make(chan struct{})
	recordErr := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, err := store.RecordChange(context.WithoutCancel(r.Context()), filetrack.ChangeRecord{
			SessionID:     "shutdown-session",
			Path:          "/work/changed.go",
			BeforeMissing: true,
			After:         []byte("changed\n"),
		})
		recordErr <- err
		w.WriteHeader(http.StatusNoContent)
	})

	ts := httptest.NewUnstartedServer(handler)
	ts.Start()
	t.Cleanup(ts.Close)

	requestErr := make(chan error, 1)
	go func() {
		resp, err := ts.Client().Get(ts.URL)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			err = resp.Body.Close()
		}
		requestErr <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("timed out waiting for file-recording handler")
	}

	return &serveServer{server: ts.Config, shutdownCh: make(chan struct{})}, release, recordErr, requestErr
}

func TestServeServerStopKeepsFileTrackingOpenForActiveHandler(t *testing.T) {
	store, path := installShutdownFileTrackStore(t)
	srv, release, recordErr, requestErr := startBlockingFileRecordServer(t, store)

	stopErr := make(chan error, 1)
	go func() { stopErr <- srv.Stop(context.Background()) }()

	select {
	case <-srv.shutdownCh:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Stop did not begin shutdown")
	}

	// Give Stop enough time to reach its wait for the active handler. The file
	// history store must remain usable until that handler finishes recording.
	time.Sleep(100 * time.Millisecond)
	_, usableErr := store.ListSessionChanges(context.Background(), "shutdown-session")
	close(release)

	if err := <-recordErr; err != nil {
		t.Fatalf("active handler failed to record during shutdown: %v", err)
	}
	if err := <-requestErr; err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if err := <-stopErr; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if usableErr != nil {
		t.Fatalf("file tracking closed while handler was active: %v", usableErr)
	}

	reopened, err := filetrack.Open(path, filetrack.Options{})
	if err != nil {
		t.Fatalf("reopen file history: %v", err)
	}
	defer reopened.Close()
	changes, err := reopened.ListSessionChanges(context.Background(), "shutdown-session")
	if err != nil {
		t.Fatalf("list persisted changes: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("persisted changes = %d, want 1", len(changes))
	}
}

func TestServeServerStopTimeoutLeavesFileTrackingOpen(t *testing.T) {
	store, _ := installShutdownFileTrackStore(t)
	srv, release, recordErr, requestErr := startBlockingFileRecordServer(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := srv.Stop(ctx); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("Stop error = %v, want context.Canceled", err)
	}

	// Shutdown returned before the active handler exited, so its retained
	// recorder must still be able to persist the already-applied mutation.
	close(release)
	if err := <-recordErr; err != nil {
		t.Fatalf("handler failed to record after timed-out Stop: %v", err)
	}
	if err := <-requestErr; err != nil {
		t.Fatalf("request failed: %v", err)
	}
}
