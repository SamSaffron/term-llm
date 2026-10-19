package llm

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
)

type retryForkBoundary struct {
	SessionID    string `json:"session_id"`
	MessagesSent int    `json:"messages_sent"`
}

// retryForkProvider exposes both fork contracts without relying on a CLI
// provider's private conversation state or launching a real process.
type retryForkProvider struct {
	core.Provider
	boundary retryForkBoundary
	state    []byte
	request  []core.Message
}

func (p *retryForkProvider) ForkConversationAtBoundary(state []byte, request []core.Message) (core.Provider, bool) {
	var boundary retryForkBoundary
	if err := json.Unmarshal(state, &boundary); err != nil {
		return nil, false
	}
	p.state = append([]byte(nil), state...)
	p.request = append([]core.Message(nil), request...)
	return &retryForkProvider{Provider: p.Provider, boundary: boundary}, true
}

func (p *retryForkProvider) ForkHelperConversation() (core.Provider, bool) {
	return &retryForkProvider{Provider: p.Provider, boundary: retryForkBoundary{SessionID: p.boundary.SessionID}}, true
}

func TestRetryProviderForwardsSuccessfulBoundaryFork(t *testing.T) {
	liveBoundary := retryForkBoundary{SessionID: "live-session-after-capture", MessagesSent: 8}
	live := &retryForkProvider{Provider: mockprovider.NewMockProvider("live"), boundary: liveBoundary}
	config := DefaultRetryConfig()
	config.MaxAttempts = 3
	wrapped := WrapWithRetry(live, config)
	if !SupportsHelperBoundaryFork(wrapped) {
		t.Fatal("RetryProvider does not advertise the boundary fork seam")
	}
	captured := retryForkBoundary{SessionID: "captured-session", MessagesSent: 2}
	state := []byte(`{"session_id":"captured-session","messages_sent":2}`)
	request := []core.Message{core.UserText("q"), core.AssistantText("a"), core.UserText("side question")}
	forked, ok := ForkHelperAtBoundary(wrapped, state, request)
	if !ok {
		t.Fatal("expected RetryProvider to forward the boundary fork")
	}
	retry, ok := forked.(*RetryProvider)
	if !ok || retry == wrapped {
		t.Fatalf("forked provider = %T (%p), want a new *RetryProvider", forked, forked)
	}
	clone, ok := retry.inner.(*retryForkProvider)
	if !ok || clone == live {
		t.Fatalf("forked inner = %T (%p), want a clone, not the live provider", retry.inner, retry.inner)
	}
	if clone.boundary != captured || live.boundary != liveBoundary {
		t.Fatalf("clone boundary = %+v, live = %+v; want captured %+v and unchanged live %+v", clone.boundary, live.boundary, captured, liveBoundary)
	}
	if string(live.state) != string(state) || !reflect.DeepEqual(live.request, request) {
		t.Fatalf("forwarded state/request = %s / %#v, want %s / %#v", live.state, live.request, state, request)
	}
	if retry.config != config {
		t.Fatalf("fork retry config = %+v, want %+v", retry.config, config)
	}
}

func TestRetryProviderForwardsSuccessfulSuffixFork(t *testing.T) {
	liveBoundary := retryForkBoundary{SessionID: "live-session", MessagesSent: 4}
	live := &retryForkProvider{Provider: mockprovider.NewMockProvider("live"), boundary: liveBoundary}
	wrapped := WrapWithRetry(live, DefaultRetryConfig())
	forked, ok := forkConversationProvider(wrapped)
	if !ok {
		t.Fatal("expected RetryProvider to forward the suffix-only fork")
	}
	retry, ok := forked.(*RetryProvider)
	if !ok || retry == wrapped {
		t.Fatalf("forked provider = %T, want a new *RetryProvider", forked)
	}
	clone, ok := retry.inner.(*retryForkProvider)
	if !ok || clone == live {
		t.Fatalf("forked inner = %T, want a clone, not the live provider", retry.inner)
	}
	if clone.boundary.SessionID != liveBoundary.SessionID || clone.boundary.MessagesSent != 0 || live.boundary != liveBoundary {
		t.Fatalf("clone boundary = %+v, live = %+v; want resumed session, helper-local offset zero, and unchanged live", clone.boundary, live.boundary)
	}
}
