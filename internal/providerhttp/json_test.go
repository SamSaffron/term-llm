package providerhttp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDoJSONRequestUsesDefaultClientAndCustomHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Custom token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Test"); got != "yes" {
			t.Errorf("X-Test = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		if err := json.Unmarshal(body, &payload); err != nil || payload["hello"] != "world" {
			t.Errorf("payload = %q, err=%v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	body, contentType, err := DoJSONRequest(context.Background(), JSONRequestOptions{
		URL: server.URL, Method: http.MethodPost, APIKey: "ignored", Payload: map[string]string{"hello": "world"}, Provider: "test",
		Headers: http.Header{"Authorization": {"Custom token"}, "X-Test": {"yes"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"ok":true}` || contentType != "application/json" {
		t.Fatalf("response = %q, %q", body, contentType)
	}
}
