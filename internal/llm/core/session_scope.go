package core

import "context"

type lockedSessionKey struct{}

// ContextWithLockedSession records that the host admitted sessionID as a locked
// conversation for this run. Only host admission code may call this; model
// output and tool arguments never reach it.
func ContextWithLockedSession(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, lockedSessionKey{}, sessionID)
}

// LockedSessionFromContext returns the admitted locked session ID, or "" when
// the run was not admitted as locked or the engine session differs.
func LockedSessionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(lockedSessionKey{}).(string)
	if id == "" {
		return ""
	}
	if engineID := SessionIDFromContext(ctx); engineID != "" && engineID != id {
		return ""
	}
	return id
}
