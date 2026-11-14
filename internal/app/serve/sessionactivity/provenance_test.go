package sessionactivity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/tools"
)

// Exercise the real tool result and ordinary storage, not the lossy web-message
// projection (successful tool-result bodies intentionally aren't shown there).
func TestActivityQueueAgentProvenanceSurvivesStorage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/jobs":
			writeTestJSON(w, map[string]string{"id": "job_exact"})
		case "/v2/jobs/job_exact/trigger":
			writeTestJSON(w, map[string]string{"id": "run_exact", "job_id": "job_exact", "status": "queued"})
		default:
			t.Errorf("unexpected tool request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	t.Setenv("TERM_LLM_JOBS_SERVER", backend.URL)
	t.Setenv("TERM_LLM_JOBS_TOKEN", "")
	ctx := context.Background()
	args := json.RawMessage(`{"agent_name":"developer","prompt":"Check activity persistence","label":"Release notes","cwd":"` + filepath.ToSlash(t.TempDir()) + `"}`)
	// The label path is only real when the tool schema declares the argument the
	// model has to produce, so assert that before exercising the tool.
	properties, _ := tools.NewQueueAgentTool(nil).Spec().Schema["properties"].(map[string]any)
	var declared map[string]any
	if err := json.Unmarshal(args, &declared); err != nil {
		t.Fatal(err)
	}
	for name := range declared {
		if _, ok := properties[name]; !ok {
			t.Fatalf("queue_agent schema does not declare %q: %#v", name, properties)
		}
	}
	output, err := tools.NewQueueAgentTool(nil).Execute(ctx, args)
	if err != nil || output.IsError {
		t.Fatalf("queue_agent: %+v, %v", output, err)
	}
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, &session.Session{ID: "parent", Provider: "debug", Model: "test", Mode: session.ModeChat}); err != nil {
		t.Fatal(err)
	}
	call := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "queue-call", Name: tools.QueueAgentToolName, Arguments: args}}}}
	for _, message := range []core.Message{call, core.ToolResultMessage("queue-call", tools.QueueAgentToolName, output.Content, nil)} {
		if err := store.AddMessage(ctx, "parent", session.NewMessage("parent", message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = session.NewSQLiteStore(session.Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	messages, err := store.GetMessages(ctx, "parent", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := jobReferences(messages)
	if len(refs) != 1 || refs[0].JobID != "job_exact" || refs[0].RunID != "run_exact" || refs[0].Title != "Release notes" {
		t.Fatalf("persisted exact provenance = %+v", refs)
	}
}

func TestActivityQueuedLabelsArePresentationOnly(t *testing.T) {
	for _, value := range []struct{ args, want string }{
		{`{"label":"Release notes"}`, "Release notes"},
		{`{"label":"  Release   notes  "}`, "Release notes"},
		{`{"prompt":"Never show the raw task prompt"}`, ""},
		{`{"label":"@worker: private instructions"}`, ""},
		{`{"label":"Release\u0000\u0007 notes"}`, "Release notes"},
		{`{"label":"` + strings.Repeat("x", 76) + `"}`, strings.Repeat("x", 76)},
		{`{"label":"` + strings.Repeat("x", 77) + `"}`, ""},
	} {
		if got := queuedTaskLabel(json.RawMessage(value.args)); got != value.want {
			t.Fatalf("label %q = %q, want %q", value.args, got, value.want)
		}
	}
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestActivityProvenanceRejectsTextArgumentsAndUnpairedResults(t *testing.T) {
	queueCall := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "queue", Name: tools.QueueAgentToolName}}}}
	waitCall := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "wait", Name: tools.WaitForJobsToolName, Arguments: json.RawMessage(`{"job_ids":["job_private"],"run_ids":["run_private"]}`)}}}}
	result := func(id, name, content string, failed bool) core.Message {
		message := core.ToolResultMessage(id, name, content, nil)
		message.Parts[0].ToolResult.IsError = failed
		return message
	}
	for _, test := range []struct {
		name     string
		messages []core.Message
		want     int
	}{
		{"text mentions", []core.Message{core.UserText(`queue_agent {"job_id":"job_private","run_id":"run_private"}`)}, 0},
		{"wait arguments only", []core.Message{waitCall}, 0},
		{"unpaired result", []core.Message{result("unknown", tools.QueueAgentToolName, `{"job_id":"job_private","run_id":"run_private"}`, false)}, 0},
		{"failed result", []core.Message{queueCall, result("queue", tools.QueueAgentToolName, `{"job_id":"job_private","run_id":"run_private"}`, true)}, 0},
		{"mismatched tool", []core.Message{queueCall, result("queue", "shell", `{"job_id":"job_private","run_id":"run_private"}`, false)}, 0},
		{"unsafe id", []core.Message{queueCall, result("queue", tools.QueueAgentToolName, `{"job_id":"../secrets","run_id":"run_private"}`, false)}, 0},
		{"paired anonymous result", []core.Message{queueCall, result("queue", "", `{"job_id":"job_exact","run_id":"run_exact"}`, false)}, 1},
		{"wait exact result", []core.Message{waitCall, result("wait", tools.WaitForJobsToolName, `[{"job_id":"job_exact","run_id":"run_exact","status":"succeeded"}]`, false)}, 1},
		{"notification", []core.Message{core.JobNotificationText("untrusted display text", core.JobNotificationProvenance{JobID: "job_exact", RunID: "run_exact", Agent: "reviewer", Status: "succeeded"})}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages := make([]session.Message, 0, len(test.messages))
			for _, message := range test.messages {
				messages = append(messages, *session.NewMessage("parent", message, -1))
			}
			refs := jobReferences(messages)
			if len(refs) != test.want {
				t.Fatalf("references = %+v, want %d", refs, test.want)
			}
			if test.want > 0 && (refs[0].JobID != "job_exact" || refs[0].RunID != "run_exact") {
				t.Fatalf("references = %+v", refs)
			}
		})
	}
}

// A wait result that pins one run establishes that exact run. A legacy run-less
// wait result establishes nothing: it cannot add a job, and it cannot widen an
// exact queue_agent or notification reference to the job's other runs.
func TestActivityWaitResultsOnlyEstablishTheirOwnRun(t *testing.T) {
	call := func(id, name, args string) core.Message {
		return core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}}}}
	}
	result := func(id, name, content string) core.Message {
		return core.ToolResultMessage(id, name, content, nil)
	}
	queueCall := call("queue", tools.QueueAgentToolName, `{"agent_name":"developer","prompt":"work"}`)
	waitCall := call("wait", tools.WaitForJobsToolName, `{"job_ids":["job_cron"]}`)
	legacyWait := result("wait", tools.WaitForJobsToolName, `[{"job_id":"job_cron","status":"succeeded"}]`)
	exactWait := result("wait", tools.WaitForJobsToolName, `[{"job_id":"job_cron","run_id":"run_observed","status":"succeeded"}]`)
	queueRun := result("queue", tools.QueueAgentToolName, `{"job_id":"job_cron","run_id":"run_queue","agent_name":"developer"}`)
	legacyQueue := result("queue", tools.QueueAgentToolName, `{"job_id":"job_cron","agent_name":"developer"}`)
	notification := core.JobNotificationText("Queued job job_cron (developer) succeeded", core.JobNotificationProvenance{JobID: "job_cron", Status: "succeeded"})
	for _, test := range []struct {
		name     string
		messages []core.Message
		want     []string
	}{
		{"legacy wait alone", []core.Message{waitCall, legacyWait}, nil},
		{"exact wait", []core.Message{waitCall, exactWait}, []string{"job_cron:run_observed"}},
		{"exact queue then legacy wait", []core.Message{queueCall, queueRun, waitCall, legacyWait}, []string{"job_cron:run_queue"}},
		{"legacy wait then exact queue", []core.Message{waitCall, legacyWait, queueCall, queueRun}, []string{"job_cron:run_queue"}},
		{"notification authority", []core.Message{notification, waitCall, legacyWait}, []string{"job_cron:"}},
		{"legacy job-only queue", []core.Message{queueCall, legacyQueue}, []string{"job_cron:"}},
		{"unrelated legacy wait", []core.Message{waitCall, legacyWait, call("other", tools.QueueAgentToolName, `{}`), result("other", tools.QueueAgentToolName, `{"job_id":"job_other","run_id":"run_other"}`)}, []string{"job_other:run_other"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages := make([]session.Message, 0, len(test.messages))
			for _, message := range test.messages {
				messages = append(messages, *session.NewMessage("parent", message, -1))
			}
			var got []string
			for _, ref := range jobReferences(messages) {
				got = append(got, ref.JobID+":"+ref.RunID)
			}
			want := append([]string(nil), test.want...)
			sort.Strings(got)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("references = %v, want %v", got, want)
			}
		})
	}
}

// The real wait_for_jobs tool, called with job_ids, must persist the run it
// pinned so activity cannot list or serve sibling and future runs of the job.
func TestActivityWaitByJobIDsDoesNotExpandToSiblingRuns(t *testing.T) {
	run := func(id, status, response string) map[string]any {
		return map[string]any{"id": id, "job_id": "job_cron", "status": status, "started_at": "2026-11-01T00:00:00Z", "finished_at": "2026-11-01T00:01:00Z", "response": response}
	}
	observed := run("run_observed", "succeeded", "observed output")
	sibling := run("run_sibling", "succeeded", "SIBLING PRIVATE OUTPUT")
	future := run("run_future", "running", "")
	byID := map[string]map[string]any{"run_observed": observed, "run_sibling": sibling, "run_future": future}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/runs":
			if r.URL.Query().Get("limit") == "1" {
				writeTestJSON(w, map[string]any{"data": []any{observed}, "total": 3})
				return
			}
			writeTestJSON(w, map[string]any{"data": []any{observed, sibling, future}, "total": 3})
		case strings.HasPrefix(r.URL.Path, "/v2/runs/"):
			value, ok := byID[strings.Split(strings.TrimPrefix(r.URL.Path, "/v2/runs/"), "/")[0]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeTestJSON(w, value)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()
	t.Setenv("TERM_LLM_JOBS_SERVER", backend.URL)
	t.Setenv("TERM_LLM_JOBS_TOKEN", "")
	ctx := context.Background()
	args := json.RawMessage(`{"job_ids":["job_cron"]}`)
	output, err := tools.NewWaitForJobsTool().Execute(ctx, args)
	if err != nil || output.IsError {
		t.Fatalf("wait_for_jobs: %+v %v", output, err)
	}
	if !strings.Contains(output.Content, `"run_id":"run_observed"`) {
		t.Fatalf("wait result lost its pinned run: %s", output.Content)
	}
	store := activityStore(t)
	call := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "wait-1", Name: tools.WaitForJobsToolName, Arguments: args}}}}
	for _, message := range []core.Message{call, core.ToolResultMessage("wait-1", tools.WaitForJobsToolName, output.Content, nil)} {
		if err := store.AddMessage(ctx, "parent", session.NewMessage("parent", message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	handler := Handler{Store: store, Children: noChildren}
	snapshot, recorder := requestActivity(t, handler, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].RunID != "run_observed" || snapshot.Jobs[0].JobID != "job_cron" || snapshot.JobsPartial {
		t.Fatalf("activity scope = %+v", snapshot)
	}
	if strings.Contains(recorder.Body.String(), "run_sibling") || strings.Contains(recorder.Body.String(), "run_future") {
		t.Fatalf("sibling/future runs leaked: %s", recorder.Body.String())
	}
	for _, id := range []string{"run_sibling", "run_future"} {
		denied := httptest.NewRecorder()
		handler.ServeRun(denied, httptest.NewRequest(http.MethodGet, "/activity", nil), "parent", id)
		if denied.Code != http.StatusNotFound || strings.Contains(denied.Body.String(), "SIBLING PRIVATE OUTPUT") {
			t.Fatalf("%s detail = %d %s", id, denied.Code, denied.Body.String())
		}
	}
	allowed := httptest.NewRecorder()
	handler.ServeRun(allowed, httptest.NewRequest(http.MethodGet, "/activity", nil), "parent", "run_observed")
	if allowed.Code != http.StatusOK || !strings.Contains(allowed.Body.String(), "observed output") {
		t.Fatalf("pinned run detail = %d %s", allowed.Code, allowed.Body.String())
	}
}

func noChildren(context.Context, string) ([]ChildRunProjection, bool, error) {
	return []ChildRunProjection{}, false, nil
}

// A legacy run-less wait result must not widen an exact queue_agent result or a
// structured notification to the job's sibling runs, in either message order.
func TestActivityLegacyWaitDoesNotWidenExactScope(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/runs/run_observed":
			writeTestJSON(w, map[string]string{"id": "run_observed", "job_id": "job_cron", "status": "succeeded", "response": "observed output"})
		case "/v2/runs/run_sibling":
			writeTestJSON(w, map[string]string{"id": "run_sibling", "job_id": "job_cron", "status": "succeeded", "response": "SIBLING PRIVATE OUTPUT"})
		default:
			writeTestJSON(w, map[string]any{"data": []any{
				map[string]string{"id": "run_observed", "job_id": "job_cron", "status": "succeeded"},
				map[string]string{"id": "run_sibling", "job_id": "job_cron", "status": "succeeded"},
			}, "total": 2})
		}
	}))
	defer backend.Close()
	t.Setenv("TERM_LLM_JOBS_SERVER", backend.URL)
	t.Setenv("TERM_LLM_JOBS_TOKEN", "")
	for _, order := range []string{"exact queue then legacy wait", "legacy wait then exact queue"} {
		t.Run(order, func(t *testing.T) {
			store := activityStore(t)
			if order == "exact queue then legacy wait" {
				persistJobReference(t, store, "parent", "job_cron", "run_observed")
				persistLegacyWaitResult(t, store, "parent", "wait-1", "job_cron")
			} else {
				persistLegacyWaitResult(t, store, "parent", "wait-1", "job_cron")
				persistJobReference(t, store, "parent", "job_cron", "run_observed")
			}
			assertExactActivityScope(t, store)
		})
	}
	t.Run("exact notification then legacy wait", func(t *testing.T) {
		store := activityStore(t)
		notice := core.JobNotificationText("Queued job job_cron (developer) succeeded", core.JobNotificationProvenance{JobID: "job_cron", RunID: "run_observed", Agent: "developer", Status: "succeeded"})
		if err := store.AddMessage(context.Background(), "parent", session.NewMessage("parent", notice, -1)); err != nil {
			t.Fatal(err)
		}
		persistLegacyWaitResult(t, store, "parent", "wait-1", "job_cron")
		assertExactActivityScope(t, store)
	})
}

func assertExactActivityScope(t *testing.T, store session.Store) {
	t.Helper()
	handler := Handler{Store: store, Children: noChildren}
	snapshot, recorder := requestActivity(t, handler, httptest.NewRequest(http.MethodGet, "/v1/sessions/parent/activity", nil))
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].RunID != "run_observed" || snapshot.JobsPartial {
		t.Fatalf("activity widened to sibling runs: %+v", snapshot)
	}
	if strings.Contains(recorder.Body.String(), "run_sibling") {
		t.Fatalf("sibling run leaked: %s", recorder.Body.String())
	}
	denied := httptest.NewRecorder()
	handler.ServeRun(denied, httptest.NewRequest(http.MethodGet, "/activity", nil), "parent", "run_sibling")
	if denied.Code != http.StatusNotFound || strings.Contains(denied.Body.String(), "SIBLING PRIVATE OUTPUT") {
		t.Fatalf("sibling detail = %d %s", denied.Code, denied.Body.String())
	}
}

// Legacy persisted wait results carry only a job id.
func persistLegacyWaitResult(t *testing.T, store session.Store, id, callID, jobID string) {
	t.Helper()
	arguments, _ := json.Marshal(map[string][]string{"job_ids": {jobID}})
	content, _ := json.Marshal([]tools.QueuedJobResult{{JobID: jobID, Status: "succeeded"}})
	for _, message := range []core.Message{
		{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: callID, Name: tools.WaitForJobsToolName, Arguments: arguments}}}},
		core.ToolResultMessage(callID, tools.WaitForJobsToolName, string(content), nil),
	} {
		if err := store.AddMessage(context.Background(), id, session.NewMessage(id, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
}
