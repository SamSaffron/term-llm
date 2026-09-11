package cmd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func (r *responseRun) appendEvent(event string, payload map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.appendEventLocked(event, payload, false)
}

func responseRunOwnedOutputKeys(runID string, messages []llm.Message) []string {
	keys := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg.ResponseID != runID {
			continue
		}
		switch msg.Role {
		case llm.RoleAssistant:
			keys = append(keys, fmt.Sprintf("assistant:%d", msg.AssistantSegmentOrdinal))
		case llm.RoleTool:
			callID := ""
			for _, part := range msg.Parts {
				if part.ToolResult != nil && part.ToolResult.ID != "" {
					callID = part.ToolResult.ID
					break
				}
				if part.ToolCall != nil && part.ToolCall.ID != "" {
					callID = part.ToolCall.ID
					break
				}
			}
			if callID != "" {
				keys = append(keys, "tool:"+callID)
			} else {
				keys = append(keys, "")
			}
		case llm.RoleEvent:
			if msg.SegmentStartSequence > 0 || msg.SegmentEndSequence > 0 {
				keys = append(keys, fmt.Sprintf("event:%d:%d", msg.SegmentStartSequence, msg.SegmentEndSequence))
			} else {
				keys = append(keys, "")
			}
		}
	}
	return keys
}

func beginResponseRunPersistence(ctx context.Context, messages []llm.Message) (func(int64, error), int) {
	run, _ := ctx.Value(responseRunContextKey{}).(*responseRun)
	if run == nil || run.persistence == nil {
		return func(int64, error) {}, 0
	}
	keys := responseRunOwnedOutputKeys(run.id, messages)
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

func runResponseRunPersistence(ctx context.Context, messages []llm.Message, persist func(session.ResponseRunFence) (int64, error)) (rev int64, err error) {
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

func addResponseRunMessage(ctx context.Context, store session.Store, sessionID string, message *session.Message) (int64, error) {
	writer, ok := store.(session.TranscriptRevisionWriter)
	if !ok {
		if err := store.AddMessage(ctx, sessionID, message); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.AddMessageWithTranscriptRev(ctx, sessionID, message)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		if err := store.AddMessage(ctx, sessionID, message); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
}

func replaceResponseRunMessages(ctx context.Context, store session.Store, sessionID string, messages []session.Message) (int64, error) {
	writer, ok := store.(session.TranscriptRevisionWriter)
	if !ok {
		if err := store.ReplaceMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.ReplaceMessagesWithTranscriptRev(ctx, sessionID, messages)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		if err := store.ReplaceMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
}

func replaceCompactedResponseRunMessages(ctx context.Context, store session.Store, sessionID string, messages []session.Message) (int64, error) {
	writer, ok := store.(session.CompactedTranscriptRevisionWriter)
	if !ok {
		replacer, supported := store.(interface {
			ReplaceCompactedMessages(context.Context, string, []session.Message) error
		})
		if !supported {
			return 0, errors.New("session store cannot replace compacted transcript")
		}
		if err := replacer.ReplaceCompactedMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.ReplaceCompactedMessagesWithTranscriptRev(ctx, sessionID, messages)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		replacer, supported := store.(interface {
			ReplaceCompactedMessages(context.Context, string, []session.Message) error
		})
		if !supported {
			return 0, errors.New("session store cannot replace compacted transcript")
		}
		if err := replacer.ReplaceCompactedMessages(ctx, sessionID, messages); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
}

func updateResponseRunStreamingMessage(ctx context.Context, store session.Store, sessionID string, message *session.Message, finalizeText bool) (int64, error) {
	writer, ok := store.(session.TranscriptRevisionWriter)
	if !ok {
		if err := session.UpdateStreamingMessage(ctx, store, sessionID, message, finalizeText); err != nil {
			return 0, err
		}
		return 0, nil
	}
	rev, err := writer.UpdateStreamingMessageWithTranscriptRev(ctx, sessionID, message, finalizeText)
	if errors.Is(err, session.ErrTranscriptRevisionUnsupported) {
		if err := session.UpdateStreamingMessage(ctx, store, sessionID, message, finalizeText); err != nil {
			return 0, err
		}
		return 0, nil
	}
	return rev, err
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

func (r *responseRun) appendLifecycleFailureLocked(payload map[string]any, lifecycleErr error) error {
	r.status = "failed"
	r.errorType = "server_error"
	r.errorMessage = "response lifecycle could not be finalized; recovery is required"
	if payload == nil {
		payload = map[string]any{}
	}
	payload["lifecycle_recovery_required"] = true
	response := mapValue(payload["response"])
	if len(response) == 0 {
		response = map[string]any{"id": r.id, "object": "response"}
		payload["response"] = response
	}
	response["status"] = "failed"
	response["error"] = map[string]any{"type": r.errorType, "message": r.errorMessage}
	delete(response, "usage")
	delete(response, "session_usage")
	delete(response, "context_usage")
	if appendErr := r.appendEventLocked("response.failed", payload, true); appendErr != nil {
		return errors.Join(lifecycleErr, appendErr)
	}
	return lifecycleErr
}
