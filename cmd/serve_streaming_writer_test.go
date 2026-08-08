package cmd

import (
	"testing"

	"github.com/samsaffron/term-llm/internal/testutil"
)

func TestStreamingResponseWriterSetsPerWriteDeadline(t *testing.T) {
	testutil.AssertStreamingWriterDeadlines(t, newStreamingResponseWriter)
}
