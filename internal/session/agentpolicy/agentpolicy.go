package agentpolicy

import (
	"context"
	"errors"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

type agentRecoveryNoticeKey struct{}

// ContextWithAgentRecoveryNotice binds a host-authored lifecycle turn to the
// exact child generations included in its trusted lifecycle metadata.
func ContextWithAgentRecoveryNotice(ctx context.Context, events ...session.AgentRun) context.Context {
	generations := make(map[string]int64, len(events))
	for _, event := range events {
		generations[event.ID] = event.RunGeneration
	}
	return context.WithValue(ctx, agentRecoveryNoticeKey{}, generations)
}

// AgentRunLifecycleRestartAllowed requires an exact offered generation, the
// durable per-child opt-in, and external proof that the previous owner is dead.
func AgentRunLifecycleRestartAllowed(ctx context.Context, run session.AgentRun, ownerDead bool) bool {
	generations, _ := ctx.Value(agentRecoveryNoticeKey{}).(map[string]int64)
	generation, offered := generations[run.ID]
	return offered && generation == run.RunGeneration && ownerDead && run.Status == "interrupted" && run.StopReason == "host_restarted" && run.AutoResumeOnRestart
}

// IsAgentRecoveryNotice reports whether this is a host lifecycle turn.
func IsAgentRecoveryNotice(ctx context.Context) bool {
	_, ok := ctx.Value(agentRecoveryNoticeKey{}).(map[string]int64)
	return ok
}

func ClaimAgentRunRestart(ctx context.Context, store session.AgentRunStore, id, parent string, generation int64, previousOwner, newOwner string, previousOwnerDead bool, at time.Time) error {
	if !previousOwnerDead {
		return errors.New("agent restart refused because the previous owner is not proven dead")
	}
	claimStore, ok := store.(session.AgentRunRestartClaimStore)
	if !ok {
		return errors.New("agent restart claim is unavailable")
	}
	claimed, err := claimStore.ClaimAgentRunRestart(ctx, id, parent, generation, previousOwner, newOwner, at)
	if err != nil {
		return err
	}
	if !claimed {
		return errors.New("agent restart refused because its policy, generation, owner, or parent stop state changed")
	}
	return nil
}

// UpdateAgentRunRestartPolicy changes only the exact current child generation.
func UpdateAgentRunRestartPolicy(ctx context.Context, store session.AgentRunStore, run session.AgentRun, parent string, enabled bool) (session.AgentRun, error) {
	policyStore, ok := store.(session.AgentRunRestartPolicyStore)
	if !ok {
		return run, errors.New("durable child restart policy updates are unavailable for this session store")
	}
	updated, err := policyStore.SetAgentRunRestartPolicy(ctx, run.ID, parent, run.RunGeneration, enabled)
	if err != nil {
		return run, err
	}
	if !updated {
		return run, errors.New("child restart policy was not changed because its generation is no longer current or the parent stopped")
	}
	run.AutoResumeOnRestart = enabled
	return run, nil
}

type lifecycleRestrictedKey struct{}

// ContextWithLifecycleRestricted prevents lifecycle reserve turns from
// creating or continuing work, even if the provider emits those tool calls.
func ContextWithLifecycleRestricted(ctx context.Context) context.Context {
	return context.WithValue(ctx, lifecycleRestrictedKey{}, true)
}

func LifecycleRestricted(ctx context.Context) bool {
	restricted, _ := ctx.Value(lifecycleRestrictedKey{}).(bool)
	return restricted
}
