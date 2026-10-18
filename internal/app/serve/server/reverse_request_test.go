package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHubReverseRequestAllowsEncodedSlashInQuery(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/v1/sessions/s1/file-changes/diff" {
			t.Fatalf("backend path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("path"); got != "var/www/discourse/Gemfile.lock" {
			t.Fatalf("query path = %q", got)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	var frames []ReverseResponse
	HandleHubReverseRequest(
		context.Background(),
		ReverseRequest{
			ID:     "req_1",
			Method: http.MethodGet,
			Path:   "/chat/v1/sessions/s1/file-changes/diff?path=var%2Fwww%2Fdiscourse%2FGemfile.lock",
		},
		"node-token",
		backend.URL,
		"/chat",
		backend.Client(),
		func(resp ReverseResponse) error {
			frames = append(frames, resp)
			return nil
		},
	)

	if len(frames) == 0 {
		t.Fatal("no reverse response frames written")
	}
	if frames[0].Error != "" {
		t.Fatalf("unexpected reverse error: %s", frames[0].Error)
	}
	if frames[0].Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", frames[0].Status)
	}
}

// A reverse-connected node must not relay traversal, encoded separators or
// requests outside the configured node base path to its local HTTP listener.
func TestHubReverseRequestRejectsInvalidPaths(t *testing.T) {
	backendCalls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { backendCalls++ }))
	defer backend.Close()
	cases := []struct {
		path   string
		status int
	}{
		{"/chat/../admin", http.StatusBadRequest},
		{"/chat/%2fadmin", http.StatusBadRequest},
		{"/chat/%5cadmin", http.StatusBadRequest},
		{"/elsewhere", http.StatusForbidden},
		{"/chatty", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var frames []ReverseResponse
			HandleHubReverseRequest(context.Background(), ReverseRequest{ID: "req", Method: http.MethodGet, Path: tc.path}, "secret", backend.URL, "/chat", backend.Client(), func(frame ReverseResponse) error {
				frames = append(frames, frame)
				return nil
			})
			if len(frames) != 1 || frames[0].Status != tc.status || backendCalls != 0 {
				t.Fatalf("frames=%+v backend calls=%d; want status %d and no backend call", frames, backendCalls, tc.status)
			}
		})
	}
}
