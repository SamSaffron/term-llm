package serve

import (
	"testing"

	"github.com/term-llm/term-llm/internal/testutil"
)

func TestStreamingResponseWriterSetsPerWriteDeadline(t *testing.T) {
	testutil.AssertStreamingWriterDeadlines(t, newStreamingResponseWriter)
}
