package share

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/agents/gist"
)

type fakeGistClient struct {
	gist        *gist.Gist
	createErr   error
	getErr      error
	updateErr   error
	createCalls int
	getCalls    int
	updateCalls int
}

func (f *fakeGistClient) Create(string, bool, map[string]string) (*gist.Gist, error) {
	f.createCalls++
	return f.gist, f.createErr
}

func (f *fakeGistClient) Get(string) (*gist.Gist, error) {
	f.getCalls++
	return f.gist, f.getErr
}

func (f *fakeGistClient) Update(string, map[string]string) error {
	f.updateCalls++
	return f.updateErr
}

func TestGitHubPublisherCapabilities(t *testing.T) {
	capabilities, err := NewGitHubPublisher().Capabilities(context.Background())
	if err != nil || ValidateCapabilities(capabilities) != nil {
		t.Fatalf("capabilities=%+v error=%v", capabilities, err)
	}
	if capabilities.Provider.ID != "github" || !capabilities.Supports(OperationUpdate) || capabilities.SupportsVisibility(VisibilityPrivate) {
		t.Fatalf("capabilities=%+v", capabilities)
	}
}

func TestGitHubPublisherReadinessProbe(t *testing.T) {
	for _, test := range []struct {
		name         string
		statuses     []int
		wantReady    bool
		wantRequests int
	}{
		{name: "becomes ready", statuses: []int{http.StatusNotFound, http.StatusOK}, wantReady: true, wantRequests: 2},
		{name: "forbidden stops", statuses: []int{http.StatusForbidden, http.StatusOK}, wantRequests: 1},
		{name: "rate limited stops", statuses: []int{http.StatusTooManyRequests, http.StatusOK}, wantRequests: 1},
		{name: "bounded failures", statuses: []int{500, 500, 500, 500, 500, 200}, wantRequests: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				status := test.statuses[requests]
				requests++
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := &fakeGistClient{gist: &gist.Gist{ID: "abc123", URL: "https://gist.github.com/test/abc123"}}
			publisher := NewGitHubPublisher()
			publisher.newClient = func() (githubGistClient, error) { return client, nil }
			publisher.http = server.Client()
			publisher.sleep = func(context.Context, time.Duration) bool { return true }
			publisher.previewURL = func(string) string { return server.URL }

			request := commandTestRequest()
			request.Visibility = VisibilityUnlisted
			result, err := publisher.Create(context.Background(), request)
			if err != nil {
				t.Fatalf("Create error=%v", err)
			}
			if result.Ready != test.wantReady || requests != test.wantRequests || client.createCalls != 1 {
				t.Fatalf("result=%+v requests=%d createCalls=%d", result, requests, client.createCalls)
			}
		})
	}
}

func TestGitHubPublisherUpdateUsesStoredVisibilityAndProbesPrimaryPreview(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := &fakeGistClient{gist: &gist.Gist{ID: "abc123", URL: "", Public: false}}
	publisher := NewGitHubPublisher()
	publisher.newClient = func() (githubGistClient, error) { return client, nil }
	publisher.http = server.Client()
	publisher.previewURL = func(string) string { return server.URL }

	request := commandTestRequest()
	request.Visibility = VisibilityPublic // Existing Gist visibility is immutable.
	result, err := publisher.Update(context.Background(), "abc123", request)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if result.Visibility != VisibilityUnlisted || !result.Ready || result.URL != server.URL || result.SourceURL != gist.GetURL("abc123") {
		t.Fatalf("result=%+v", result)
	}
	if client.getCalls != 1 || client.updateCalls != 1 || requests != 1 {
		t.Fatalf("calls get=%d update=%d readiness=%d", client.getCalls, client.updateCalls, requests)
	}
}

func TestGitHubPublisherUpdateReadinessFailureDoesNotFailUpdate(t *testing.T) {
	client := &fakeGistClient{gist: &gist.Gist{ID: "abc123", URL: gist.GetURL("abc123"), Public: true}}
	publisher := NewGitHubPublisher()
	publisher.newClient = func() (githubGistClient, error) { return client, nil }
	publisher.http = &http.Client{Timeout: time.Millisecond}
	publisher.sleep = func(context.Context, time.Duration) bool { return true }
	publisher.previewURL = func(string) string { return "https://127.0.0.1:1/unreachable" }
	request := commandTestRequest()
	request.Visibility = VisibilityUnlisted
	result, err := publisher.Update(context.Background(), "abc123", request)
	if err != nil || result.Visibility != VisibilityPublic || result.Ready {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

func TestGitHubPublisherDoesNotFailCreatedShareWhenProbeFails(t *testing.T) {
	client := &fakeGistClient{gist: &gist.Gist{ID: "abc123", URL: "https://gist.github.com/test/abc123"}}
	publisher := NewGitHubPublisher()
	publisher.newClient = func() (githubGistClient, error) { return client, nil }
	publisher.http = &http.Client{Timeout: time.Millisecond}
	publisher.sleep = func(context.Context, time.Duration) bool { return true }
	publisher.previewURL = func(string) string { return "https://127.0.0.1:1/unreachable" }
	request := commandTestRequest()
	request.Visibility = VisibilityPublic
	result, err := publisher.Create(context.Background(), request)
	if err != nil || result.ID != "abc123" || result.Ready {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
