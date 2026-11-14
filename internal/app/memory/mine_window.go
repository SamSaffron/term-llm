package memory

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	memorydb "github.com/term-llm/term-llm/internal/memory"
	"github.com/term-llm/term-llm/internal/session"
)

// A range describes actual evidence supplied to extraction, not the raw cursor
// interval (which may also contain deliberately skipped out-of-window messages).
type memoryEvidenceRange struct{ Start, End int }

// memoryMessageBlockedError is a session-local content refusal. Store/query
// failures remain ordinary errors and can still abort the walk.
type memoryMessageBlockedError struct {
	Sequence int
	Reason   string
}

func (e *memoryMessageBlockedError) Error() string {
	return fmt.Sprintf("message at sequence %d %s; cursor retained", e.Sequence, e.Reason)
}

func (opts MineOptions) hasMessageBounds() bool {
	return !opts.MessageSince.IsZero() || !opts.MessageBefore.IsZero()
}

func (opts MineOptions) validateMessageBounds() error {
	if !opts.MessageSince.IsZero() && !opts.MessageBefore.IsZero() && !opts.MessageSince.Before(opts.MessageBefore) {
		return fmt.Errorf("--message-since must be earlier than --message-before")
	}
	if opts.hasMessageBounds() && opts.Insights {
		return fmt.Errorf("--insights is not supported with message time bounds; run it separately")
	}
	return nil
}

// Upper bounds are sequence barriers, not skips: a later sequence can have an
// earlier timestamp. Leave the boundary AND all later sequences unread so a
// subsequent invocation without that upper bound can process them without loss.
func (opts MineOptions) messageScope(msg session.Message) (skip, stop bool, err error) {
	if !opts.hasMessageBounds() {
		return false, false, nil
	}
	if msg.CreatedAt.IsZero() {
		return false, false, &memoryMessageBlockedError{msg.Sequence, "has no timestamp"}
	}
	if !opts.MessageBefore.IsZero() && !msg.CreatedAt.Before(opts.MessageBefore) {
		return false, true, nil
	}
	return !opts.MessageSince.IsZero() && msg.CreatedAt.Before(opts.MessageSince), false, nil
}

func (r *memoryMineLoadResult) addEvidence(sequence int) {
	n := len(r.EvidenceRanges)
	if n > 0 && r.EvidenceRanges[n-1].End == sequence {
		r.EvidenceRanges[n-1].End = sequence + 1
		return
	}
	r.EvidenceRanges = append(r.EvidenceRanges, memoryEvidenceRange{sequence, sequence + 1})
}

func (opts MineOptions) reportMessageScope(number int64, r memoryMineLoadResult) {
	if !opts.hasMessageBounds() {
		return
	}
	suffix := ""
	if r.UpperBoundary {
		suffix = " (later sequences unexamined at upper boundary)"
	}
	fmt.Printf("#%d message scope: skipped_before=%d evidence_ranges=%v upper_boundary=%t next_offset=%d%s\n", number, r.SkippedBefore, r.EvidenceRanges, r.UpperBoundary, r.NextOffset, suffix)
}

func (opts MineOptions) boundedPromptFits(candidate memoryMineCandidate, start, end int, messages []session.Message, taxonomy string) bool {
	parts := opts.extractionPrompt(candidate, start, memoryMineLoadResult{NextOffset: end, Messages: messages}, taxonomy)
	return engine.EstimateTokens(memoryExtractionSystemPrompt)+engine.EstimateTokens(parts.Prompt) <= opts.PromptMaxTokens
}

func (opts MineOptions) extractionPrompt(candidate memoryMineCandidate, start int, r memoryMineLoadResult, taxonomy string) memoryPromptParts {
	parts := buildExtractionPromptParts(protectionOrDefault(opts.protection).candidate(candidate), start, r.NextOffset, r.Messages, taxonomy)
	if opts.hasMessageBounds() {
		// Explicitly distinguish raw checkpoint traversal from the actual evidence.
		parts.Prompt += fmt.Sprintf("\nMessage evidence scope: since=%s (inclusive), before=%s (exclusive); Only timestamp-filtered evidence is supplied. The raw checkpoint range is not a claim all messages were mined.\n", formatMessageBound(opts.MessageSince), formatMessageBound(opts.MessageBefore))
	}
	return parts
}

func formatMessageBound(t time.Time) string {
	if t.IsZero() {
		return "none"
	}
	return t.Format(time.RFC3339Nano)
}

func (opts MineOptions) recordExtractionSources(ctx context.Context, store *memorydb.Store, candidate memoryMineCandidate, start int, r memoryMineLoadResult, paths []string) {
	ranges := r.EvidenceRanges
	if !opts.hasMessageBounds() {
		ranges = []memoryEvidenceRange{{start, r.NextOffset}}
	}
	for _, p := range paths {
		for _, span := range ranges {
			if err := store.AddFragmentSource(ctx, candidate.Agent, p, candidate.Session.ID, span.Start, span.End); err != nil {
				if !opts.hasMessageBounds() {
					// Preserve the established default-mode diagnostics.
					fmt.Fprintf(os.Stderr, "warning: record fragment source %s: %v\n", p, err)
				} else {
					fmt.Fprintf(os.Stderr, "warning: record memory source failed for evidence range [%d,%d)\n", span.Start, span.End)
				}
			}
		}
	}
}

func hasTextEvidence(messages []session.Message) bool {
	for _, msg := range messages {
		if !excludedMemoryEvidenceRole(msg.Role) && msg.Role != core.RoleTool && strings.TrimSpace(msg.TextContent) != "" {
			return true
		}
	}
	return false
}
