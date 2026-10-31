package serve

import (
	"context"
	"fmt"
	"sync"
)

type responseEffortChangeKey struct{}

type responseEffortChange struct {
	sessionID, effort string
	once              sync.Once
	err               error
}

// Presence, including an empty effort (provider default), authorizes a change
// for this session only. Children and suspended resumes must not reapply it.
func withResponseEffortChange(ctx context.Context, sessionID, effort string) context.Context {
	return context.WithValue(ctx, responseEffortChangeKey{}, &responseEffortChange{sessionID: sessionID, effort: effort})
}

// acceptResponseEffortChange runs under rt.mu after admission. Effort retargets
// the provider via core.Request; acceptance does not wait for inference success.
func (rt *serveRuntime) acceptResponseEffortChange(ctx context.Context, sessionID string) error {
	change, _ := ctx.Value(responseEffortChangeKey{}).(*responseEffortChange)
	if change == nil || change.sessionID != sessionID || rt.store == nil || sessionID == "" {
		return nil
	}
	change.once.Do(func() { change.err = rt.persistResponseEffortChange(ctx, sessionID, change.effort) })
	return change.err
}

func (rt *serveRuntime) persistResponseEffortChange(ctx context.Context, sessionID, effort string) error {
	// Read fresh metadata rather than overwriting accounting or ownership from
	// the runtime's potentially older sessionMeta snapshot.
	sess, err := rt.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session for reasoning effort change: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session %q not found for reasoning effort change", sessionID)
	}
	if sess.ReasoningEffort != effort {
		sess.ReasoningEffort = effort
		if err := rt.store.Update(ctx, sess); err != nil {
			return fmt.Errorf("persist reasoning effort change: %w", err)
		}
	}
	rt.sessionMeta = sess
	return nil
}
