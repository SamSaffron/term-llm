package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/gitcommit"
)

func TestActiveCommitBlocksNewSessionWork(t *testing.T) {
	srv, _, dir := commitAPIServer(t)
	srv.commitMu.Lock()
	if srv.commitOperations == nil {
		srv.commitOperations = map[string]*serveCommitOperation{}
	}
	srv.commitOperations[operationMapKey("commit-session", "active")] = &serveCommitOperation{
		ID: "active", SessionID: "commit-session", Status: "running", checkoutRoot: canonicalCommitCheckout(dir),
	}
	srv.commitMu.Unlock()
	if !srv.commitActiveForSession(context.Background(), "commit-session") {
		t.Fatal("active commit operation did not block new session work")
	}
}

func TestCommitOperationConcurrentSameKeyCoalesces(t *testing.T) {
	srv, _, dir := commitAPIServer(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	repo, _ := gitcommit.Open(context.Background(), dir)
	state, _ := repo.Inspect(context.Background())
	staged, err := repo.Stage(context.Background(), gitcommit.StageRequest{Mode: gitcommit.StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"message":"Concurrent","expected_fingerprint":%s}`, commitTestJSON(staged.Fingerprint))
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			<-start
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Idempotency-Key", "concurrent-key")
			rr := httptest.NewRecorder()
			srv.handleCreateCommitOperation(rr, req, "commit-session")
			results <- rr
		}()
	}
	close(start)
	first := decodeBody[serveCommitOperation](t, <-results)
	second := decodeBody[serveCommitOperation](t, <-results)
	if first.ID != second.ID {
		t.Fatalf("same key created two operations: %q and %q", first.ID, second.ID)
	}
	srv.commitOperationsWG.Wait()
}
