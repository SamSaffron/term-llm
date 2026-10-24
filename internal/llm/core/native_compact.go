package core

import (
	"context"
	"errors"
)

// ErrNativeCompactUnavailable reports that a NativeCompactor cannot compact
// right now (for example, no provider-side conversation exists yet). Callers
// should fall back to term-llm's own transcript compaction.
var ErrNativeCompactUnavailable = errors.New("native compaction unavailable")

// NativeCompactor is an optional Provider capability for providers that own
// their conversation state (such as Claude Code sessions resumed by claude-bin)
// and can compact it in place. Native compaction leaves term-llm's persisted
// transcript untouched; only the provider-side context shrinks.
type NativeCompactor interface {
	NativeCompact(ctx context.Context, req NativeCompactRequest) (*NativeCompactResult, error)
}

// NativeCompactRequest describes one native compaction.
type NativeCompactRequest struct {
	// SystemPrompt is the caller's view of the session instructions, used when
	// the provider has not yet sent a turn in this process.
	SystemPrompt string
	// Model optionally overrides the provider's configured model.
	Model string
}

// NativeCompactResult reports the provider-side context size change.
type NativeCompactResult struct {
	PreTokens  int
	PostTokens int
}
