package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type retryStreamingProvider struct {
	attempt int
}

type textThenErrorProvider struct {
	attempts int
}

type phaseThenErrorProvider struct {
	attempts int
}

type warningPhaseThenErrorProvider struct {
	attempts int
}

type syncToolProvider struct{}

func (p *retryStreamingProvider) Name() string        { return "retry-streaming" }
func (p *textThenErrorProvider) Name() string         { return "text-then-error" }
func (p *phaseThenErrorProvider) Name() string        { return "phase-then-error" }
func (p *warningPhaseThenErrorProvider) Name() string { return "warning-phase-then-error" }
func (p *syncToolProvider) Name() string              { return "sync-tool" }

func (p *retryStreamingProvider) Credential() string        { return "mock" }
func (p *textThenErrorProvider) Credential() string         { return "mock" }
func (p *phaseThenErrorProvider) Credential() string        { return "mock" }
func (p *warningPhaseThenErrorProvider) Credential() string { return "mock" }
func (p *syncToolProvider) Credential() string              { return "mock" }

func (p *retryStreamingProvider) Capabilities() Capabilities        { return Capabilities{} }
func (p *textThenErrorProvider) Capabilities() Capabilities         { return Capabilities{} }
func (p *phaseThenErrorProvider) Capabilities() Capabilities        { return Capabilities{} }
func (p *warningPhaseThenErrorProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *syncToolProvider) Capabilities() Capabilities              { return Capabilities{} }

func (p *retryStreamingProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.attempt++
	if p.attempt == 1 {
		return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
			send.Send(Event{Type: EventError, Err: &RateLimitError{Message: "rate limit", RetryAfter: 5 * time.Millisecond}})
			return nil
		}), nil
	}
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		send.Send(Event{Type: EventTextDelta, Text: "hello"})
		send.Send(Event{Type: EventTextDelta, Text: " world"})
		return nil
	}), nil
}

func (p *textThenErrorProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.attempts++
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		send.Send(Event{Type: EventTextDelta, Text: "hello"})
		send.Send(Event{Type: EventError, Err: errors.New("502 bad gateway")})
		return nil
	}), nil
}

func (p *phaseThenErrorProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.attempts++
	if p.attempts == 1 {
		return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
			send.Send(Event{Type: EventPhase, Text: "Compacting context..."})
			send.Send(Event{Type: EventError, Err: errors.New("502 bad gateway")})
			return nil
		}), nil
	}
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		send.Send(Event{Type: EventTextDelta, Text: "hello"})
		send.Send(Event{Type: EventTextDelta, Text: " world"})
		return nil
	}), nil
}

func (p *warningPhaseThenErrorProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.attempts++
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		send.Send(Event{Type: EventPhase, Text: WarningPhasePrefix + "context is nearly full"})
		send.Send(Event{Type: EventError, Err: errors.New("502 bad gateway")})
		return nil
	}), nil
}

func (p *syncToolProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		responseCh := make(chan ToolExecutionResponse, 1)
		send.Send(Event{
			Type:         EventToolCall,
			ToolCallID:   "tool-1",
			ToolName:     "read_file",
			Tool:         &ToolCall{ID: "tool-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/tmp/test.txt"}`)},
			ToolResponse: responseCh,
		})

		select {
		case resp := <-responseCh:
			if resp.Err != nil {
				return resp.Err
			}
			send.Send(Event{Type: EventTextDelta, Text: resp.Result.Content})
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), nil
}

func TestDefaultRetryConfigUsesThirtyMinuteElapsedBudget(t *testing.T) {
	cfg := DefaultRetryConfig()
	if cfg.MaxAttempts != 0 {
		t.Fatalf("MaxAttempts = %d, want 0 for time-budgeted retries", cfg.MaxAttempts)
	}
	if cfg.MaxElapsedTime != 30*time.Minute {
		t.Fatalf("MaxElapsedTime = %s, want 30m", cfg.MaxElapsedTime)
	}
	if cfg.BaseBackoff != time.Second || cfg.MaxBackoff != 30*time.Second {
		t.Fatalf("backoff defaults = %s/%s, want 1s/30s", cfg.BaseBackoff, cfg.MaxBackoff)
	}
}

func TestRetryProvider_ZeroConfigDoesNotRetry(t *testing.T) {
	inner := &retryFailNTimesProvider{failures: 2, err: errors.New("503 service unavailable")}
	provider := WrapWithRetry(inner, RetryConfig{})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv returned error: %v", err)
	}
	if event.Type != EventError || event.Err == nil {
		t.Fatalf("got event %+v, want error event", event)
	}
	if inner.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", inner.attempts)
	}
}

func TestIsRetryable_500InternalServerError(t *testing.T) {
	cases := []struct {
		msg       string
		retryable bool
	}{
		{"anthropic streaming error: POST \"https://api.anthropic.com/v1/messages\": 500 Internal Server Error", true},
		{"500 internal server error", true},
		{"got 500 from upstream", true},
		{"501 not implemented from upstream", true},
		{"provider returned status 525", true},
		{"provider returned status 599", true},
		{"internal server error occurred", true},
		{"400 Bad Request", false},
		{"401 Unauthorized", false},
		{"token budget 1500 exceeded", false},
	}
	for _, tc := range cases {
		got := isRetryable(errors.New(tc.msg))
		if got != tc.retryable {
			t.Errorf("isRetryable(%q) = %v, want %v", tc.msg, got, tc.retryable)
		}
	}
}

func TestIsRetryable_APIErrorTerminated(t *testing.T) {
	cases := []struct {
		msg       string
		retryable bool
	}{
		{"claude API error: API Error: terminated", true},
		{"claude api error: api error: terminated", true},
		{"API Error: terminated", true},
		{"some other api error: bad request", false},
	}
	for _, tc := range cases {
		got := isRetryable(errors.New(tc.msg))
		if got != tc.retryable {
			t.Errorf("isRetryable(%q) = %v, want %v", tc.msg, got, tc.retryable)
		}
	}
}

// toolThenErrorProvider emits a synchronous tool call then a retryable error.
// The retry loop must NOT retry after the tool call has been committed.
type toolThenErrorProvider struct {
	attempts int
}

func (p *toolThenErrorProvider) Name() string       { return "tool-then-error" }
func (p *toolThenErrorProvider) Credential() string { return "mock" }
func (p *toolThenErrorProvider) Capabilities() Capabilities {
	return Capabilities{}
}

func (p *toolThenErrorProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.attempts++
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		// Emit a synchronous tool call (has ToolResponse channel)
		response := make(chan ToolExecutionResponse, 1)
		send.Send(Event{
			Type:         EventToolCall,
			ToolResponse: response,
			ToolName:     "test_tool",
		})
		// Simulate tool execution completing
		response <- ToolExecutionResponse{Result: ToolOutput{Content: "tool result"}}
		// Then a retryable error occurs
		send.Send(Event{Type: EventError, Err: errors.New("502 bad gateway")})
		return nil
	}), nil
}

type retryFailNTimesProvider struct {
	attempts int
	failures int
	err      error
}

func (p *retryFailNTimesProvider) Name() string               { return "retry-fail-n-times" }
func (p *retryFailNTimesProvider) Credential() string         { return "mock" }
func (p *retryFailNTimesProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *retryFailNTimesProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.attempts++
	if p.attempts <= p.failures {
		return nil, p.err
	}
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		return send.Send(Event{Type: EventDone})
	}), nil
}

func TestRetryProvider_TimeBudgetRetriesBeyondFiveAttempts(t *testing.T) {
	inner := &retryFailNTimesProvider{failures: 6, err: errors.New("503 service unavailable")}
	provider := WrapWithRetry(inner, RetryConfig{
		MaxAttempts:    0,
		MaxElapsedTime: 2 * time.Second,
		BaseBackoff:    time.Millisecond,
		MaxBackoff:     time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	retryEvents := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv returned error: %v", err)
		}
		if event.Type == EventError {
			t.Fatalf("unexpected error event: %v", event.Err)
		}
		if event.Type == EventRetry {
			retryEvents++
			if event.RetryMaxAttempts != 0 {
				t.Fatalf("RetryMaxAttempts = %d, want 0 for time-budget retry", event.RetryMaxAttempts)
			}
		}
	}

	if inner.attempts != 7 {
		t.Fatalf("attempts = %d, want 7", inner.attempts)
	}
	if retryEvents != 6 {
		t.Fatalf("retryEvents = %d, want 6", retryEvents)
	}
}

func TestRetryProvider_StopsWhenRetryAfterExceedsElapsedBudget(t *testing.T) {
	inner := &retryFailNTimesProvider{
		failures: 100,
		err:      &RateLimitError{Message: "rate limit", RetryAfter: 20 * time.Millisecond},
	}
	provider := WrapWithRetry(inner, RetryConfig{
		MaxAttempts:    0,
		MaxElapsedTime: 5 * time.Millisecond,
		BaseBackoff:    time.Millisecond,
		MaxBackoff:     time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv returned error: %v", err)
	}
	if event.Type != EventError || event.Err == nil {
		t.Fatalf("got event %+v, want error event", event)
	}
	if !strings.Contains(event.Err.Error(), "exceeds remaining retry window") {
		t.Fatalf("error = %v, want retry window context", event.Err)
	}
	if inner.attempts != 1 {
		t.Fatalf("attempts = %d, want 1", inner.attempts)
	}
}

func TestIsRetryable_HTTPStatusError(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusNotImplemented, http.StatusServiceUnavailable, 525} {
		if !isRetryable(newHTTPStatusErrorString("test", status, http.StatusText(status), nil, "transient")) {
			t.Fatalf("%d HTTPStatusError should be retryable", status)
		}
	}
	if isRetryable(newHTTPStatusErrorString("test", http.StatusNotFound, "404 Not Found", nil, "not found")) {
		t.Fatal("404 HTTPStatusError should not be retryable")
	}
	if isRetryable(newHTTPStatusErrorString("test", http.StatusConflict, "409 Conflict", nil, "conflict")) {
		t.Fatal("409 HTTPStatusError should not be retryable by default")
	}
}

func TestRetryProvider_BackoffUsesStructuredRetryAfterWithoutMaxBackoffCap(t *testing.T) {
	provider := &RetryProvider{config: RetryConfig{
		BaseBackoff: time.Second,
		MaxBackoff:  30 * time.Second,
	}}

	err := &RateLimitError{Message: "rate limit", RetryAfter: 2 * time.Minute}
	if got := provider.calculateBackoff(1, err); got != 2*time.Minute {
		t.Fatalf("backoff = %s, want uncapped Retry-After 2m", got)
	}
}

func TestRetryProvider_BackoffParsesRetryAfterMsFromHTTPStatusError(t *testing.T) {
	provider := &RetryProvider{config: RetryConfig{
		BaseBackoff: time.Second,
		MaxBackoff:  30 * time.Second,
	}}
	header := http.Header{"Retry-After-Ms": {"250"}}
	err := newHTTPStatusErrorString("test", http.StatusTooManyRequests, "429 Too Many Requests", header, "slow down")

	if got := provider.calculateBackoff(1, err); got != 250*time.Millisecond {
		t.Fatalf("backoff = %s, want Retry-After-Ms 250ms", got)
	}
}

func TestRetryProvider_DoesNotRetryAfterCommittedToolCall(t *testing.T) {
	inner := &toolThenErrorProvider{}
	provider := WrapWithRetry(inner, RetryConfig{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			break // error is expected — the committed error propagates
		}
	}

	if inner.attempts != 1 {
		t.Fatalf("inner.attempts = %d, want 1 (should not retry after committed tool call)", inner.attempts)
	}
}

func TestRetryProvider_RetriesBeforeVisibleTextIsCommitted(t *testing.T) {
	provider := WrapWithRetry(&retryStreamingProvider{}, RetryConfig{
		MaxAttempts: 2,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	var text string
	retryEvents := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv returned error: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			text += event.Text
		case EventRetry:
			retryEvents++
		case EventError:
			t.Fatalf("unexpected final error event: %v", event.Err)
		}
	}

	if retryEvents != 1 {
		t.Fatalf("retryEvents = %d, want 1", retryEvents)
	}
	if text != "hello world" {
		t.Fatalf("text = %q, want %q", text, "hello world")
	}
}

func TestRetryProvider_RetriesAfterTransientPhaseBeforeCommit(t *testing.T) {
	inner := &phaseThenErrorProvider{}
	provider := WrapWithRetry(inner, RetryConfig{
		MaxAttempts: 2,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	var text string
	phaseEvents := 0
	retryEvents := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv returned error: %v", err)
		}
		switch event.Type {
		case EventTextDelta:
			text += event.Text
		case EventPhase:
			phaseEvents++
		case EventRetry:
			retryEvents++
		case EventError:
			t.Fatalf("unexpected final error event: %v", event.Err)
		}
	}

	if inner.attempts != 2 {
		t.Fatalf("inner.attempts = %d, want 2", inner.attempts)
	}
	if retryEvents != 1 {
		t.Fatalf("retryEvents = %d, want 1", retryEvents)
	}
	if phaseEvents != 0 {
		t.Fatalf("phaseEvents = %d, want 0 (transient phase should not commit the attempt)", phaseEvents)
	}
	if text != "hello world" {
		t.Fatalf("text = %q, want %q", text, "hello world")
	}
}

func TestRetryProvider_DoesNotRetryAfterVisibleWarningPhaseCommit(t *testing.T) {
	inner := &warningPhaseThenErrorProvider{}
	provider := WrapWithRetry(inner, RetryConfig{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	firstEvent := make(chan Event, 1)
	errCh := make(chan error, 1)
	go func() {
		event, err := stream.Recv()
		if err != nil {
			errCh <- err
			return
		}
		firstEvent <- event
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Recv returned error before warning phase event: %v", err)
	case event := <-firstEvent:
		want := WarningPhasePrefix + "context is nearly full"
		if event.Type != EventPhase || event.Text != want {
			t.Fatalf("first event = %+v, want warning phase %q", event, want)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for warning phase event; retry wrapper buffered visible phase")
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("expected committed error event, got recv error: %v", err)
	}
	if event.Type != EventError || event.Err == nil {
		t.Fatalf("expected committed error event after warning phase, got %+v", event)
	}
	if inner.attempts != 1 {
		t.Fatalf("inner.attempts = %d, want 1 (should not retry after visible warning phase was committed)", inner.attempts)
	}
}

func TestRetryProvider_ForwardsVisibleTextImmediatelyAndDoesNotRetryAfterCommit(t *testing.T) {
	inner := &textThenErrorProvider{}
	provider := WrapWithRetry(inner, RetryConfig{
		MaxAttempts: 3,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	firstEvent := make(chan Event, 1)
	errCh := make(chan error, 1)
	go func() {
		event, err := stream.Recv()
		if err != nil {
			errCh <- err
			return
		}
		firstEvent <- event
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Recv returned error before first text event: %v", err)
	case event := <-firstEvent:
		if event.Type != EventTextDelta || event.Text != "hello" {
			t.Fatalf("first event = %+v, want text_delta hello", event)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for first text event; retry wrapper buffered visible text")
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("expected committed error event, got recv error: %v", err)
	}
	if event.Type != EventError || event.Err == nil {
		t.Fatalf("expected committed error event after visible text, got %+v", event)
	}
	if inner.attempts != 1 {
		t.Fatalf("inner.attempts = %d, want 1 (should not retry after visible text was committed)", inner.attempts)
	}
}

func TestRetryProvider_ForwardsSyncToolCallsImmediately(t *testing.T) {
	provider := WrapWithRetry(&syncToolProvider{}, RetryConfig{
		MaxAttempts: 2,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
	})

	stream, err := provider.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	defer stream.Close()

	gotTool := make(chan Event, 1)
	errCh := make(chan error, 1)
	go func() {
		event, err := stream.Recv()
		if err != nil {
			errCh <- err
			return
		}
		gotTool <- event
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Recv returned error before tool event: %v", err)
	case event := <-gotTool:
		if event.Type != EventToolCall {
			t.Fatalf("first event type = %v, want %v", event.Type, EventToolCall)
		}
		if event.ToolResponse == nil {
			t.Fatal("expected ToolResponse channel on sync tool event")
		}
		event.ToolResponse <- ToolExecutionResponse{Result: ToolOutput{Content: "alpha"}}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for sync tool event; retry wrapper buffered it")
	}

	event, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv after tool response returned error: %v", err)
	}
	if event.Type != EventTextDelta || event.Text != "alpha" {
		t.Fatalf("got event %+v, want text_delta alpha", event)
	}
}

type listModelsProvider struct {
	models []ModelInfo
	err    error
	calls  int
}

func (p *listModelsProvider) Name() string               { return "list-models" }
func (p *listModelsProvider) Credential() string         { return "mock" }
func (p *listModelsProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *listModelsProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return nil, errors.New("not used in this test")
}
func (p *listModelsProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	p.calls++
	return p.models, p.err
}

type noListModelsProvider struct{}

func (p *noListModelsProvider) Name() string               { return "no-list-models" }
func (p *noListModelsProvider) Credential() string         { return "mock" }
func (p *noListModelsProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *noListModelsProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return nil, errors.New("not used in this test")
}

func TestRetryProvider_BackoffIsExponentialWithJitter(t *testing.T) {
	provider := &RetryProvider{config: RetryConfig{
		BaseBackoff: time.Second,
		MaxBackoff:  time.Minute,
	}}

	// attempt 6 => base * 2^(6-1) = 32s, jittered into [16s, 48s].
	// The old linear formula would be capped to [3s, 9s], so this range
	// catches regressions without depending on a fixed random seed.
	got := provider.calculateBackoff(6, errors.New("502 bad gateway"))
	if got < 16*time.Second || got > 48*time.Second {
		t.Fatalf("backoff = %s, want exponential jitter range [16s, 48s]", got)
	}
}

func TestRetryProvider_BackoffParsesRetryAfterNumericText(t *testing.T) {
	provider := &RetryProvider{config: RetryConfig{
		BaseBackoff: time.Second,
		MaxBackoff:  time.Minute,
	}}

	for _, message := range []string{
		"upstream 429: Retry-After: 7",
		"upstream 429: retry after 7 seconds",
	} {
		if got := provider.calculateBackoff(2, errors.New(message)); got != 7*time.Second {
			t.Fatalf("backoff for %q = %s, want 7s", message, got)
		}
	}
}

func TestRetryProvider_BackoffParsesRetryAfterHTTPDate(t *testing.T) {
	provider := &RetryProvider{config: RetryConfig{
		BaseBackoff: time.Second,
		MaxBackoff:  2 * time.Hour,
	}}
	retryAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	err := errors.New("upstream 429: Retry-After: " + retryAt.Format(http.TimeFormat))

	got := provider.calculateBackoff(3, err)
	if got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("backoff = %s, want about 1h from Retry-After HTTP-date", got)
	}
}

func TestRetryProvider_BackoffDoesNotCapRetryAfterHTTPDate(t *testing.T) {
	provider := &RetryProvider{config: RetryConfig{
		BaseBackoff: time.Second,
		MaxBackoff:  30 * time.Second,
	}}
	retryAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	err := errors.New("upstream 429: Retry-After: " + retryAt.Format(http.TimeFormat))

	got := provider.calculateBackoff(1, err)
	if got < 59*time.Minute || got > 61*time.Minute {
		t.Fatalf("backoff = %s, want uncapped Retry-After HTTP-date of about 1h", got)
	}
}

func TestRetryProviderForwardsListModels(t *testing.T) {
	inner := &listModelsProvider{models: []ModelInfo{{ID: "a"}, {ID: "b"}}}
	wrapped := WrapWithRetry(inner, DefaultRetryConfig())

	lister, ok := wrapped.(interface {
		ListModels(context.Context) ([]ModelInfo, error)
	})
	if !ok {
		t.Fatal("wrapped provider does not satisfy ListModels interface")
	}
	got, err := lister.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("ListModels returned %+v, want [{a} {b}]", got)
	}
	if inner.calls != 1 {
		t.Fatalf("inner.ListModels called %d times, want 1", inner.calls)
	}
}

type retryingListModelsProvider struct {
	calls int
}

func (p *retryingListModelsProvider) Name() string               { return "retrying-list-models" }
func (p *retryingListModelsProvider) Credential() string         { return "mock" }
func (p *retryingListModelsProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *retryingListModelsProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return nil, errors.New("not used in this test")
}
func (p *retryingListModelsProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	p.calls++
	if p.calls == 1 {
		return nil, newHTTPStatusErrorString("test", http.StatusServiceUnavailable, "503 Service Unavailable", nil, "try again")
	}
	return []ModelInfo{{ID: "ok"}}, nil
}

func TestRetryProviderRetriesListModelsTransientError(t *testing.T) {
	inner := &retryingListModelsProvider{}
	wrapped := WrapWithRetry(inner, RetryConfig{
		MaxAttempts: 2,
		BaseBackoff: time.Millisecond,
		MaxBackoff:  time.Millisecond,
	})
	lister := wrapped.(interface {
		ListModels(context.Context) ([]ModelInfo, error)
	})
	models, err := lister.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if inner.calls != 2 {
		t.Fatalf("ListModels calls = %d, want 2", inner.calls)
	}
	if len(models) != 1 || models[0].ID != "ok" {
		t.Fatalf("models = %+v, want ok", models)
	}
}

func TestRetryProviderListModelsUnsupportedWhenInnerLacksMethod(t *testing.T) {
	wrapped := WrapWithRetry(&noListModelsProvider{}, DefaultRetryConfig())
	lister := wrapped.(interface {
		ListModels(context.Context) ([]ModelInfo, error)
	})
	_, err := lister.ListModels(context.Background())
	if !errors.Is(err, ErrListModelsUnsupported) {
		t.Fatalf("got err %v, want ErrListModelsUnsupported", err)
	}
}

func TestRetryProvider_ResponseHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			// Hold headers until the transport cancels the timed-out request.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	defer close(release)
	client := newStreamingHTTPClient()
	client.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
	defer client.CloseIdleConnections()
	original := defaultHTTPClient
	defaultHTTPClient = client
	defer func() { defaultHTTPClient = original }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	provider := WrapWithRetry(NewOpenAICompatProvider(server.URL, "", "test-model", "Test"), RetryConfig{
		MaxAttempts: 2, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
	})
	stream, err := provider.Stream(ctx, Request{Messages: []Message{UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var text strings.Builder
	var retries int
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == EventError {
			t.Fatalf("stream failed after %d requests (parent error: %v): %v", requests.Load(), ctx.Err(), event.Err)
		}
		if event.Type == EventTextDelta {
			text.WriteString(event.Text)
		}
		if event.Type == EventRetry {
			retries++
		}
	}
	if requests.Load() != 2 || retries != 1 || text.String() != "hello" {
		t.Fatalf("requests=%d retries=%d text=%q; want 2, 1, hello", requests.Load(), retries, text.String())
	}
	if ctx.Err() != nil {
		t.Fatalf("parent context expired: %v", ctx.Err())
	}
}

func TestRetryCall_DeadlineErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		expired      bool
		canceled     bool
		committed    bool
		budget       time.Duration
		wantAttempts int
	}{
		{name: "live parent", wantAttempts: 2},
		{name: "expired parent", expired: true, wantAttempts: 1},
		{name: "canceled parent", canceled: true, wantAttempts: 1},
		{name: "committed timeout", committed: true, wantAttempts: 1},
		{name: "exhausted budget", budget: time.Nanosecond, wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.expired {
				var expire context.CancelFunc
				ctx, expire = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer expire()
			}
			var attempts, retries int
			_, err := retryCall(ctx, RetryConfig{
				MaxAttempts: 2, MaxElapsedTime: tc.budget, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
			}, func() (struct{}, error) {
				attempts++
				if attempts == 2 {
					return struct{}{}, nil
				}
				if tc.canceled {
					cancel()
				}
				err := fmt.Errorf("request failed: %w", context.DeadlineExceeded)
				if tc.committed {
					err = &committedError{err}
				}
				return struct{}{}, err
			}, func(retryInfo) error { retries++; return nil })
			if attempts != tc.wantAttempts || retries != tc.wantAttempts-1 {
				t.Fatalf("attempts=%d retries=%d; want %d, %d", attempts, retries, tc.wantAttempts, tc.wantAttempts-1)
			}
			if tc.wantAttempts == 2 {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected error")
			}
			if tc.canceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
		})
	}
}
