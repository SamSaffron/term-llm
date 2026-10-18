package anthropic

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	llmstream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/providerhttp"
)

const summaryPrefix = "[Context Compaction]\n"

// isRetryable mirrors the structured part of the root llm retry classifier
// (incomplete streams and HTTP status errors), which this package cannot
// import. The root golden tests keep covering the string heuristics.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	var incomplete *llmstream.StreamIncompleteError
	if errors.As(err, &incomplete) {
		return true
	}
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) {
		return providerhttp.RetryableStatus(statusErr.HTTPStatusCode())
	}
	return false
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newOpenCodeGoAnthropicProvider(apiKey, baseURL, model string, client *http.Client) *AnthropicProvider {
	return NewMessagesProvider(apiKey, baseURL, model, client, "invalid OpenCode Go base URL")
}

func TestMain(m *testing.M) {
	// The real hooks live in the root llm package (model registry); a minimal
	// stand-in strips explicit effort suffixes so suffix decoding is covered.
	ParentHooks.BaseModelAndEffort = func(model string) (string, string) {
		if base, ok := trimKnownEffortSuffix(model); ok && !(base == "claude-sonnet-4-6" && strings.HasSuffix(model, "-xhigh")) {
			return base, model[len(base)+1:]
		}
		return model, ""
	}
	os.Exit(m.Run())
}
