package serve

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	transcriptstore "github.com/samsaffron/term-llm/internal/session/transcript"
)

func (r *responseRun) appendEvent(event string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendEventLocked(event, payload, false)
}

func beginResponseRunPersistence(ctx context.Context, messages []core.Message) (func(int64, error), int) {
	run, _ := ctx.Value(responseRunContextKey{}).(*responseRun)
	if run == nil || run.persistence == nil {
		return func(int64, error) {}, 0
	}
	keys := transcriptstore.ResponseRunOwnedOutputKeys(run.id, messages)
	if len(keys) == 0 {
		return func(int64, error) {}, 0
	}

	ledger := run.persistence
	ledger.mu.Lock()
	if ledger.inflight == 0 {
		ledger.idle = make(chan struct{})
	}
	ledger.inflight++
	for _, key := range keys {
		if key == "" {
			ledger.nextOutputID++
			key = fmt.Sprintf("write:%d", ledger.nextOutputID)
		}
		ledger.outputKeys[key] = struct{}{}
	}
	reservedOutputCount := len(ledger.outputKeys)
	ledger.mu.Unlock()

	var once sync.Once
	return func(rev int64, persistErr error) {
		once.Do(func() {
			ledger.mu.Lock()
			if rev > ledger.maxRev {
				ledger.maxRev = rev
			}
			if persistErr != nil {
				ledger.failed = true
				if ledger.failureText == "" {
					ledger.failureText = persistErr.Error()
				}
			}
			ledger.inflight--
			if ledger.inflight == 0 {
				close(ledger.idle)
			}
			outputCount := len(ledger.outputKeys)
			ledger.mu.Unlock()
			if persistErr == nil && run.checkpointLifecycle != nil && !run.atomicTranscriptFencing {
				if checkpointErr := run.checkpointLifecycle(rev, outputCount); checkpointErr != nil {
					ledger.mu.Lock()
					ledger.failed = true
					if ledger.failureText == "" {
						ledger.failureText = checkpointErr.Error()
					}
					ledger.mu.Unlock()
					if errors.Is(checkpointErr, session.ErrResponseRunLeaseLost) {
						run.mu.Lock()
						cancelRun := run.cancel
						run.mu.Unlock()
						if cancelRun != nil {
							cancelRun()
						}
					}
				}
			}
		})
	}, reservedOutputCount
}

func runResponseRunPersistence(ctx context.Context, messages []core.Message, persist func(session.ResponseRunFence) (int64, error)) (rev int64, err error) {
	run := responseRunFromContext(ctx)
	if run != nil && run.validateLifecycle != nil {
		if err := run.validateLifecycle(); err != nil {
			return 0, err
		}
	}
	finish, outputCount := beginResponseRunPersistence(ctx, messages)
	defer func() { finish(rev, err) }()
	var fence session.ResponseRunFence
	if run != nil {
		run.mu.Lock()
		fence = session.ResponseRunFence{ResponseID: run.id, OwnerInstanceID: run.ownerInstanceID,
			FencingToken: run.fencingToken, DurableOutputCount: outputCount}
		run.mu.Unlock()
	}
	return persist(fence)
}

func (r *responseRun) readDurableHandoff() responseRunDurableHandoff {
	return r.readDurableHandoffWithTimeout(responseRunRevisionReadTimeout)
}

func (r *responseRun) readDurableHandoffWithTimeout(timeout time.Duration) responseRunDurableHandoff {
	if r == nil || r.persistence == nil {
		return responseRunDurableHandoff{Valid: true}
	}
	ledger := r.persistence
	if timeout <= 0 {
		timeout = responseRunRevisionReadTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		ledger.mu.Lock()
		if ledger.inflight == 0 {
			break
		}
		idle := ledger.idle
		ledger.mu.Unlock()
		select {
		case <-idle:
			continue
		case <-timer.C:
			ledger.mu.Lock()
			handoff := responseRunDurableHandoff{
				Valid:       false,
				FinalRev:    ledger.maxRev,
				OutputCount: len(ledger.outputKeys),
				Error:       "persistence barrier timed out",
			}
			ledger.mu.Unlock()
			return handoff
		}
	}
	outputCount := len(ledger.outputKeys)
	maxRev := ledger.maxRev
	failed := ledger.failed
	errorText := ledger.failureText
	ledger.mu.Unlock()

	if !failed && outputCount > 0 && maxRev <= 0 && r.finalRevReader != nil {
		var err error
		maxRev, err = r.finalRevReader()
		if err != nil {
			failed = true
			errorText = err.Error()
		}
	}
	valid := !failed && (outputCount == 0 || maxRev > 0)
	if !valid && errorText == "" {
		errorText = "durable output has no transcript revision"
	}
	return responseRunDurableHandoff{
		Valid:       valid,
		FinalRev:    maxRev,
		OutputCount: outputCount,
		Error:       errorText,
	}
}

func (r *responseRun) applyTerminalContinuationLocked(payload map[string]any) {
	response := mapValue(payload["response"])
	if id := stringValue(response["id"]); id != "" {
		r.continuationResponseID = id
	}
}

func (r *responseRun) applyDurableHandoffLocked(handoff responseRunDurableHandoff) {
	r.finalRev = handoff.FinalRev
	r.durableHandoff = handoff.Valid
	r.durableOutputCount = handoff.OutputCount
	r.durableHandoffErr = handoff.Error
}

func (r *responseRun) finalizeLifecycleLocked(outcome session.ResponseRunState) error {
	if r.finalizeLifecycle == nil {
		return nil
	}
	attention, err := r.finalizeLifecycle(outcome, r.finalRev, r.durableOutputCount)
	if err != nil {
		return fmt.Errorf("persist terminal response lifecycle: %w", err)
	}
	r.attentionSeq = attention.LatestAttentionSeq
	r.attentionStoreID = attention.StoreInstanceID
	return nil
}

// Terminal failure payloads must carry the reason at the top level. Streaming
// clients read event.error, and response recovery projects the same field into
// the transcript, so a reason recorded only under response.error surfaces as a
// bare "Response failed" notice that then vanishes on reload.
func (r *responseRun) applyTerminalErrorPayloadLocked(payload map[string]any) {
	if payload == nil {
		return
	}
	// The run state is the single source of truth for the reason, but an empty
	// one must never overwrite a caller's: a reasonless error is the very shape
	// this normalization exists to prevent.
	if r.errorMessage != "" {
		// Both locations get their own map. Sharing one would alias the event
		// body to the response body for any later mutation.
		payload["error"] = map[string]any{"type": r.errorType, "message": r.errorMessage}
	}
	response := mapValue(payload["response"])
	if len(response) == 0 {
		return
	}
	response["status"] = "failed"
	if r.errorMessage != "" {
		response["error"] = map[string]any{"type": r.errorType, "message": r.errorMessage}
	}
	delete(response, "usage")
	delete(response, "session_usage")
	delete(response, "context_usage")
}

func (r *responseRun) appendLifecycleFailureLocked(payload map[string]any, lifecycleErr error) error {
	r.status = "failed"
	r.errorType = "server_error"
	r.errorMessage = "response lifecycle could not be finalized; recovery is required"
	if payload == nil {
		payload = map[string]any{}
	}
	// A lifecycle failure that follows an earlier terminal reason is a double
	// fault, which is exactly when the first cause is worth keeping.
	if cause := strings.TrimSpace(stringValue(mapValue(payload["error"])["message"])); cause != "" {
		r.errorMessage += " (after " + cause + ")"
	}
	log.Printf("[serve] response %s lifecycle finalization failed: %v", r.id, lifecycleErr)
	payload["lifecycle_recovery_required"] = true
	if len(mapValue(payload["response"])) == 0 {
		payload["response"] = map[string]any{"id": r.id, "object": "response"}
	}
	r.applyTerminalErrorPayloadLocked(payload)
	if appendErr := r.appendEventLocked("response.failed", payload, true); appendErr != nil {
		return errors.Join(lifecycleErr, appendErr)
	}
	return lifecycleErr
}
