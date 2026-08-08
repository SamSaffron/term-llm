package servehttp

import (
	"net/http/httptest"
	"testing"

	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestNewStreamingResponseWriterNoTimeoutReturnsOriginal(t *testing.T) {
	recorder := httptest.NewRecorder()
	if got := NewStreamingResponseWriter(recorder, 0); got != recorder {
		t.Fatal("expected original writer when timeout is disabled")
	}
}

func TestStreamingResponseWriterSetsPerWriteDeadline(t *testing.T) {
	testutil.AssertStreamingWriterDeadlines(t, NewStreamingResponseWriter)
}
