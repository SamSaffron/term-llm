package sessionactivity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
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
