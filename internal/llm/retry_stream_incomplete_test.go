package llm

import (
	"testing"

	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
)

func TestBedrockCompatTruncatedStreamIsRetryable(t *testing.T) {
	if !isRetryable(&corestream.StreamIncompleteError{}) {
		t.Fatal("incomplete stream errors must be retryable")
	}
}
