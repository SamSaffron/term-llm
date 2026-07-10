package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"regexp"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/providerhttp"
)

// RetryConfig configures retry behavior.
type RetryConfig struct {
	// MaxAttempts limits total attempts. A value of 0 means there is no
	// attempt-count limit and retries are governed by MaxElapsedTime.
	MaxAttempts int
	// MaxElapsedTime limits the retry window from the first attempt. A value of
	// 0 disables the elapsed-time budget (useful for tests/custom fixed attempts).
	MaxElapsedTime time.Duration
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
}

// DefaultRetryConfig returns sensible defaults for transient provider retries.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:    0,
		MaxElapsedTime: 30 * time.Minute,
		BaseBackoff:    1 * time.Second,
		MaxBackoff:     30 * time.Second,
	}
}

// RetryProvider wraps a provider with automatic retry on transient errors.
type RetryProvider struct {
	inner  Provider
	config RetryConfig
}

// WrapWithRetry wraps a provider with retry logic.
func WrapWithRetry(p Provider, config RetryConfig) Provider {
	return &RetryProvider{inner: p, config: normalizeRetryConfig(config)}
}

func normalizeRetryConfig(config RetryConfig) RetryConfig {
	zeroConfig := config == (RetryConfig{})
	if zeroConfig {
		// Preserve the zero-value contract for custom/test callers: perform the
		// initial provider call but do not retry. Production providers opt into the
		// time-budgeted policy via DefaultRetryConfig().
		config.MaxAttempts = 1
	}
	if config.MaxAttempts < 0 {
		config.MaxAttempts = 0
	}
	if config.BaseBackoff <= 0 {
		config.BaseBackoff = time.Second
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = 30 * time.Second
	}
	if config.MaxBackoff < config.BaseBackoff {
		config.MaxBackoff = config.BaseBackoff
	}
	return config
}

// RateLimitError represents a rate limit error with retry information.
type RateLimitError struct {
	Message        string
	RetryAfter     time.Duration
	PlanType       string
	PrimaryUsed    int
	PrimaryLimit   int
	SecondaryUsed  int
	SecondaryLimit int
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "rate limit"
	}
	return e.Message
}

// RetryAfterDelay exposes structured Retry-After metadata to the retry loop.
func (e *RateLimitError) RetryAfterDelay() (time.Duration, bool) {
	if e == nil || e.RetryAfter <= 0 {
		return 0, false
	}
	return e.RetryAfter, true
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

// ExportProviderState forwards opaque conversation transport state when the
// wrapped provider supports persistence.
func (r *RetryProvider) ExportProviderState() ([]byte, bool) {
	if exporter, ok := r.inner.(ProviderStateExporter); ok {
		return exporter.ExportProviderState()
	}
	return nil, false
}

// ImportProviderState restores opaque conversation transport state on the
// wrapped provider.
func (r *RetryProvider) ImportProviderState(data []byte) error {
	if importer, ok := r.inner.(ProviderStateImporter); ok {
		return importer.ImportProviderState(data)
	}
	return fmt.Errorf("provider %q does not support provider state import", r.inner.Name())
}

// SetToolExecutor forwards to the inner provider if it implements ToolExecutorSetter.
// This ensures CLI bridge providers can receive their tool executor
// even when wrapped with retry logic.
func (r *RetryProvider) SetToolExecutor(executor func(ctx context.Context, name string, args json.RawMessage) (ToolOutput, error)) {
	if setter, ok := r.inner.(ToolExecutorSetter); ok {
		setter.SetToolExecutor(executor)
	}
}

// CleanupMCP forwards to the inner provider if it implements ProviderCleaner.
// This ensures stateful providers get cleaned up properly
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

// ListModels forwards to the inner provider if it implements model listing and
// applies the same retry policy as Stream for transient HTTP/provider failures.
// Without this forwarder, callers that type-assert on a ListModels interface
// would silently miss it on any retry-wrapped provider — and every provider
// built via NewProvider/NewProviderByName is retry-wrapped.
func (r *RetryProvider) ListModels(ctx context.Context) ([]ModelInfo, error) {
	if lister, ok := r.inner.(interface {
		ListModels(context.Context) ([]ModelInfo, error)
	}); ok {
		return retryCall(ctx, r.config, func() ([]ModelInfo, error) {
			return lister.ListModels(ctx)
		}, nil)
	}
	return nil, ErrListModelsUnsupported
}

func (r *RetryProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	config := normalizeRetryConfig(r.config)
	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		_, err := retryCall(ctx, config, func() (struct{}, error) {
			stream, err := r.inner.Stream(ctx, req)
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, r.forwardAttempt(ctx, stream, send)
		}, func(info retryInfo) error {
			// Emit retry event so UI can show progress. RetryMaxAttempts==0 means
			// time-budgeted retry with no fixed attempt ceiling.
			return send.Send(Event{
				Type:             EventRetry,
				RetryAttempt:     info.Attempt,
				RetryMaxAttempts: info.MaxAttempts,
				RetryWaitSecs:    info.Wait.Seconds(),
			})
		})
		return err
	}), nil
}

type retryInfo struct {
	Attempt     int
	MaxAttempts int
	Wait        time.Duration
}

func retryCall[T any](ctx context.Context, config RetryConfig, run func() (T, error), onRetry func(retryInfo) error) (T, error) {
	config = normalizeRetryConfig(config)
	started := time.Now()
	var zero T
	var lastErr error

	for attempt := 1; ; attempt++ {
		if config.MaxAttempts > 0 && attempt > config.MaxAttempts {
			break
		}

		result, err := run()
		if err == nil {
			return result, nil
		}
		if !isRetryable(err) {
			return zero, err
		}
		lastErr = err

		// Don't retry if context is already cancelled.
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}

		// Don't retry if this was the last count-limited attempt.
		if config.MaxAttempts > 0 && attempt >= config.MaxAttempts {
			break
		}

		wait := calculateRetryBackoff(config, attempt, lastErr)
		if err := checkRetryBudget(started, config.MaxElapsedTime, wait, lastErr); err != nil {
			return zero, err
		}

		if onRetry != nil {
			if err := onRetry(retryInfo{Attempt: attempt, MaxAttempts: config.MaxAttempts, Wait: wait}); err != nil {
				return zero, err
			}
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(wait):
		}
	}

	return zero, lastErr
}

func checkRetryBudget(started time.Time, maxElapsedTime time.Duration, wait time.Duration, lastErr error) error {
	if maxElapsedTime <= 0 {
		return nil
	}
	remaining := maxElapsedTime - time.Since(started)
	if remaining <= 0 {
		return fmt.Errorf("%w (retry window %s exhausted)", lastErr, maxElapsedTime)
	}
	if wait <= remaining {
		return nil
	}
	if hasRetryAfter(lastErr) {
		return fmt.Errorf("%w (retry-after %s exceeds remaining retry window %s; max retry window %s)", lastErr, wait.Round(time.Millisecond), remaining.Round(time.Millisecond), maxElapsedTime)
	}
	return fmt.Errorf("%w (next retry wait %s exceeds remaining retry window %s; max retry window %s)", lastErr, wait.Round(time.Millisecond), remaining.Round(time.Millisecond), maxElapsedTime)
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

	// Structured HTTP status errors from providers.
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) {
		return providerhttp.RetryableStatus(statusErr.HTTPStatusCode())
	}

	// Check for structured rate-limit errors. Long waits are handled by the
	// elapsed-time budget in the retry loop rather than by retryability.
	var rle *RateLimitError
	if errors.As(err, &rle) {
		return true
	}

	errStr := strings.ToLower(err.Error())

	// HTTP status codes and rate limit messages
	if strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "too many requests") ||
		strings.Contains(errStr, "high concurrency") ||
		containsHTTP5xxStatus(errStr) ||
		strings.Contains(errStr, "internal server error") ||
		strings.Contains(errStr, "bad gateway") ||
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

// http5xxStatusRegex matches standalone HTTP 5xx status codes in legacy string
// errors from providers that have not attached structured status metadata.
var http5xxStatusRegex = regexp.MustCompile(`\b5\d\d\b`)

func containsHTTP5xxStatus(message string) bool {
	return http5xxStatusRegex.MatchString(message)
}

// retryAfterHeaderRegex matches Retry-After header-like values in error messages.
var retryAfterHeaderRegex = regexp.MustCompile(`(?im)retry[- ]?after(?:[- ]?(ms))?[:\s]+([^\r\n]+)`)

func parseRetryAfterDelay(message string, now time.Time) (time.Duration, bool) {
	matches := retryAfterHeaderRegex.FindStringSubmatch(message)
	if len(matches) <= 2 {
		return 0, false
	}
	unit := strings.ToLower(strings.TrimSpace(matches[1]))
	value := strings.TrimSpace(matches[2])
	if value == "" {
		return 0, false
	}
	if unit == "ms" {
		return providerhttp.ParseRetryAfterMillisecondsValue(value)
	}
	return providerhttp.ParseRetryAfterValue(value, now)
}

type retryAfterError interface {
	RetryAfterDelay() (time.Duration, bool)
}

func retryAfterDelay(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	var typed retryAfterError
	if errors.As(err, &typed) {
		if wait, ok := typed.RetryAfterDelay(); ok {
			return wait, true
		}
	}
	return parseRetryAfterDelay(err.Error(), time.Now())
}

func hasRetryAfter(err error) bool {
	_, ok := retryAfterDelay(err)
	return ok
}

// calculateBackoff computes the wait duration for a retry attempt.
func (r *RetryProvider) calculateBackoff(attempt int, err error) time.Duration {
	return calculateRetryBackoff(r.config, attempt, err)
}

func calculateRetryBackoff(config RetryConfig, attempt int, err error) time.Duration {
	// Prefer structured Retry-After metadata from provider errors, then fall back
	// to parsing wrapped/stringified errors. Retry-After is an explicit server
	// instruction and is not capped by MaxBackoff; the retry loop's elapsed-time
	// budget decides whether it is too long to honor.
	if wait, ok := retryAfterDelay(err); ok {
		return wait
	}

	// Exponential backoff with jitter: base * 2^(attempt-1) * jitter (jitter in [0.5, 1.5])
	base := config.BaseBackoff
	if base <= 0 {
		base = time.Second
	}
	maxBackoff := config.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	jitter := 0.5 + rand.Float64()
	exponent := max(attempt-1, 0)
	delayFloat := math.Ldexp(float64(base), exponent) * jitter
	if delayFloat >= float64(maxBackoff) {
		return maxBackoff
	}
	delay := time.Duration(delayFloat)

	// Cap fallback exponential backoff.
	if delay > maxBackoff {
		delay = maxBackoff
	}

	return delay
}
