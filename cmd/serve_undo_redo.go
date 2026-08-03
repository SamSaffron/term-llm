package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type sessionUndoRedoRequest struct {
	ExpectedRev    int64 `json:"expected_rev"`
	ExpectedHeadID int64 `json:"expected_head_id"`
}

func (rt *serveRuntime) resetAfterTranscriptMutation(ctx context.Context, sessionID string) error {
	if rt == nil {
		return nil
	}
	// Reset continuation and make the in-memory transcript non-authoritative
	// before any reload that can fail. A later request will hydrate from storage
	// when sessionMeta remains nil.
	rt.history = nil
	rt.historyPersisted = false
	rt.sessionMeta = nil
	rt.lastInjectedPlatform = ""
	rt.clearLastUIRunError()
	if rt.engine != nil {
		rt.engine.ResetSessionState(sessionID)
		rt.engine.SetContextEstimateBaseline(0, 0)
	}
	rt.responseMu.Lock()
	rt.lastResponseID = ""
	rt.responseIDs = nil
	rt.responseMu.Unlock()
	rt.refreshSideQuestionSnapshot(nil)

	sess, err := rt.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("reload session metadata: %w", err)
	}
	messages, err := session.LoadActiveMessages(ctx, rt.store, sess)
	if err != nil {
		return fmt.Errorf("reload active transcript: %w", err)
	}
	history := make([]llm.Message, 0, len(messages))
	for i := range messages {
		history = append(history, messages[i].ToLLMMessage())
	}
	rt.history = history
	rt.historyPersisted = true
	rt.sessionMeta = sess
	rt.restorePlatformInjectionStateFromHistory()
	rt.refreshSideQuestionSnapshot(history)
	return nil
}

func (s *serveServer) handleSessionUndoRedo(w http.ResponseWriter, r *http.Request, sessionID string, redo bool) {
	operation := "undo"
	if redo {
		operation = "redo"
	}
	var req sessionUndoRedoRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	mutationStore, ok := s.store.(session.TranscriptUndoRedoStore)
	if !ok {
		writeOpenAIError(w, http.StatusNotImplemented, "server_error", "session storage does not support undo/redo")
		return
	}

	if s.responseRuns != nil && s.responseRuns.activeRunID(sessionID) != "" {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot mutate the transcript while work is active")
		return
	}

	var rt *serveRuntime
	if s.sessionMgr != nil {
		rt, _ = s.sessionMgr.Get(sessionID)
		if rt != nil {
			if !rt.mu.TryLock() {
				writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot mutate the transcript while work is active")
				return
			}
			defer rt.mu.Unlock()
			if rt.hasActiveActivity() || (s.responseRuns != nil && s.responseRuns.activeRunID(sessionID) != "") {
				writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot mutate the transcript while work is active")
				return
			}
		}
	}

	expected := session.TranscriptMutationState{Rev: req.ExpectedRev, HeadID: req.ExpectedHeadID}
	var result session.TranscriptMutationResult
	var err error
	if redo {
		result, err = mutationStore.RedoLastUserTurn(r.Context(), sessionID, expected)
	} else {
		result, err = mutationStore.UndoLastUserTurn(r.Context(), sessionID, expected)
	}
	if err != nil {
		switch {
		case errors.Is(err, session.ErrNotFound):
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		case errors.Is(err, session.ErrNothingToUndo):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "nothing to undo")
		case errors.Is(err, session.ErrNothingToRedo):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "nothing to redo")
		case errors.Is(err, session.ErrTranscriptConflict):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "transcript changed; refresh and try again")
		default:
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to "+operation+" transcript")
		}
		return
	}

	if rt != nil {
		oldResponseIDs := rt.getResponseIDs()
		if rt.store == nil {
			rt.store = s.store
		}
		if err := rt.resetAfterTranscriptMutation(r.Context(), sessionID); err != nil {
			log.Printf("[serve] transcript mutation runtime reload failed for %s: %v", sessionID, err)
		}
		for _, responseID := range oldResponseIDs {
			s.responseToSession.Delete(responseID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"operation":           operation,
		"rev":                 result.Rev,
		"head_id":             result.HeadID,
		"user_text":           result.UserText,
		"attachments_omitted": result.AttachmentsOmitted,
	})
}
