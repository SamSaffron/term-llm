package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/gitcommit"
	"github.com/samsaffron/term-llm/internal/session"
)

func commitAPIGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func commitAPIServer(t *testing.T) (*serveServer, session.Store, string) {
	t.Helper()
	dir := t.TempDir()
	commitAPIGit(t, dir, "init", "-q")
	commitAPIGit(t, dir, "config", "user.name", "Test")
	commitAPIGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	commitAPIGit(t, dir, "add", "-A")
	commitAPIGit(t, dir, "commit", "-qm", "base")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "commit-session", Provider: "test", Model: "test", CWD: dir, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err = store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &serveServer{store: store, cfgRef: &config.Config{Commit: config.CommitConfig{MessageAgent: "commit-message"}}}, store, dir
}
func decodeBody[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(rr.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode %d %s: %v", rr.Code, rr.Body.String(), err)
	}
	return value
}

func TestCommitStatusStageAndIdempotentOperation(t *testing.T) {
	srv, _, dir := commitAPIServer(t)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	srv.handleCommitStatus(rr, httptest.NewRequest(http.MethodGet, "/", nil), "commit-session")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	state := decodeBody[gitcommit.RepositoryState](t, rr)
	if len(state.Unstaged) != 1 || len(state.Staged) != 0 {
		t.Fatalf("state=%+v", state)
	}
	body := fmt.Sprintf(`{"mode":"all","expected_status_token":%q,"expected_fingerprint":%s}`, state.StatusToken, commitTestJSON(state.Fingerprint))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.handleCommitStage(rr, req, "commit-session")
	if rr.Code != http.StatusOK {
		t.Fatalf("stage %d: %s", rr.Code, rr.Body.String())
	}
	staged := decodeBody[gitcommit.RepositoryState](t, rr)
	opBody := fmt.Sprintf(`{"message":"Change base","expected_fingerprint":%s}`, commitTestJSON(staged.Fingerprint))
	start := func(message string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(message))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "same-key")
		rec := httptest.NewRecorder()
		srv.handleCreateCommitOperation(rec, request, "commit-session")
		return rec
	}
	first := start(opBody)
	if first.Code != http.StatusAccepted {
		t.Fatalf("operation %d: %s", first.Code, first.Body.String())
	}
	created := decodeBody[serveCommitOperation](t, first)
	deadline := time.Now().Add(5 * time.Second)
	var finished serveCommitOperation
	for time.Now().Before(deadline) {
		get := httptest.NewRecorder()
		srv.handleGetCommitOperation(get, httptest.NewRequest(http.MethodGet, "/", nil), "commit-session", created.ID)
		finished = decodeBody[serveCommitOperation](t, get)
		if finished.Status == "succeeded" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finished.Status != "succeeded" || finished.Result == nil {
		t.Fatalf("finished=%+v", finished)
	}
	replay := start(opBody)
	same := decodeBody[serveCommitOperation](t, replay)
	if same.ID != created.ID {
		t.Fatalf("replay created %q, want %q", same.ID, created.ID)
	}
	mismatch := start(strings.Replace(opBody, "Change base", "Different", 1))
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatch=%d %s", mismatch.Code, mismatch.Body.String())
	}
}

func TestCommitOperationRejectsStaleFingerprint(t *testing.T) {
	srv, _, dir := commitAPIServer(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	repo, _ := gitcommit.Open(context.Background(), dir)
	state, _ := repo.Inspect(context.Background())
	staged, _ := repo.Stage(context.Background(), gitcommit.StageRequest{Mode: gitcommit.StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0644); err != nil {
		t.Fatal(err)
	}
	commitAPIGit(t, dir, "add", "b.txt")
	body := fmt.Sprintf(`{"message":"Stale","expected_fingerprint":%s}`, commitTestJSON(staged.Fingerprint))
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "stale")
	rr := httptest.NewRecorder()
	srv.handleCreateCommitOperation(rr, req, "commit-session")
	created := decodeBody[serveCommitOperation](t, rr)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		get := httptest.NewRecorder()
		srv.handleGetCommitOperation(get, httptest.NewRequest(http.MethodGet, "/", nil), "commit-session", created.ID)
		op := decodeBody[serveCommitOperation](t, get)
		if op.Status == "failed" {
			if op.ErrorKind != gitcommit.ErrStale {
				t.Fatalf("op=%+v", op)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stale operation did not finish")
}
func commitTestJSON(v any) string { data, _ := json.Marshal(v); return string(data) }
