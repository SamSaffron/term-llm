package core

import "time"

// RateLimitError carries a provider's structured rate-limit details across
// provider and retry package boundaries.
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
