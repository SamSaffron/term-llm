package tools

import "context"

// InitiateHandoverFunc triggers the handover UI for a target agent.
// It blocks until the user confirms or cancels.
// Returns true if the user confirmed, false if cancelled.
type InitiateHandoverFunc func(ctx context.Context, agent string) (confirmed bool, err error)

type handoverUIContextKey struct{}

// ContextWithHandoverFunc stores a request-scoped handover handler in ctx.
func ContextWithHandoverFunc(ctx context.Context, fn InitiateHandoverFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, handoverUIContextKey{}, fn)
}

func handoverFuncFromContext(ctx context.Context) InitiateHandoverFunc {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(handoverUIContextKey{}).(InitiateHandoverFunc)
	return fn
}
