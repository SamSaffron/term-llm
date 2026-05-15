package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"net/http"
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

// ResetConversation forwards to the inner provider if it implements
// ResetConversation. This preserves provider-side conversation reset behavior
// when providers are wrapped with retry logic.
func (r *RetryProvider) ResetConversation() {
	if resetter, ok := r.inner.(interface{ ResetConversation() }); ok {
		resetter.ResetConversation()
	}
}

// SetToolExecutor forwards to the inner provider if it implements ToolExecutorSetter.
// This ensures providers like ClaudeBinProvider can receive their tool executor
// even when wrapped with retry logic.
func (r *RetryProvider) SetToolExecutor(executor func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error)) {
	if setter, ok := r.inner.(ToolExecutorSetter); ok {
		setter.SetToolExecutor(executor)
	}
}

// CleanupMCP forwards to the inner provider if it implements ProviderCleaner.
// This ensures providers like ClaudeBinProvider get cleaned up properly
// even when wrapped with retry logic.
func (r *RetryProvider) CleanupMCP() {
	if cleaner, ok := r.inner.(ProviderCleaner); ok {
		cleaner.CleanupMCP()
	}
}

// CleanupTurn forwards to the inner provider if it implements
// ProviderTurnCleaner so per-turn cleanup survives retry wrapping.
func (r *RetryProvider) CleanupTurn() {
	if cleaner, ok := r.inner.(ProviderTurnCleaner); ok {
		cleaner.CleanupTurn()
	}
}

// ErrListModelsUnsupported is returned by RetryProvider.ListModels when the
// inner provider does not implement model listing. Callers that prefer a
// curated fallback (the web /v1/models handler) can detect this and fall
// through, while callers that want to surface the limitation (cmd/models.go)
// can report it.
var ErrListModelsUnsupported = errors.New("provider does not support model listing")

// ListModels forwards to the inner provider if it implements model listing.
// Without this forwarder, callers that type-assert on a ListModels interface
// would silently miss it on any retry-wrapped provider — and every provider
// built via NewProvider/NewProviderByName is retry-wrapped.
func (r *RetryProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if lister, ok := r.inner.(interface {
		ListModels(context.Context) ([]ModelInfo, error)
	}); ok {
		return lister.ListModels(ctx)
	}
	return nil, ErrListModelsUnsupported
}

func (r *RetryProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
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
				err = r.forwardAttempt(ctx, stream, send)
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
			if err := send.Send(Event{
				Type:             EventRetry,
				RetryAttempt:     attempt,
				RetryMaxAttempts: r.config.MaxAttempts,
				RetryWaitSecs:    wait.Seconds(),
			}); err != nil {
				return err
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

// committedError wraps an error that occurred after events were already
// forwarded to the outer stream (e.g. after a synchronous tool call was
// executed). Retrying at this point would duplicate side effects, so the
// retry loop must treat this as non-retryable.
type committedError struct{ err error }

func (e *committedError) Error() string { return e.err.Error() }
func (e *committedError) Unwrap() error { return e.err }

// forwardAttempt reads a single inner stream attempt.
//
// Before any externally-visible side effects, events are buffered so retryable
// failures can be retried without leaking partial output into the outer stream.
//
// Buffering stops once the attempt has visibly committed to the caller:
//   - assistant text deltas
//   - reasoning deltas (streamed to the user in real time)
//   - warning-prefixed phase updates (rendered as visible warnings)
//   - interjections injected into the conversation
//   - tool calls, which are durable model actions and may be followed by side effects
//   - synchronous tool requests (EventToolCall with ToolResponse)
//   - provider-native tool execution (EventToolExecStart/End, e.g. web_search)
//
// After that point the attempt has already escaped, so retrying would duplicate
// visible output or side effects. Any subsequent error is wrapped in
// committedError so the retry loop will not retry.
func (r *RetryProvider) forwardAttempt(ctx context.Context, stream Stream, send eventSender) error {
	defer stream.Close()

	var buffered []Event
	live := false

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		event, err := stream.Recv()
		if err == io.EOF {
			if !live {
				return flushEvents(send, buffered)
			}
			return nil
		}
		if err != nil {
			if live {
				return &committedError{err}
			}
			return err
		}

		// Check for error events from the stream (e.g., 429 during streaming)
		if event.Type == EventError && event.Err != nil {
			if live {
				return &committedError{event.Err}
			}
			return event.Err
		}

		if !live && eventRequiresImmediateForwarding(event) {
			if err := flushEvents(send, buffered); err != nil {
				return err
			}
			buffered = nil
			live = true
		}

		if live {
			if err := send.Send(event); err != nil {
				return err
			}
			continue
		}

		buffered = append(buffered, event)
	}
}

func eventRequiresImmediateForwarding(event Event) bool {
	switch event.Type {
	case EventTextDelta, EventInterjection, EventReasoningDelta,
		EventToolExecStart, EventToolExecEnd:
		return true
	case EventPhase:
		return strings.HasPrefix(event.Text, WarningPhasePrefix)
	case EventToolCall:
		return true
	default:
		return false
	}
}

func flushEvents(send eventSender, buffered []Event) error {
	for _, event := range buffered {
		if err := send.Send(event); err != nil {
			return err
		}
	}
	return nil
}

// isRetryable returns true if the error is a transient error worth retrying.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	// Never retry if events were already committed to the outer stream.
	var ce *committedError
	if errors.As(err, &ce) {
		return false
	}

	// Never retry if the context itself has been cancelled or deadline exceeded.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	// Stream framing / terminal marker failures are transient transport failures.
	var incomplete *StreamIncompleteError
	if errors.As(err, &incomplete) {
		return true
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
		strings.Contains(errStr, "500") ||
		strings.Contains(errStr, "internal server error") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "overloaded") ||
		strings.Contains(errStr, "api error: terminated") {
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

// retryAfterHeaderRegex matches Retry-After header-like values in error messages.
var retryAfterHeaderRegex = regexp.MustCompile(`(?im)retry[- ]?after[:\s]+([^\r\n]+)`)

func parseRetryAfterDelay(message string, now time.Time) (time.Duration, bool) {
	matches := retryAfterHeaderRegex.FindStringSubmatch(message)
	if len(matches) <= 1 {
		return 0, false
	}
	value := strings.TrimSpace(matches[1])
	if value == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(value); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second, true
	}
	if fields := strings.Fields(value); len(fields) > 0 {
		first := strings.Trim(fields[0], ",;.")
		if secs, err := strconv.Atoi(first); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second, true
		}
	}
	if when, err := http.ParseTime(value); err == nil {
		wait := time.Until(when)
		if !now.IsZero() {
			wait = when.Sub(now)
		}
		if wait > 0 {
			return wait, true
		}
	}
	return 0, false
}

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

	// Try to parse Retry-After from error message. Accept both numeric
	// seconds and HTTP-date forms because gateways/providers commonly
	// surface raw header text in wrapped errors.
	if err != nil {
		if wait, ok := parseRetryAfterDelay(err.Error(), time.Now()); ok {
			if wait > r.config.MaxBackoff {
				wait = r.config.MaxBackoff
			}
			return wait
		}
	}

	// Exponential backoff with jitter: base * 2^(attempt-1) * jitter (jitter in [0.5, 1.5])
	jitter := 0.5 + rand.Float64()
	multiplier := 1 << max(attempt-1, 0)
	delay := time.Duration(float64(r.config.BaseBackoff) * float64(multiplier) * jitter)

	// Cap at max backoff
	if delay > r.config.MaxBackoff {
		delay = r.config.MaxBackoff
	}

	return delay
}
