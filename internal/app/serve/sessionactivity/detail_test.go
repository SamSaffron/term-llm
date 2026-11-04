package sessionactivity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/linkeddevices"
)

func TestActivityRunDetailRevalidatesMembershipAndDeniesDevice(t *testing.T) {
	store := activityStore(t)
	persistJobReference(t, store, "child", "job_exact", "run_exact")
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/mounted/v2/runs/run_exact" || r.URL.RawQuery != "" {
			t.Errorf("unexpected detail request %s", r.URL.String())
			http.NotFound(w, r)
			return
		}
		writeTestJSON(w, map[string]string{"id": "run_exact", "job_id": "job_exact", "status": "succeeded", "session_id": "child", "response": "Report complete", "stdout": strings.Repeat("x", maxActivityOutputBytes) + "secret tail", "stderr": "warning"})
	}))
	defer backend.Close()
	t.Setenv("TERM_LLM_JOBS_SERVER", backend.URL+"/mounted")
	handler := Handler{Store: store, Children: testChildren}
	request := httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity/runs/run_exact", nil)
	recorder := httptest.NewRecorder()
	handler.ServeRun(recorder, request, "parent", "run_exact")
	if recorder.Code != http.StatusOK {
		t.Fatalf("detail status %d: %s", recorder.Code, recorder.Body.String())
	}
	var detail ActivityJobDetail
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.RunID != "run_exact" || detail.JobID != "job_exact" || detail.SessionID != "" || detail.Response != "Report complete" || !detail.Truncated || len(detail.Stdout) != maxActivityOutputBytes {
		t.Fatalf("detail metadata/output invalid: %d bytes; %+v", len(detail.Stdout), detail.ActivityJobRun)
	}
	if recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("details must not be cached")
	}
	// A sibling run of the associated job is NOT in scope. Reject it before any
	// backend call, not after loading all of the job's executions.
	denied := httptest.NewRecorder()
	handler.ServeRun(denied, request, "parent", "run_other")
	if denied.Code != http.StatusNotFound || calls.Load() != 1 {
		t.Fatalf("sibling detail status %d, backend calls %d", denied.Code, calls.Load())
	}
	linked := linkeddevices.WithDevicePrincipal(request, linkeddevices.DevicePrincipal{DeviceID: "phone", Scope: linkeddevices.ScopeWebV1})
	denied = httptest.NewRecorder()
	handler.ServeRun(denied, linked, "parent", "run_exact")
	if denied.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("linked detail status %d, backend calls %d", denied.Code, calls.Load())
	}
	// Remove the durable provenance after the first read: a prior snapshot does
	// not grant continuing access to the run detail.
	if err := store.ResetMessages(context.Background(), "child", nil); err != nil {
		t.Fatal(err)
	}
	denied = httptest.NewRecorder()
	handler.ServeRun(denied, request, "parent", "run_exact")
	if denied.Code != http.StatusNotFound || calls.Load() != 1 {
		t.Fatalf("removed membership status %d, backend calls %d", denied.Code, calls.Load())
	}
}

func TestActivityRunDetailValidatesBackendIdentity(t *testing.T) {
	store := activityStore(t)
	persistJobReference(t, store, "parent", "job_exact", "run_exact")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]string{"id": "run_exact", "job_id": "unrelated", "response": "private output"})
	}))
	defer backend.Close()
	t.Setenv("TERM_LLM_JOBS_SERVER", backend.URL)
	recorder := httptest.NewRecorder()
	Handler{Store: store, Children: testChildren}.ServeRun(recorder, httptest.NewRequest(http.MethodGet, "/activity", nil), "parent", "run_exact")
	if recorder.Code != http.StatusNotFound || strings.Contains(recorder.Body.String(), "private output") {
		t.Fatalf("mismatched detail: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestActivityOutputBoundPreservesUTF8(t *testing.T) {
	output := boundedOutput(strings.Repeat("a", maxActivityOutputBytes-1) + "🙂")
	if len(output) > maxActivityOutputBytes || !utf8.ValidString(output) || len(output) != maxActivityOutputBytes-1 {
		t.Fatalf("invalid bounded output: %d", len(output))
	}
}
