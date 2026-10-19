package responsesproviders

import (
	"io"
	"net/http"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
)

type (
	EventType                      = core.EventType
	MultiAgentOptions              = core.MultiAgentOptions
	PromptCacheOptions             = core.PromptCacheOptions
	ProgrammaticToolCallingOptions = core.ProgrammaticToolCallingOptions
	ResponsesInputItem             = responses.ResponsesInputItem
	ToolImageData                  = core.ToolImageData
)

const (
	EventError                   = core.EventError
	EventProviderReplay          = core.EventProviderReplay
	PartImage                    = core.PartImage
	ReasoningKindSummary         = core.ReasoningKindSummary
	RoleAssistant                = core.RoleAssistant
	ToolChoiceAuto               = core.ToolChoiceAuto
	responsesWebSocketBetaHeader = responses.WebSocketBetaHeader
)

var (
	UserText      = core.UserText
	SystemText    = core.SystemText
	AssistantText = core.AssistantText

	BuildResponsesInputWithInstructions = responses.BuildResponsesInputWithInstructions
	composeBetaHeader                   = responses.ComposeBetaHeader
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
