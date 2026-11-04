package sessionactivity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	servejobs "github.com/samsaffron/term-llm/internal/app/serve/jobs"
	"github.com/samsaffron/term-llm/internal/linkeddevices"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func activityStore(t *testing.T) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, id := range []string{"parent", "child", "grandchild", "unrelated"} {
		if err := store.Create(context.Background(), &session.Session{ID: id, Provider: "debug", Model: "actual-model", Mode: session.ModeChat}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func persistJobReference(t *testing.T, store session.Store, id, job, run string) {
	t.Helper()
	callID := "call-" + job + run
	output, _ := json.Marshal(tools.QueueAgentResult{JobID: job, RunID: run, AgentName: "developer"})
	for _, message := range []core.Message{
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: callID, Name: tools.QueueAgentToolName}}}},
		core.ToolResultMessage(callID, tools.QueueAgentToolName, string(output), nil),
	} {
		if err := store.AddMessage(context.Background(), id, session.NewMessage(id, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
}

func requestActivity(t *testing.T, handler Handler, request *http.Request) (SessionActivityResponse, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeSession(recorder, request, "parent")
	if recorder.Code != http.StatusOK {
		t.Fatalf("activity status %d: %s", recorder.Code, recorder.Body.String())
	}
	var snapshot SessionActivityResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot, recorder
}

func testChildren(_ context.Context, id string) ([]ChildRunProjection, bool, error) {
	switch id {
	case "parent":
		return []ChildRunProjection{{SessionID: "child", ParentSessionID: "parent", Live: true, Title: "Review activity", State: session.StatusActive}}, false, nil
	case "child":
		return []ChildRunProjection{{SessionID: "grandchild", ParentSessionID: "child", Title: "Check tests", State: session.StatusComplete}}, false, nil
	default:
		return []ChildRunProjection{}, false, nil
	}
}

func TestActivityLocalJobsScopeAndETag(t *testing.T) {
	store := activityStore(t)
	manager, err := servejobs.NewManager(":memory:", 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	job, err := manager.CreateJob(servejobs.Job{Name: "not a UI label", Enabled: true, RunnerType: servejobs.RunnerProgram, RunnerConfig: json.RawMessage(`{"command":"true"}`), TriggerType: servejobs.TriggerManual, TriggerConfig: json.RawMessage(`{}`), ConcurrencyPolicy: "allow", MaxConcurrentRuns: 2})
	if err != nil {
		t.Fatal(err)
	}
	run, err := manager.TriggerJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := manager.TriggerJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistJobReference(t, store, "grandchild", job.ID, run.ID)
	persistJobReference(t, store, "unrelated", job.ID, other.ID)
	handler := Handler{Store: store, Jobs: manager, Children: testChildren}
	snapshot, recorder := requestActivity(t, handler, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
	if len(snapshot.Agents) != 2 || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].RunID != run.ID || snapshot.Jobs[0].JobID != job.ID || snapshot.JobsAvailability != "available" || snapshot.JobsPartial {
		t.Fatalf("scope = %+v", snapshot)
	}
	if snapshot.Jobs[0].Title != "Developer task" || snapshot.Jobs[0].Status != "queued" {
		t.Fatalf("job projection: %+v", snapshot.Jobs[0])
	}
	if strings.Contains(recorder.Body.String(), other.ID) || strings.Contains(recorder.Body.String(), "runner_config") {
		t.Fatal("unrelated/private jobs data leaked")
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil)
	request.Header.Set("If-None-Match", recorder.Header().Get("ETag"))
	conditional := httptest.NewRecorder()
	handler.ServeSession(conditional, request, "parent")
	if conditional.Code != http.StatusNotModified || conditional.Body.Len() != 0 {
		t.Fatalf("conditional = %d %s", conditional.Code, conditional.Body.String())
	}
	if _, err := manager.CancelRun(run.ID); err != nil {
		t.Fatal(err)
	}
	updated := httptest.NewRecorder()
	handler.ServeSession(updated, request, "parent")
	if updated.Code != http.StatusOK || updated.Header().Get("ETag") == recorder.Header().Get("ETag") {
		t.Fatal("job lifecycle did not invalidate snapshot ETag")
	}
}

func TestActivityExplicitExternalJobsSummaryAndRemoteSession(t *testing.T) {
	store := activityStore(t)
	persistJobReference(t, store, "parent", "job_exact", "run_exact")
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/mounted/v2/runs/run_exact" || r.URL.RawQuery != "summary=true" || r.Header.Get("Authorization") != "Bearer private-token" {
			t.Errorf("unexpected jobs request %s auth present=%v", r.URL.String(), r.Header.Get("Authorization") != "")
			http.NotFound(w, r)
			return
		}
		writeTestJSON(w, map[string]any{"id": "run_exact", "job_id": "job_exact", "status": "running", "session_id": "child", "stdout": "private output", "input_tokens": 999, "output_tokens": 12})
	}))
	defer backend.Close()
	t.Setenv("TERM_LLM_JOBS_SERVER", backend.URL+"/mounted/")
	t.Setenv("TERM_LLM_JOBS_TOKEN", "private-token")
	handler := Handler{Store: store, Children: testChildren}
	snapshot, recorder := requestActivity(t, handler, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
	if calls.Load() != 1 || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].SessionID != "" || snapshot.Jobs[0].Status != "running" {
		t.Fatalf("external snapshot: %+v; calls %d", snapshot, calls.Load())
	}
	for _, private := range []string{"private-token", "private output", "999", "output_tokens\":12"} {
		if strings.Contains(recorder.Body.String(), private) {
			t.Fatalf("private/incomplete data exposed: %s", private)
		}
	}
	// The same native route is deliberately readable by linked devices, but the
	// jobs source is not: ZERO owner backend reads, even with exact provenance.
	linked := linkeddevices.WithDevicePrincipal(httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil), linkeddevices.DevicePrincipal{DeviceID: "phone", Scope: linkeddevices.ScopeWebV1})
	restricted, _ := requestActivity(t, handler, linked)
	if calls.Load() != 1 || restricted.JobsAvailability != "restricted" || len(restricted.Jobs) != 0 || len(restricted.Agents) != 2 {
		t.Fatalf("device activity = %+v; backend calls=%d", restricted, calls.Load())
	}
}

func TestActivityJobsFailureKeepsKnownActivityAndAgents(t *testing.T) {
	for _, mode := range []string{"unsupported", "failure", "mismatched", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			store := activityStore(t)
			persistJobReference(t, store, "parent", "job_exact", "run_exact")
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch mode {
				case "failure":
					http.Error(w, "private configuration failure", http.StatusServiceUnavailable)
				case "mismatched":
					writeTestJSON(w, map[string]string{"id": "run_exact", "job_id": "unrelated", "status": "running"})
				case "redirect":
					http.Redirect(w, r, "/owner-secrets", http.StatusFound)
				default:
					t.Error("unsupported source must never be called")
				}
			}))
			defer backend.Close()
			server := backend.URL
			if mode == "unsupported" {
				server = ""
			}
			t.Setenv("TERM_LLM_JOBS_SERVER", server)
			t.Setenv("TERM_LLM_JOBS_TOKEN", "")
			snapshot, recorder := requestActivity(t, Handler{Store: store, Children: testChildren}, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
			if snapshot.AgentsAvailability != "available" || len(snapshot.Agents) != 2 || len(snapshot.Jobs) != 1 || snapshot.Jobs[0].RunID != "run_exact" || snapshot.Jobs[0].Status != "unknown" || snapshot.JobsAvailability != "unavailable" || !snapshot.JobsPartial {
				t.Fatalf("optional jobs lost data: %+v", snapshot)
			}
			if strings.Contains(recorder.Body.String(), "private configuration") {
				t.Fatal("backend error disclosed")
			}
			expected := int32(1)
			if mode == "unsupported" {
				expected = 0
			}
			if calls.Load() != expected {
				t.Fatalf("backend calls = %d, want %d", calls.Load(), expected)
			}
		})
	}
}

func TestActivityAgentBoundsAndPartialFailure(t *testing.T) {
	store := activityStore(t)
	t.Setenv("TERM_LLM_JOBS_SERVER", "")
	handler := Handler{Store: store, Children: func(_ context.Context, id string) ([]ChildRunProjection, bool, error) {
		if id == "parent" {
			return []ChildRunProjection{{SessionID: "child"}, {SessionID: "unrelated"}}, true, nil
		}
		if id == "unrelated" {
			return nil, false, errors.New("unavailable store")
		}
		return []ChildRunProjection{{SessionID: "parent"}}, false, nil // Cycle is bounded.
	}}
	snapshot, _ := requestActivity(t, handler, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
	if len(snapshot.Agents) != 2 || !snapshot.AgentsPartial || snapshot.AgentsAvailability != "unavailable" || !snapshot.JobsPartial {
		t.Fatalf("partial agents: %+v", snapshot)
	}
	calls := 0
	handler.Children = func(_ context.Context, id string) ([]ChildRunProjection, bool, error) {
		calls++
		return []ChildRunProjection{{SessionID: id + "-next"}}, false, nil
	}
	snapshot, _ = requestActivity(t, handler, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
	if calls != maxActivityDepth || len(snapshot.Agents) != maxActivityDepth || !snapshot.AgentsPartial {
		t.Fatalf("depth bounds: %+v; calls %d", snapshot, calls)
	}
	calls = 0
	handler.Children = func(_ context.Context, id string) ([]ChildRunProjection, bool, error) {
		calls++
		children := make([]ChildRunProjection, maxActivitySessions+1)
		for i := range children {
			children[i].SessionID = id + strings.Repeat("x", i+1)
		}
		return children, false, nil
	}
	snapshot, _ = requestActivity(t, handler, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
	if calls != 1 || len(snapshot.Agents) != maxActivitySessions || !snapshot.AgentsPartial {
		t.Fatalf("node bounds: %d calls %d", len(snapshot.Agents), calls)
	}
}

func TestActivityMethodsAndMissingSessions(t *testing.T) {
	handler := Handler{Store: activityStore(t), Children: testChildren}
	for _, test := range []struct {
		method, id string
		want       int
	}{{http.MethodPost, "parent", 405}, {http.MethodGet, "missing", 404}} {
		recorder := httptest.NewRecorder()
		handler.ServeSession(recorder, httptest.NewRequest(test.method, "/activity", nil), test.id)
		if recorder.Code != test.want {
			t.Fatalf("%s %s status %d", test.method, test.id, recorder.Code)
		}
	}
}
