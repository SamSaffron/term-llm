package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestNormalizeJSONPayload_YAML(t *testing.T) {
	input := []byte("name: nightly\ntrigger_type: manual\nrunner_config:\n  command: echo\n")
	out, err := normalizeJSONPayload(input)
	if err != nil {
		t.Fatalf("normalizeJSONPayload failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}
	if decoded["name"] != "nightly" {
		t.Fatalf("name = %v, want nightly", decoded["name"])
	}
}

func TestReadPayload_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.yaml")
	if err := os.WriteFile(path, []byte("name: test-job\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	out, err := readPayload(path, "")
	if err != nil {
		t.Fatalf("readPayload failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}
	if decoded["name"] != "test-job" {
		t.Fatalf("name = %v, want test-job", decoded["name"])
	}
}

func TestJobsClientResolveJobID_ByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/jobs" {
			t.Fatalf("path = %s, want /v2/jobs", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"job_123","name":"nightly"}]}`))
	}))
	defer srv.Close()

	c := &jobsClient{baseURL: srv.URL, http: srv.Client()}
	id, err := c.resolveJobID(context.Background(), "nightly")
	if err != nil {
		t.Fatalf("resolveJobID failed: %v", err)
	}
	if id != "job_123" {
		t.Fatalf("id = %s, want job_123", id)
	}
}

func TestJobsClientDo_ParsesOpenAIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	c := &jobsClient{baseURL: srv.URL, http: srv.Client()}
	err := c.do(context.Background(), http.MethodGet, "/v2/jobs", nil, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	if err.Error() != "bad request" {
		t.Fatalf("err = %q, want bad request", err.Error())
	}
}

func TestJobsCommandsRejectExtraArgs(t *testing.T) {
	if err := jobsCmd.Args(jobsCmd, []string{"running"}); err == nil {
		t.Fatalf("expected jobs command to reject extra args")
	}
	if err := jobsListCmd.Args(jobsListCmd, []string{"unexpected"}); err == nil {
		t.Fatalf("expected jobs list command to reject extra args")
	}
}

func TestRunJobsActive_JSONFiltersActiveRunsAndUsesPagedRequests(t *testing.T) {
	requests := make([]string, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v2/jobs":
			_, _ = w.Write([]byte(`{"data":[{"id":"job_alpha","name":"alpha"},{"id":"job_beta","name":"beta"}]}`))
		case "/v2/runs":
			jobID := r.URL.Query().Get("job_id")
			offset := r.URL.Query().Get("offset")
			limit := r.URL.Query().Get("limit")
			if limit != "10" {
				t.Fatalf("limit = %s, want 10", limit)
			}
			switch {
			case jobID == "job_alpha" && offset == "0":
				_, _ = w.Write([]byte(`{"data":[
					{"id":"run_running","job_id":"job_alpha","status":"running","started_at":"2026-02-27T15:04:05Z","scheduled_for":"2026-02-27T15:00:00Z","worker_id":"worker-1"},
					{"id":"run_done","job_id":"job_alpha","status":"succeeded","scheduled_for":"2026-02-27T14:00:00Z"}
				]}`))
			case jobID == "job_alpha" && offset == "10":
				_, _ = w.Write([]byte(`{"data":[
					{"id":"run_queued","job_id":"job_alpha","status":"queued","scheduled_for":"2026-02-27T13:00:00Z"}
				]}`))
			case jobID == "job_alpha" && offset == "20":
				_, _ = w.Write([]byte(`{"data":[]}`))
			case jobID == "job_beta" && offset == "0":
				_, _ = w.Write([]byte(`{"data":[
					{"id":"run_failed","job_id":"job_beta","status":"failed","scheduled_for":"2026-02-27T12:00:00Z"}
				]}`))
			default:
				t.Fatalf("unexpected runs request: %s", r.URL.RequestURI())
			}
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	oldServerURL := jobsServerURL
	oldToken := jobsToken
	oldTimeout := jobsTimeout
	oldJSON := jobsJSON
	jobsServerURL = srv.URL
	jobsToken = ""
	jobsTimeout = 2 * time.Second
	jobsJSON = true
	t.Cleanup(func() {
		jobsServerURL = oldServerURL
		jobsToken = oldToken
		jobsTimeout = oldTimeout
		jobsJSON = oldJSON
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	var runErr error
	output := captureStdout(t, func() {
		runErr = runJobsActive(cmd, nil)
	})
	if runErr != nil {
		t.Fatalf("runJobsActive failed: %v", runErr)
	}

	var runs []jobsActiveRun
	if err := json.Unmarshal([]byte(output), &runs); err != nil {
		t.Fatalf("decode output: %v\noutput=%s", err, output)
	}
	if len(runs) != 2 {
		t.Fatalf("len(runs) = %d, want 2", len(runs))
	}
	if runs[0].RunID != "run_running" || runs[0].Status != jobsV2RunRunning || runs[0].JobName != "alpha" {
		t.Fatalf("unexpected first active run: %+v", runs[0])
	}
	if runs[1].RunID != "run_queued" || runs[1].Status != jobsV2RunQueued || runs[1].JobName != "alpha" {
		t.Fatalf("unexpected second active run: %+v", runs[1])
	}

	expectedRequests := []string{
		"/v2/jobs?limit=500",
		"/v2/runs?limit=10&offset=0&job_id=job_alpha",
		"/v2/runs?limit=10&offset=10&job_id=job_alpha",
		"/v2/runs?limit=10&offset=20&job_id=job_alpha",
		"/v2/runs?limit=10&offset=0&job_id=job_beta",
	}
	if !reflect.DeepEqual(requests, expectedRequests) {
		t.Fatalf("requests = %#v, want %#v", requests, expectedRequests)
	}
}

func TestRunJobsActive_TableOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/jobs":
			_, _ = w.Write([]byte(`{"data":[{"id":"job_alpha","name":"alpha"}]}`))
		case "/v2/runs":
			offset := r.URL.Query().Get("offset")
			if offset == "0" {
				_, _ = w.Write([]byte(`{"data":[{"id":"run_queued","job_id":"job_alpha","status":"queued","scheduled_for":"2026-02-27T13:00:00Z"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	oldServerURL := jobsServerURL
	oldToken := jobsToken
	oldTimeout := jobsTimeout
	oldJSON := jobsJSON
	jobsServerURL = srv.URL
	jobsToken = ""
	jobsTimeout = 2 * time.Second
	jobsJSON = false
	t.Cleanup(func() {
		jobsServerURL = oldServerURL
		jobsToken = oldToken
		jobsTimeout = oldTimeout
		jobsJSON = oldJSON
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	var runErr error
	output := captureStdout(t, func() {
		runErr = runJobsActive(cmd, nil)
	})
	if runErr != nil {
		t.Fatalf("runJobsActive failed: %v", runErr)
	}
	if !strings.Contains(output, "JOB_ID") || !strings.Contains(output, "RUN_ID") {
		t.Fatalf("expected table header in output, got: %s", output)
	}
	if !strings.Contains(output, "run_queued") || !strings.Contains(output, "job_alpha") {
		t.Fatalf("expected row in output, got: %s", output)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return string(out)
}
