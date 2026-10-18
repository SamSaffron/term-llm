package llm

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/engine"
)

// Deprecated: use engine.InterruptAction.
type InterruptAction = engine.InterruptAction

const (
	InterruptCancel = engine.InterruptCancel
	InterruptSteer  = engine.InterruptSteer
)

// Deprecated: use engine.InterruptActivity.
type InterruptActivity = engine.InterruptActivity

// Deprecated: use engine.Classify.
func Classify(ctx context.Context, provider Provider, prompt string, timeout time.Duration) (string, error) {
	return engine.Classify(ctx, provider, prompt, timeout)
}

// Deprecated: use engine.ClassifyInterruptImmediate.
func ClassifyInterruptImmediate(msg string) (InterruptAction, bool) {
	return engine.ClassifyInterruptImmediate(msg)
}

// Deprecated: use engine.ClassifyInterrupt.
func ClassifyInterrupt(ctx context.Context, provider Provider, msg string, activity InterruptActivity) InterruptAction {
	return engine.ClassifyInterrupt(ctx, provider, msg, activity)
}
