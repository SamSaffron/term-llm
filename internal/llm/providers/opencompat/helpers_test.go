package opencompat

import (
	"bufio"
	"io"
	"net/http"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/core/stream"
)

type (
	EventType       = core.EventType
	ReasoningKind   = core.ReasoningKind
	ToolResult      = core.ToolResult
	ToolContentPart = core.ToolContentPart
	ToolImageData   = core.ToolImageData
)

const (
	EventError          = core.EventError
	ToolContentPartText = core.ToolContentPartText
)

var UserText = core.UserText

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

func readSSEEvent(reader *bufio.Reader) (string, string, bool, error) {
	return stream.ReadEvent(reader)
}

func readSSELine(reader *bufio.Reader) (string, bool, error) { return stream.ReadLine(reader) }

func drainStreamToDone(t *testing.T, stream Stream) {
	t.Helper()
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			return
		}
		if recvErr != nil {
			t.Fatalf("stream recv failed: %v", recvErr)
		}
		if event.Type == EventError {
			t.Fatalf("stream returned error event: %v", event.Err)
		}
		if event.Type == EventDone {
			return
		}
	}
}
