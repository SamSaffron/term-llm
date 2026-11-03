package wire_test

import (
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/app/serve/wire"
)

func TestConditionalJSONPreservesPrivateCachePolicy(t *testing.T) {
	payload := map[string]any{"viewer": "alice"}
	for _, conditional := range []bool{false, true} {
		request := httptest.NewRequest("GET", "/", nil)
		if conditional {
			request.Header.Set("If-None-Match", wire.JSONPayloadETag([]byte(`{"viewer":"alice"}`)))
		}
		response := httptest.NewRecorder()
		response.Header().Set("Cache-Control", "private, no-store")
		wire.WriteJSONConditional(response, request, 200, payload)
		want := 200
		if conditional {
			want = 304
		}
		if response.Code != want || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("conditional=%t response=%d %v", conditional, response.Code, response.Header())
		}
	}
}
