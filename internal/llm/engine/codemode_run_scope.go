package engine

import "context"

// scopeEngineToolContext prevents a child engine from inheriting its caller's
// leaf execution override or actual-completion lease. The child's tools are
// ordinary calls owned by that engine, not calls inside the parent's script.
func scopeEngineToolContext(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, nestedContextKey{}, struct{}{})
	return context.WithValue(ctx, nestedToolOverrideKey{}, struct{}{})
}
