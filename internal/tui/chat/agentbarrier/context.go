package agentbarrier

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type stopKey struct{}

func ContextWithStop(ctx context.Context, stopped *atomic.Bool) context.Context {
	return context.WithValue(ctx, stopKey{}, stopped)
}

func StopRequested(ctx context.Context) (bool, bool) {
	stopped, ok := ctx.Value(stopKey{}).(*atomic.Bool)
	return ok && stopped.Load(), ok
}

// RunContext preserves human authority and lifecycle routing across view changes.
func RunContext(ctx context.Context, sessionID string, stopped *atomic.Bool, barrier bool, wake func(string)) context.Context {
	ctx = ContextWithStop(core.ContextWithSessionID(ctx, sessionID), stopped)
	ctx = tools.ContextWithQueueAgentOrigin(ctx, tools.QueueAgentOriginContext{Origin: "tui", SessionID: sessionID, ResponseID: parentResponseID(ctx)})
	ctx = tools.ContextWithAgentCompletionWake(ctx, wake)
	if barrier {
		ctx = tools.ContextWithAgentRecoveryNotice(ctx)
	}
	return ctx
}

// Pending excludes deleted/archived parents before reconciling child state.
func Pending(ctx context.Context, store session.Store, parentID string) ([]session.AgentRun, error) {
	parent, err := store.Get(ctx, parentID)
	if errors.Is(err, session.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || parent == nil || parent.Archived {
		return nil, err
	}
	return tools.PendingAgentBarrier(ctx, session.AsAgentRunDeliveryStore(store), parentID)
}

// ProviderSettings restores durable selection, falling back for older sessions.
func ProviderSettings(sess *session.Session, provider, model string) (string, string) {
	if key := strings.TrimSpace(sess.ProviderKey); key != "" {
		provider = key
	}
	if name := strings.TrimSpace(sess.Model); name != "" {
		model = name
	}
	return provider, model
}

// ExecutionContext observes Stop under the caller's run-publication lock. A
// human cancellation before registration must not be lost by detached ownership.
func ExecutionContext(owner context.Context, stopped *atomic.Bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(owner)
	if stopped.Load() {
		cancel()
	}
	return ContextWithStop(ctx, stopped), cancel
}

// CheckRun observes human Stop even while cancellation is waiting on publication.
func CheckRun(ctx context.Context) error {
	if stopped, _ := StopRequested(ctx); stopped {
		return context.Canceled
	}
	return ctx.Err()
}
