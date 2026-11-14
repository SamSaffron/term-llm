package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/term-llm/term-llm/internal/formatutil"
)

// The optional label is presentation metadata for the conversation activity
// view. It must be declared in the real schema so a model can actually produce
// it, and it must never become job identity.
func TestQueueAgentLabelSchemaIsOptionalAndBounded(t *testing.T) {
	spec := NewQueueAgentTool(nil).Spec()
	properties, ok := spec.Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", spec.Schema["properties"])
	}
	label, ok := properties["label"].(map[string]any)
	if !ok {
		t.Fatalf("queue_agent schema has no label property: %#v", properties)
	}
	if label["type"] != "string" || label["maxLength"] != formatutil.MaxDisplayLabelRunes {
		t.Fatalf("label schema = %#v", label)
	}
	if description, _ := label["description"].(string); !strings.Contains(description, "Optional") {
		t.Fatalf("label description = %q", description)
	}
	required, _ := spec.Schema["required"].([]string)
	for _, name := range required {
		if name == "label" {
			t.Fatal("label must stay optional")
		}
	}
	if spec.Schema["additionalProperties"] != false {
		t.Fatal("queue_agent schema must keep rejecting undeclared arguments")
	}
}

func TestQueueAgentLabelNormalization(t *testing.T) {
	for _, test := range []struct{ name, label, want string }{
		{"empty", "", ""},
		{"blank", "   \t\n ", ""},
		{"trimmed and collapsed", "  Release   notes  ", "Release notes"},
		{"control characters dropped", "Release\x00\x07 notes", "Release notes"},
		{"single line", "Release\nnotes", "Release notes"},
		{"boundary runes", strings.Repeat("x", formatutil.MaxDisplayLabelRunes), strings.Repeat("x", formatutil.MaxDisplayLabelRunes)},
		{"over the bound", strings.Repeat("x", formatutil.MaxDisplayLabelRunes+1), ""},
		{"mention", "@worker: private instructions", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := formatutil.NormalizeDisplayLabel(test.label); got != test.want {
				t.Fatalf("formatutil.NormalizeDisplayLabel(%q) = %q, want %q", test.label, got, test.want)
			}
		})
	}
	if utf8.RuneCountInString(strings.Repeat("é", formatutil.MaxDisplayLabelRunes)) != formatutil.MaxDisplayLabelRunes {
		t.Fatal("label bound must count runes, not bytes")
	}
}

func TestQueueAgentRejectsUnusableLabelBeforeQueueing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		t.Errorf("unusable label must not reach the jobs server: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	tool := NewQueueAgentToolWithClient(&jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()}, nil)
	for _, label := range []string{strings.Repeat("x", formatutil.MaxDisplayLabelRunes+1), "@worker private"} {
		args, err := json.Marshal(QueueAgentArgs{AgentName: "developer", Prompt: "work", Label: label})
		if err != nil {
			t.Fatal(err)
		}
		output, err := tool.Execute(context.Background(), args)
		if err != nil || !output.IsError || !strings.Contains(output.Content, string(ErrInvalidParams)) || !strings.Contains(output.Content, "label") {
			t.Fatalf("label %q accepted: %+v %v", label, output, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("jobs server calls = %d", calls.Load())
	}
}

func TestQueueAgentLabelNeverChangesJobIdentity(t *testing.T) {
	var payload jobsV2AgentJobPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/jobs":
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			writeJSON(t, w, jobsV2AgentJobResponse{ID: "job_123"})
		case "/v2/jobs/job_123/trigger":
			writeJSON(t, w, jobsV2AgentRunResponse{ID: "run_123", JobID: "job_123", Status: "queued"})
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	tool := NewQueueAgentToolWithClient(&jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()}, nil)
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"agent_name":"developer","prompt":"work","cwd":"/tmp/work","label":"  Release   notes "}`))
	if err != nil || output.IsError {
		t.Fatalf("labelled queue_agent: %+v %v", output, err)
	}
	var result QueueAgentResult
	if err := json.Unmarshal([]byte(output.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.JobID != "job_123" || result.RunID != "run_123" || result.AgentName != "developer" {
		t.Fatalf("result = %+v", result)
	}
	if _, ok := payload.RunnerConfig["label"]; ok || strings.Contains(string(payload.Labels), "Release") {
		t.Fatalf("presentation label leaked into the job payload: %#v %s", payload.RunnerConfig, payload.Labels)
	}
	if strings.Contains(payload.RunnerConfig["instructions"].(string), "Release") {
		t.Fatal("presentation label leaked into the job instructions")
	}
}

// A job_ids wait pins one run; the persisted result must say which one so the
// activity view cannot expand to sibling or future runs.
func TestWaitForJobsByJobIDsRecordsPinnedRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/runs":
			writeJSON(t, w, jobsV2AgentRunsListResponse{Data: []jobsV2AgentRunResponse{{ID: "run_first", JobID: "job_1", Status: "running"}}})
		case "/v2/runs/run_first":
			writeJSON(t, w, jobsV2AgentRunResponse{ID: "run_first", JobID: "job_1", Status: "succeeded"})
		default:
			t.Fatalf("unexpected request: %s", r.URL.String())
		}
	}))
	defer server.Close()
	tool := NewWaitForJobsToolWithClient(&jobsBackedAgentClient{baseURL: server.URL, httpClient: server.Client()})
	output, err := tool.Execute(context.Background(), json.RawMessage(`{"job_ids":["job_1"]}`))
	if err != nil || output.IsError {
		t.Fatalf("wait_for_jobs: %+v %v", output, err)
	}
	var results []QueuedJobResult
	if err := json.Unmarshal([]byte(output.Content), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].JobID != "job_1" || results[0].RunID != "run_first" || results[0].Status != "succeeded" {
		t.Fatalf("job_ids result = %+v", results)
	}
}
