package llm

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type retryStreamingProvider struct {
	attempt int
}

func (p *retryStreamingProvider) Name() string { return "retry-streaming" }

func (p *retryStreamingProvider) Credential() string { return "mock" }

func (p *retryStreamingProvider) Capabilities() Capabilities { return Capabilities{} }

func (p *retryStreamingProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	p.attempt++
	if p.attempt == 1 {
		return newEventStream(ctx, func(ctx context.Context, ch chan<- Event) error {
			ch <- Event{Type: EventTextDelta, Text: "hello"}
			ch <- Event{Type: EventError, Err: &RateLimitError{Message: "rate limit", RetryAfter: 5 * time.Millisecond}}
			return nil
		}), nil
	}
	return newEventStream(ctx, func(ctx context.Context, ch chan<- Event) error {
		ch <- Event{Type: EventTextDelta, Text: "hello"}
		ch <- Event{Type: EventTextDelta, Text: " world"}
		return nil
	}), nil
}

func TestIsRetryable_500InternalServerError(t *testing.T) {
	cases := []struct {
		msg       string
		retryable bool
	}{
		{"anthropic streaming error: POST \"https://api.anthropic.com/v1/messages\": 500 Internal Server Error", true},
		{"500 internal server error", true},
		{"got 500 from upstream", true},
		{"internal server error occurred", true},
		{"400 Bad Request", false},
		{"401 Unauthorized", false},
	}
	for _, tc := range cases {
		got := isRetryable(errors.New(tc.msg))
		if got != tc.retryable {
			t.Errorf("isRetryable(%q) = %v, want %v", tc.msg, got, tc.retryable)
		}
	}
}

func TestRetryProvider_DropsPartialTextFromRetriedAttempt(t *testing.T) {
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
