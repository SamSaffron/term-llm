package llm

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RetryConfig configures retry behavior.
type RetryConfig struct {
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// DefaultRetryConfig returns sensible defaults for rate limit retries.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts: 5,
		BaseBackoff: 1 * time.Second,
		MaxBackoff:  30 * time.Second,
	}
}

// RetryProvider wraps a provider with automatic retry on transient errors.
type RetryProvider struct {
	inner  Provider
	config RetryConfig
}

// WrapWithRetry wraps a provider with retry logic.
func WrapWithRetry(p Provider, config RetryConfig) Provider {
	return &RetryProvider{inner: p, config: config}
}

func (r *RetryProvider) Name() string {
	return r.inner.Name()
}

func (r *RetryProvider) Credential() string {
	return r.inner.Credential()
}

func (r *RetryProvider) Capabilities() Capabilities {
	return r.inner.Capabilities()
}

// SetToolExecutor forwards to the inner provider if it implements ToolExecutorSetter.
// This ensures providers like ClaudeBinProvider can receive their tool executor
// even when wrapped with retry logic.
func (r *RetryProvider) SetToolExecutor(executor func(ctx context.Context, name string, args json.RawMessage) (string, error)) {
	if setter, ok := r.inner.(ToolExecutorSetter); ok {
		setter.SetToolExecutor(executor)
	}
}

func (r *RetryProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, events chan<- Event) error {
		var lastErr error

		for attempt := 1; attempt <= r.config.MaxAttempts; attempt++ {
			stream, err := r.inner.Stream(ctx, req)
			if err != nil {
				// Error creating stream
				if !isRetryable(err) {
					return err
				}
				lastErr = err
			} else {
				// Stream created, forward events (may also fail with retryable error)
				err = r.forwardEvents(ctx, stream, events)
				if err == nil {
					return nil // Success!
				}
				if !isRetryable(err) {
					return err
				}
				lastErr = err
			}

			// Don't retry if context is already cancelled
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// Don't retry if this was the last attempt
			if attempt >= r.config.MaxAttempts {
				break
			}

			wait := r.calculateBackoff(attempt, lastErr)

			// Emit retry event so UI can show progress
			events <- Event{
				Type:             EventRetry,
				RetryAttempt:     attempt,
				RetryMaxAttempts: r.config.MaxAttempts,
				RetryWaitSecs:    wait.Seconds(),
			}

			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}

		return lastErr
	}), nil
}

// forwardEvents reads events from the inner stream and forwards them.
// Returns a retryable error if the stream fails with a transient error.
func (r *RetryProvider) forwardEvents(ctx context.Context, stream Stream, events chan<- Event) error {
	defer stream.Close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		event, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Check for error events from the stream (e.g., 429 during streaming)
		if event.Type == EventError && event.Err != nil {
			return event.Err
		}

		select {
		case events <- event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// isRetryable returns true if the error is a transient error worth retrying.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Check for RateLimitError - only retry if it's a short wait
	if rle, ok := err.(*RateLimitError); ok {
		return !rle.IsLongWait()
	}

	errStr := strings.ToLower(err.Error())

	// HTTP status codes and rate limit messages
	if strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "high concurrency") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "overloaded") {
		return true
	}

	// Connection errors
	if strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "temporary failure") ||
		strings.Contains(errStr, "no such host") {
		return true
	}

	return false
}

// retryAfterRegex matches Retry-After values in error messages.
var retryAfterRegex = regexp.MustCompile(`(?i)retry[- ]?after[:\s]+(\d+)`)

// calculateBackoff computes the wait duration for a retry attempt.
func (r *RetryProvider) calculateBackoff(attempt int, err error) time.Duration {
	// Check for RateLimitError with explicit RetryAfter
	if rle, ok := err.(*RateLimitError); ok && rle.RetryAfter > 0 {
		wait := rle.RetryAfter
		// Cap at max backoff for automatic retries
		if wait > r.config.MaxBackoff {
			wait = r.config.MaxBackoff
		}
		return wait
	}

	// Try to parse Retry-After from error message
	if err != nil {
		if matches := retryAfterRegex.FindStringSubmatch(err.Error()); len(matches) > 1 {
			if secs, parseErr := strconv.Atoi(matches[1]); parseErr == nil && secs > 0 {
				wait := time.Duration(secs) * time.Second
				// Cap at max backoff
				if wait > r.config.MaxBackoff {
					wait = r.config.MaxBackoff
				}
				return wait
			}
		}
	}

	// Exponential backoff: base * 2^(attempt-1)
	backoff := float64(r.config.BaseBackoff) * math.Pow(2, float64(attempt-1))

	// Add jitter: +/- 25%
	jitter := (rand.Float64() - 0.5) * 0.5 * backoff
	backoff += jitter

	// Cap at max backoff
	if backoff > float64(r.config.MaxBackoff) {
		backoff = float64(r.config.MaxBackoff)
	}

	return time.Duration(backoff)
}
