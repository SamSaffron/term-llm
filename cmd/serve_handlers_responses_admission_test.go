package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
