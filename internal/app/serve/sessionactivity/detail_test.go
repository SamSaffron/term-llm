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
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
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

// A run detail must take its title from the reference for that run's own job,
// never from whichever job-wide reference happened to come first.
func TestActivityRunDetailUsesTheMatchingJobReference(t *testing.T) {
	store := activityStore(t)
	persistLegacyJobLabel(t, store, "parent", "job_alpha", "Alpha report")
	persistLegacyJobLabel(t, store, "parent", "job_beta", "Beta report")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/runs/run_beta":
			writeTestJSON(w, map[string]string{"id": "run_beta", "job_id": "job_beta", "status": "succeeded", "response": "beta output"})
		case "/v2/runs/run_other":
			writeTestJSON(w, map[string]string{"id": "run_other", "job_id": "job_other", "status": "succeeded", "response": "other output"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	t.Setenv("TERM_LLM_JOBS_SERVER", backend.URL)
	handler := Handler{Store: store, Children: noChildren}
	recorder := httptest.NewRecorder()
	handler.ServeRun(recorder, httptest.NewRequest(http.MethodGet, "/activity", nil), "parent", "run_beta")
	if recorder.Code != http.StatusOK {
		t.Fatalf("detail status %d: %s", recorder.Code, recorder.Body.String())
	}
	var detail ActivityJobDetail
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.JobID != "job_beta" || detail.Title != "Beta report" {
		t.Fatalf("detail reference = %+v", detail.ActivityJobRun)
	}
	denied := httptest.NewRecorder()
	handler.ServeRun(denied, httptest.NewRequest(http.MethodGet, "/activity", nil), "parent", "run_other")
	if denied.Code != http.StatusNotFound || strings.Contains(denied.Body.String(), "other output") {
		t.Fatalf("out-of-scope detail = %d %s", denied.Code, denied.Body.String())
	}
}

func TestActivityOutputBoundRetainsInteriorInvalidBytes(t *testing.T) {
	input := strings.Repeat("a", 10) + "\xff" + strings.Repeat("b", maxActivityOutputBytes+10)
	output := boundedOutput(input)
	if len(output) != maxActivityOutputBytes || output != input[:maxActivityOutputBytes] {
		t.Fatalf("bounded output = %d bytes, want %d", len(output), maxActivityOutputBytes)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	var decoded string
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("a", 10) + "\uFFFD" + strings.Repeat("b", maxActivityOutputBytes-11)
	if decoded != want {
		t.Fatalf("JSON replacement dropped output: %d bytes", len(decoded))
	}
}

func TestActivityOutputBoundCutsOnlyPartialRunes(t *testing.T) {
	for _, test := range []struct {
		name, suffix string
		keep         int
	}{
		{"one byte of a four-byte rune", "🙂", 1},
		{"two bytes of a four-byte rune", "🙂", 2},
		{"three bytes of a four-byte rune", "🙂", 3},
		{"one byte of a three-byte rune", "€", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefix := maxActivityOutputBytes - test.keep
			output := boundedOutput(strings.Repeat("a", prefix) + test.suffix + "tail")
			if len(output) != prefix || output != strings.Repeat("a", prefix) || !utf8.ValidString(output) {
				t.Fatalf("bounded output = %d bytes, want %d valid rune boundary", len(output), prefix)
			}
		})
	}
}

// Legacy job-wide references carry their label on the queue_agent call.
func persistLegacyJobLabel(t *testing.T, store session.Store, id, job, label string) {
	t.Helper()
	callID := "legacy-" + job
	output, _ := json.Marshal(tools.QueueAgentResult{JobID: job, AgentName: "developer"})
	arguments, _ := json.Marshal(map[string]string{"agent_name": "developer", "prompt": "work", "label": label})
	for _, message := range []core.Message{
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: callID, Name: tools.QueueAgentToolName, Arguments: arguments}}}},
		core.ToolResultMessage(callID, tools.QueueAgentToolName, string(output), nil),
	} {
		if err := store.AddMessage(context.Background(), id, session.NewMessage(id, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
}
