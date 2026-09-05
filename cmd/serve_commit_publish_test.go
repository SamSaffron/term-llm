package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/gitcommit"
)

func TestCommitPublishOperationLifecycle(t *testing.T) {
	srv, store, dir := commitAPIServer(t)
	remote := t.TempDir()
	commitAPIGit(t, remote, "init", "--bare", "-q")
	commitAPIGit(t, dir, "remote", "add", "origin", remote)
	preview := httptest.NewRecorder()
	srv.handleCommitPublishPlan(preview, httptest.NewRequest(http.MethodGet, "/?kind=push", nil), "commit-session")
	if preview.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	plan := decodeBody[gitcommit.PublishPlan](t, preview)
	body := commitTestJSON(commitOperationBody{Kind: "push", Publish: &gitcommit.PublishRequest{Plan: plan, Branch: plan.Target}})
	start := func(payload, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		rr := httptest.NewRecorder()
		srv.handleCreateCommitOperation(rr, req, "commit-session")
		return rr
	}
	rr := start(body, "publish-key")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	initial := decodeBody[serveCommitOperation](t, rr)
	srv.commitOperationsWG.Wait()
	rr = httptest.NewRecorder()
	srv.handleGetCommitOperation(rr, httptest.NewRequest(http.MethodGet, "/", nil), "commit-session", initial.ID)
	finished := decodeBody[serveCommitOperation](t, rr)
	if finished.Status != "succeeded" || finished.Kind != "push" || finished.Result != nil || finished.PublishResult == nil || !finished.PublishResult.Pushed {
		t.Fatalf("finished: %+v", finished)
	}
	replay := decodeBody[serveCommitOperation](t, start(body, "publish-key"))
	if replay.ID != initial.ID {
		t.Fatal("duplicate publish operation")
	}
	if rr := start(strings.Replace(body, `"kind":"push"`, `"kind":"pr"`, 1), "publish-key"); rr.Code != http.StatusConflict {
		t.Fatalf("key mismatch: %d", rr.Code)
	}
	// Persisted remote results survive a server restart, just like local commits.
	restarted := &serveServer{store: store}
	rr = httptest.NewRecorder()
	restarted.handleGetCommitOperation(rr, httptest.NewRequest(http.MethodGet, "/", nil), "commit-session", initial.ID)
	recovered := decodeBody[serveCommitOperation](t, rr)
	if recovered.PublishResult == nil || !recovered.PublishResult.Pushed || recovered.Kind != "push" {
		t.Fatalf("recovered: %+v", recovered)
	}
	// A publish also participates in checkout admission (including sibling sessions).
	srv.commitMu.Lock()
	srv.commitOperations[operationMapKey("commit-session", initial.ID)].Status = "running"
	srv.commitMu.Unlock()
	if !srv.commitActiveForSession(context.Background(), "commit-session") {
		t.Fatal("publish did not block session work")
	}
}

func TestCommitPublishRejectsInvalidKinds(t *testing.T) {
	srv, _, _ := commitAPIServer(t)
	for _, body := range []string{`{"kind":"pr"}`, `{"kind":"push"}`, `{"kind":"bogus","publish":{}}`, `{"publish":{}}`} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "bad-request")
		rr := httptest.NewRecorder()
		srv.handleCreateCommitOperation(rr, req, "commit-session")
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	srv.handleCommitPublishPlan(rr, httptest.NewRequest(http.MethodGet, "/?kind=other", nil), "commit-session")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid preview: %d", rr.Code)
	}
}
