package llm

import (
	"testing"

	corestream "github.com/term-llm/term-llm/internal/llm/core/stream"
)

func TestBedrockCompatTruncatedStreamIsRetryable(t *testing.T) {
	if !isRetryable(&corestream.StreamIncompleteError{}) {
		t.Fatal("incomplete stream errors must be retryable")
	}
}
