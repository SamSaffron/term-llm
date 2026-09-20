package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleResponsesRejectsInvalidAdmissionRequests(t *testing.T) {
	t.Parallel()

	longValue := strings.Repeat("x", maxResponseClientMessageIDLength+1)
	tests := []struct {
		name        string
		method      string
		body        string
		contentType bool
		headers     map[string]string
		wantStatus  int
		wantBody    string
	}{
		{
			name:       "method",
			method:     http.MethodGet,
			wantStatus: http.StatusMethodNotAllowed,
			wantBody:   "method not allowed",
		},
		{
			name:       "content type",
			method:     http.MethodPost,
			body:       `{"input":"hello"}`,
			wantStatus: http.StatusUnsupportedMediaType,
			wantBody:   "Content-Type",
		},
		{
			name:        "malformed JSON",
			method:      http.MethodPost,
			body:        `{`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "unexpected EOF",
		},
		{
			name:        "invalid input format",
			method:      http.MethodPost,
			body:        `{"input":42}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "invalid input format",
		},
		{
			name:        "empty input array",
			method:      http.MethodPost,
			body:        `{"input":[]}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "input is required",
		},
		{
			name:        "third-party UI context",
			method:      http.MethodPost,
			body:        `{"input":"hello","ui_context":{"width":80}}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "invalid UI context",
		},
		{
			name:        "oversized loaded UI context id",
			method:      http.MethodPost,
			body:        `{"input":"hello","client_message_id":"msg-ui","ui_context":{"loaded":["` + longValue + `"]}}`,
			contentType: true,
			headers:     map[string]string{"X-Term-LLM-UI-Version": "test"},
			wantStatus:  http.StatusBadRequest,
			wantBody:    "invalid UI context",
		},
		{
			name:        "third-party agent",
			method:      http.MethodPost,
			body:        `{"input":"hello","agent":"reviewer"}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "agent is available only to the Web UI",
		},
		{
			name:        "unsafe agent name",
			method:      http.MethodPost,
			body:        `{"input":"hello","client_message_id":"msg-agent","agent":"../reviewer"}`,
			contentType: true,
			headers:     map[string]string{"X-Term-LLM-UI-Version": "test"},
			wantStatus:  http.StatusBadRequest,
			wantBody:    "invalid agent name",
		},
		{
			name:        "mutually exclusive project selection",
			method:      http.MethodPost,
			body:        `{"input":"hello","no_project":true,"project_id":"project"}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "no_project and project_id are mutually exclusive",
		},
		{
			name:        "third-party no project",
			method:      http.MethodPost,
			body:        `{"input":"hello","no_project":true}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "no_project is available only to the Web UI",
		},
		{
			name:        "disabled project mode no project",
			method:      http.MethodPost,
			body:        `{"input":"hello","client_message_id":"msg-project","no_project":true}`,
			contentType: true,
			headers:     map[string]string{"X-Term-LLM-UI-Version": "test"},
			wantStatus:  http.StatusBadRequest,
			wantBody:    "no_project is not accepted while project mode is disabled",
		},
		{
			name:        "missing first-party client message id",
			method:      http.MethodPost,
			body:        `{"input":"hello"}`,
			contentType: true,
			headers:     map[string]string{"X-Term-LLM-UI-Version": "test"},
			wantStatus:  http.StatusBadRequest,
			wantBody:    "client_message_id is required for first-party requests",
		},
		{
			name:        "oversized client message id",
			method:      http.MethodPost,
			body:        `{"input":"hello","client_message_id":"` + longValue + `"}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "client_message_id is too long",
		},
		{
			name:        "mismatched client message id",
			method:      http.MethodPost,
			body:        `{"input":[{"type":"message","role":"user","content":"hello","client_message_id":"item-id"}],"client_message_id":"request-id"}`,
			contentType: true,
			headers:     map[string]string{"X-Term-LLM-UI-Version": "test"},
			wantStatus:  http.StatusBadRequest,
			wantBody:    "top-level client_message_id must match the final user message",
		},
		{
			name:        "branch without previous response",
			method:      http.MethodPost,
			body:        `{"input":"hello","branch":true}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "branch requires previous_response_id",
		},
		{
			name:        "branch context without branch",
			method:      http.MethodPost,
			body:        `{"input":"hello","branch_context":{"mode":"auto"}}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    "branch_context requires branch=true",
		},
		{
			name:        "invalid draft",
			method:      http.MethodPost,
			body:        `{"input":"hello"}`,
			contentType: true,
			headers:     map[string]string{requestDraftIDHeader: "draft_client"},
			wantStatus:  http.StatusBadRequest,
			wantBody:    "draft id is only valid for a first-party new conversation",
		},
		{
			name:        "unknown previous response",
			method:      http.MethodPost,
			body:        `{"input":"hello","previous_response_id":"resp_missing"}`,
			contentType: true,
			wantStatus:  http.StatusBadRequest,
			wantBody:    `previous_response_id \"resp_missing\" not found`,
		},
		{
			name:        "third-party completion notification",
			method:      http.MethodPost,
			body:        `{"input":"hello"}`,
			contentType: true,
			headers:     map[string]string{requestPushSubscriptionHeader: "subscription"},
			wantStatus:  http.StatusBadRequest,
			wantBody:    "completion notification target is only valid for first-party streaming responses",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := &serveServer{}
			request := httptest.NewRequest(test.method, "/v1/responses", strings.NewReader(test.body))
			if test.contentType {
				request.Header.Set("Content-Type", "application/json")
			}
			for name, value := range test.headers {
				request.Header.Set(name, value)
			}
			response := httptest.NewRecorder()

			server.handleResponses(response, request)

			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), test.wantBody) {
				t.Fatalf("response = %d %s, want status %d containing %q", response.Code, response.Body.String(), test.wantStatus, test.wantBody)
			}
			if test.method != http.MethodPost && response.Header().Get("Allow") != "POST" {
				t.Fatalf("Allow header = %q, want POST", response.Header().Get("Allow"))
			}
		})
	}
}

func TestHandleResponsesRejectsResolvedSessionAdmission(t *testing.T) {
	t.Parallel()

	t.Run("corrupted response mapping", func(t *testing.T) {
		t.Parallel()
		server := &serveServer{}
		server.responseToSession.Store("legacy-response", 42)
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello","previous_response_id":"legacy-response"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()

		server.handleResponses(response, request)

		if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "corrupted session mapping") {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("conflicting mapped session", func(t *testing.T) {
		t.Parallel()
		server := &serveServer{}
		server.responseToSession.Store("legacy-response", "mapped-session")
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello","previous_response_id":"legacy-response"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(requestSessionIDHeader, "header-session")
		response := httptest.NewRecorder()

		server.handleResponses(response, request)

		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `session_id \"header-session\" conflicts`) {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})

	t.Run("branch context preparation", func(t *testing.T) {
		t.Parallel()
		server := &serveServer{}
		server.branchNotes.Store("busy-session", struct{}{})
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(requestSessionIDHeader, "busy-session")
		response := httptest.NewRecorder()

		server.handleResponses(response, request)

		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "branch context is still being prepared") {
			t.Fatalf("response = %d %s", response.Code, response.Body.String())
		}
	})
}

func TestPrepareResolvedResponseAdmissionCanceledDoesNotCallNilRelease(t *testing.T) {
	t.Parallel()

	server := &serveServer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	response := httptest.NewRecorder()

	_, release, stop := server.prepareResolvedResponseAdmission(response, request, ctx, resolvedResponsesRequest{
		req: responsesCreateRequest{Stream: true},
	}, nil, "session", "scope", "key")
	if !stop {
		t.Fatal("canceled admission did not stop request handling")
	}
	if release == nil {
		t.Fatal("canceled admission returned a nil release function")
	}
	release()
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestPrepareResolvedResponseAdmissionReleasesClaimOnWorkspaceRejection(t *testing.T) {
	t.Parallel()

	manager := newServeResponseRunManager()
	t.Cleanup(manager.Close)
	server := &serveServer{responseRuns: manager}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response := httptest.NewRecorder()

	_, release, stop := server.prepareResolvedResponseAdmission(response, request, context.Background(), resolvedResponsesRequest{
		req: responsesCreateRequest{Stream: true, ProjectID: "project-while-disabled"},
	}, nil, "session", "scope", "key")
	if !stop {
		t.Fatal("workspace rejection did not stop request handling")
	}
	release()
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "projects_disabled") {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}

	manager.mu.Lock()
	_, claimHeld := manager.idempotencyAdmissions[responseRunIdempotencyScope("scope", "key")]
	manager.mu.Unlock()
	if claimHeld {
		t.Fatal("workspace rejection retained the idempotency admission claim")
	}
	retryRelease, err := manager.admitIdempotency(context.Background(), "scope", "key")
	if err != nil {
		t.Fatalf("reacquire released claim: %v", err)
	}
	retryRelease()
}
