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

func TestRelativeTime(t *testing.T) {
	now := time.Now()
	cases := []struct {
		input time.Time
		check func(string) bool
		desc  string
	}{
		{now.Add(-30 * time.Second), func(s string) bool { return s == "just now" }, "30s ago → just now"},
		{now.Add(-5 * time.Minute), func(s string) bool { return s == "5m ago" }, "5m ago"},
		{now.Add(-2 * time.Hour), func(s string) bool { return s == "2h ago" }, "2h ago"},
		{now.Add(-3 * 24 * time.Hour), func(s string) bool { return s == "3d ago" }, "3d ago"},
		{now.Add(-10 * 24 * time.Hour), func(s string) bool { return strings.Contains(s, " ") && !strings.Contains(s, "ago") }, ">7d → date"},
	}
	for _, c := range cases {
		got := relativeTime(c.input)
		if !c.check(got) {
			t.Errorf("%s: got %q", c.desc, got)
		}
	}
}

func TestShortRunStatus(t *testing.T) {
	cases := []struct {
		status jobsV2RunStatus
		want   string
	}{
		{jobsV2RunSucceeded, "ok"},
		{jobsV2RunFailed, "FAIL"},
		{jobsV2RunCancelled, "cancelled"},
		{jobsV2RunTimedOut, "timeout"},
		{jobsV2RunSkipped, "skipped"},
		{jobsV2RunRunning, "running"},
	}
	for _, c := range cases {
		got := shortRunStatus(c.status)
		if got != c.want {
			t.Errorf("shortRunStatus(%q) = %q, want %q", c.status, got, c.want)
		}
	}
}

// jobsListTestServer creates an httptest server serving the given jobs and runs JSON payloads.
func jobsListTestServer(t *testing.T, jobsPayload, runsPayload string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/jobs":
			_, _ = w.Write([]byte(jobsPayload))
		case "/v2/runs":
			_, _ = w.Write([]byte(runsPayload))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func runJobsListHelper(t *testing.T, srv *httptest.Server, all bool) string {
	t.Helper()
	oldServerURL := jobsServerURL
	oldToken := jobsToken
	oldTimeout := jobsTimeout
	oldJSON := jobsJSON
	oldAll := jobsListAll
	jobsServerURL = srv.URL
	jobsToken = ""
	jobsTimeout = 2 * time.Second
	jobsJSON = false
	jobsListAll = all
	t.Cleanup(func() {
		jobsServerURL = oldServerURL
		jobsToken = oldToken
		jobsTimeout = oldTimeout
		jobsJSON = oldJSON
		jobsListAll = oldAll
	})

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	var runErr error
	output := captureStdout(t, func() {
		runErr = runJobsList(cmd, nil)
	})
	if runErr != nil {
		t.Fatalf("runJobsList failed: %v", runErr)
	}
	return output
}

// TestRunJobsList_FiltersEphemeralJobs verifies that fired once-off and finished agent jobs
// are hidden by default and visible with --all.
func TestRunJobsList_FiltersEphemeralJobs(t *testing.T) {
	// Jobs:
	//   job_cron        – cron job, enabled → always visible
	//   job_once_recent – once job fired 2h ago (within grace) → still visible by default
	//   job_once_old    – once job fired 8h ago (past grace) → hidden by default
	//   job_agent       – manual+llm with completed run → hidden by default
	//   job_manual      – manual+program with completed run → always visible
	recentUpdatedAt := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	oldUpdatedAt := time.Now().Add(-8 * time.Hour).UTC().Format(time.RFC3339)
	jobsPayload := `{"data":[
		{"id":"job_cron",        "name":"my-cron",     "enabled":true,  "trigger_type":"cron",   "runner_type":"program", "updated_at":"2026-02-28T10:00:00Z"},
		{"id":"job_once_recent", "name":"fresh-once",  "enabled":false, "trigger_type":"once",   "runner_type":"program", "updated_at":"` + recentUpdatedAt + `"},
		{"id":"job_once_old",    "name":"stale-once",  "enabled":false, "trigger_type":"once",   "runner_type":"program", "updated_at":"` + oldUpdatedAt + `"},
		{"id":"job_agent",       "name":"sub-agent",   "enabled":true,  "trigger_type":"manual", "runner_type":"llm",     "updated_at":"2026-02-28T10:00:00Z"},
		{"id":"job_manual",      "name":"run-me",      "enabled":true,  "trigger_type":"manual", "runner_type":"program", "updated_at":"2026-02-28T10:00:00Z"}
	]}`
	runsPayload := `{"data":[
		{"id":"run_1","job_id":"job_cron",        "status":"succeeded","scheduled_for":"2026-02-28T10:00:00Z","finished_at":"2026-02-28T10:01:00Z"},
		{"id":"run_2","job_id":"job_once_recent", "status":"succeeded","scheduled_for":"2026-02-28T08:00:00Z","finished_at":"2026-02-28T08:01:00Z"},
		{"id":"run_3","job_id":"job_once_old",    "status":"succeeded","scheduled_for":"2026-02-27T08:00:00Z","finished_at":"2026-02-27T08:01:00Z"},
		{"id":"run_4","job_id":"job_agent",       "status":"succeeded","scheduled_for":"2026-02-27T09:00:00Z","finished_at":"2026-02-27T09:30:00Z"},
		{"id":"run_5","job_id":"job_manual",      "status":"succeeded","scheduled_for":"2026-02-27T11:00:00Z","finished_at":"2026-02-27T11:01:00Z"}
	]}`

	srv := jobsListTestServer(t, jobsPayload, runsPayload)
	defer srv.Close()

	// Default view
	out := runJobsListHelper(t, srv, false)
	if !strings.Contains(out, "fresh-once") {
		t.Errorf("default list should show once-off job fired within 6h; got:\n%s", out)
	}
	if strings.Contains(out, "stale-once") {
		t.Errorf("default list should hide once-off job fired >6h ago; got:\n%s", out)
	}
	if strings.Contains(out, "sub-agent") {
		t.Errorf("default list should hide finished agent job; got:\n%s", out)
	}
	if !strings.Contains(out, "my-cron") {
		t.Errorf("default list should show cron job; got:\n%s", out)
	}
	if !strings.Contains(out, "run-me") {
		t.Errorf("default list should show manual+program job; got:\n%s", out)
	}
	if !strings.Contains(out, "hidden") {
		t.Errorf("expected hidden-count footer in output; got:\n%s", out)
	}

	// --all: everything visible
	outAll := runJobsListHelper(t, srv, true)
	if !strings.Contains(outAll, "stale-once") {
		t.Errorf("--all should show old once-off job; got:\n%s", outAll)
	}
	if !strings.Contains(outAll, "sub-agent") {
		t.Errorf("--all should show agent job; got:\n%s", outAll)
	}
}

// TestRunJobsList_NewTableFormat verifies the table uses the new column layout.
func TestRunJobsList_NewTableFormat(t *testing.T) {
	jobsPayload := `{"data":[
		{"id":"job_cron","name":"heartbeat","enabled":true,"trigger_type":"cron","runner_type":"program",
		 "next_run_at":"2026-03-02T04:00:00Z"}
	]}`
	finishedAt := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	runsPayload := `{"data":[
		{"id":"run_1","job_id":"job_cron","status":"succeeded","scheduled_for":"2026-03-01T02:00:00Z","finished_at":"` + finishedAt + `"}
	]}`

	srv := jobsListTestServer(t, jobsPayload, runsPayload)
	defer srv.Close()

	out := runJobsListHelper(t, srv, false)

	if !strings.Contains(out, "NAME") || !strings.Contains(out, "TRIGGER") ||
		!strings.Contains(out, "STATUS") || !strings.Contains(out, "LAST_RUN") || !strings.Contains(out, "NEXT_RUN") {
		t.Errorf("expected new column headers; got:\n%s", out)
	}
	if !strings.Contains(out, "heartbeat") {
		t.Errorf("expected job name in output; got:\n%s", out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("expected 'ok' status in LAST_RUN column; got:\n%s", out)
	}
	if !strings.Contains(out, "ago") {
		t.Errorf("expected relative time in LAST_RUN column; got:\n%s", out)
	}
}

// TestRunJobsList_ActiveRunStatus verifies that a job with an active run shows its run status.
func TestRunJobsList_ActiveRunStatus(t *testing.T) {
	jobsPayload := `{"data":[
		{"id":"job_1","name":"digester","enabled":true,"trigger_type":"cron","runner_type":"program"}
	]}`
	startedAt := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	runsPayload := `{"data":[
		{"id":"run_1","job_id":"job_1","status":"running","scheduled_for":"2026-03-01T10:00:00Z","started_at":"` + startedAt + `"}
	]}`

	srv := jobsListTestServer(t, jobsPayload, runsPayload)
	defer srv.Close()

	out := runJobsListHelper(t, srv, false)
	if !strings.Contains(out, "running") {
		t.Errorf("expected STATUS=running for active job; got:\n%s", out)
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
