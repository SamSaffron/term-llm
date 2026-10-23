package opencompat

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestXAIListModels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "array pricing",
			body: `{"data":[{"id":"grok-test","created":123,"owned_by":"xai","pricing":[{"prompt":"0.01"}]}]}`,
		},
		{
			name: "object pricing",
			body: `{"data":[{"id":"grok-test","created":123,"owned_by":"xai","pricing":{"prompt":"0.01","completion":"0.02"}}]}`,
		},
		{
			name: "no pricing",
			body: `{"data":[{"id":"grok-test","created":123,"owned_by":"xai"}]}`,
		},
		{
			name:    "malformed model",
			body:    `{"data":[{"id":[]}]}`,
			wantErr: true,
		},
		{
			name:    "invalid JSON",
			body:    `{"data":[`,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origClient := defaultHTTPClient
			t.Cleanup(func() { defaultHTTPClient = origClient })
			defaultHTTPClient = &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Method != http.MethodGet || req.URL.String() != xaiBaseURL+"/models" {
						t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
					}
					if got := req.Header.Get("Authorization"); got != "Bearer test-key" {
						t.Fatalf("Authorization = %q, want Bearer test-key", got)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(tc.body)),
					}, nil
				}),
			}

			models, err := NewXAIProvider("test-key", "grok-test").ListModels(context.Background())
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "failed to parse models response") {
					t.Fatalf("expected parse error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if len(models) != 1 {
				t.Fatalf("got %d models, want 1", len(models))
			}
			model := models[0]
			if model.ID != "grok-test" || model.Created != 123 || model.OwnedBy != "xai" {
				t.Fatalf("unexpected model: %+v", model)
			}
			if model.InputLimit != InputLimitForModel("grok-test") {
				t.Fatalf("unexpected input limit: %d", model.InputLimit)
			}
		})
	}
}
