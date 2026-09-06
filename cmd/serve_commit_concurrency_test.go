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
	"time"

	"github.com/samsaffron/term-llm/internal/gitcommit"
	"github.com/samsaffron/term-llm/internal/session"
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

func TestCommitStatusIdentifiesBlockingSession(t *testing.T) {
	activities := []struct {
		name string
		want string
		set  func(*serveServer, string, string) func()
	}{
		{"response", "an active response", func(s *serveServer, id, _ string) func() {
			rt := &serveRuntime{}
			state := &runtimeInterruptState{}
			rt.setActiveInterrupt(state)
			putTestSession(s.sessionMgr, id, rt)
			return func() { rt.clearActiveInterrupt(state) }
		}},
		{"skill", "an active skill run", func(s *serveServer, id, _ string) func() {
			run := &serveSkillRun{SessionID: id, Status: "running"}
			s.skillRuns = map[string]*serveSkillRun{"skill": run}
			return func() { run.Status = "succeeded" }
		}},
		{"commit workflow", "an active commit workflow", func(s *serveServer, id, root string) func() {
			run := &serveCommitRun{SessionID: id, Status: "cancelling", checkoutRoot: root}
			s.commitRuns = map[string]*serveCommitRun{"run": run}
			return func() { run.Status = "cancelled" }
		}},
		{"queued operation", "a queued or running Git operation", func(s *serveServer, id, root string) func() {
			op := &serveCommitOperation{SessionID: id, Status: "queued", checkoutRoot: root}
			s.commitOperations = map[string]*serveCommitOperation{"operation": op}
			return func() { op.Status = "succeeded" }
		}},
		{"running operation", "a queued or running Git operation", func(s *serveServer, id, root string) func() {
			op := &serveCommitOperation{SessionID: id, Status: "running", checkoutRoot: root}
			s.commitOperations = map[string]*serveCommitOperation{"operation": op}
			return func() { op.Status = "failed" }
		}},
	}
	for _, activity := range activities {
		for _, owner := range []string{"current session", "same checkout", "different checkout"} {
			t.Run(activity.name+"/"+owner, func(t *testing.T) {
				srv, store, dir := commitAPIServer(t)
				srv.sessionMgr = newServeSessionManager(time.Minute, 10, nil)
				defer srv.sessionMgr.Close()
				id := "commit-session"
				if owner != "current session" {
					id = "other-session"
					if owner == "different checkout" {
						dir = t.TempDir()
						commitAPIGit(t, dir, "init", "-q")
					}
					err := store.Create(context.Background(), &session.Session{
						ID: id, Name: "Other work", CWD: dir, CreatedAt: time.Now(), UpdatedAt: time.Now(),
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				finish := activity.set(srv, id, canonicalCommitCheckout(dir))
				status := func() *httptest.ResponseRecorder {
					rr := httptest.NewRecorder()
					srv.handleCommitStatus(rr, httptest.NewRequest(http.MethodGet, "/", nil), "commit-session")
					return rr
				}
				rr := status()
				if owner == "different checkout" {
					if rr.Code != http.StatusOK {
						t.Fatalf("unrelated checkout blocked: %d %s", rr.Code, rr.Body.String())
					}
				} else {
					if rr.Code != http.StatusConflict {
						t.Fatalf("status = %d, want conflict: %s", rr.Code, rr.Body.String())
					}
					body := decodeBody[struct {
						Error struct{ Message string } `json:"error"`
					}](t, rr)
					if !strings.Contains(body.Error.Message, activity.want) {
						t.Fatalf("missing activity %q: %s", activity.want, body.Error.Message)
					}
					if owner == "same checkout" {
						for _, want := range []string{"another session", "Other work", id, "sharing this checkout"} {
							if !strings.Contains(body.Error.Message, want) {
								t.Errorf("missing %q: %s", want, body.Error.Message)
							}
						}
					} else if !strings.Contains(body.Error.Message, "this session has") {
						t.Fatalf("incorrect blocker: %s", body.Error.Message)
					}
				}
				finish()
				if rr = status(); rr.Code != http.StatusOK {
					t.Fatalf("completed activity still blocks: %d %s", rr.Code, rr.Body.String())
				}
			})
		}
	}
}

func TestCommitBlockerMessageFallsBackToSessionID(t *testing.T) {
	srv, _, _ := commitAPIServer(t)
	blocker := &serveCommitBlocker{sessionID: "missing-session", activity: "an active response"}
	message := blocker.message(context.Background(), srv, "commit-session")
	if !strings.Contains(message, `another session "missing-session"`) {
		t.Fatalf("missing fallback session ID: %s", message)
	}
}
