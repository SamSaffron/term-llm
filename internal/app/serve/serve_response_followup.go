package serve

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/session"
)

// claimUIFollowUp claims only first-party client messages that this runtime
// owns. The returned lease explicitly owns release until transferred to a run.
func (s *serveServer) claimUIFollowUp(ctx context.Context, rFirstParty bool, rt *serveRuntime, ownsSession bool, sessionID string, inputMessages []core.Message) (*followUpClaimLease, error) {
	ids := responseClientMessageIDs(inputMessages)
	if !ownsSession || len(ids) == 0 || !rFirstParty || rt == nil {
		return nil, nil
	}
	rt.mu.Lock()
	if rt.store != nil && !rt.ensurePersistedSession(ctx, sessionID, inputMessages) {
		rt.mu.Unlock()
		return nil, errors.New("failed to hydrate session history before client message claim")
	}
	history := copyLLMMessageSlice(rt.history)
	rt.mu.Unlock()
	trailing := len(history)
	for trailing > 0 && history[trailing-1].Role == core.RoleUser {
		trailing--
	}
	found := make(map[string]bool, len(ids))
	unanswered := make(map[string]bool, len(ids))
	for i := range history {
		id := strings.TrimSpace(history[i].ClientMessageID)
		if id == "" {
			continue
		}
		found[id] = true
		if i >= trailing {
			unanswered[id] = true
		}
	}
	durable := make(map[string]*session.Message)
	if rt.store != nil {
		var err error
		durable, err = session.FindMessagesByClientMessageIDs(ctx, rt.store, sessionID, ids)
		if err != nil {
			return nil, fmt.Errorf("lookup client_message_ids: %w", err)
		}
	}
	for _, id := range ids {
		_, exists := durable[id]
		if (found[id] || exists) && !unanswered[id] {
			return nil, fmt.Errorf("%w: %q", errResponseClientMessageAlreadyCommitted, id)
		}
	}
	claims := make([]engine.SteeringClaimStatus, len(ids))
	for i := range claims {
		claims[i] = engine.SteeringClaimNotFound
	}
	if rt.engine != nil {
		claims = rt.claimSteering(ids)
	}
	newClaim, existing := false, false
	for _, claim := range claims {
		newClaim = newClaim || claim == engine.SteeringClaimed
		existing = existing || claim == engine.SteeringClaimFollowUpOwned
	}
	if newClaim && existing {
		return nil, errServeSessionBusy
	}
	claimed := make([]string, 0, len(ids))
	for i, claim := range claims {
		switch claim {
		case engine.SteeringClaimed:
			claimed = append(claimed, ids[i])
		case engine.SteeringClaimRushOwned:
			return nil, errServeSessionBusy
		case engine.SteeringClaimCommitted:
			return nil, fmt.Errorf("%w: %q", errResponseClientMessageAlreadyCommitted, ids[i])
		case engine.SteeringClaimFollowUpOwned:
			if !unanswered[ids[i]] {
				return nil, errServeSessionBusy
			}
		}
	}
	if len(claimed) == 0 {
		return nil, nil
	}
	return &followUpClaimLease{release: func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rt.releaseClaimedPendingSteering(releaseCtx, sessionID, claimed)
	}}, nil
}
