package responsesproviders_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/llm/core"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

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

func containsModelID(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func boolPtr(v bool) *bool { return &v }

// drainStreamErr drains the stream and returns any terminal error event err.
func drainStreamErr(t *testing.T, stream core.Stream) error {
	t.Helper()
	var streamErr error
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return streamErr
		}
		if err != nil {
			return err
		}
		if event.Type == core.EventError && event.Err != nil {
			streamErr = event.Err
		}
	}
}

func drainStreamToDone(t *testing.T, stream llm.Stream) {
	t.Helper()
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		if event.Type == llm.EventError {
			t.Fatalf("stream returned error event: %v", event.Err)
		}
		if event.Type == llm.EventDone {
			return
		}
	}
}

// createProviderFromConfig builds a provider through the public factory
// without the retry wrapper.
func createProviderFromConfig(name string, pc *config.ProviderConfig) (llm.Provider, error) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{name: *pc}}
	return llm.NewProviderByNameNoRetry(cfg, name, "")
}

type countingTool struct {
	calls atomic.Int64
}

func (t *countingTool) Spec() core.ToolSpec {
	return core.ToolSpec{
		Name:        "count_tool",
		Description: "Counts executions",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *countingTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	t.calls.Add(1)
	return core.TextOutput("ok"), nil
}

func (t *countingTool) Preview(args json.RawMessage) string { return "" }
