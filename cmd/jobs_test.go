package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
