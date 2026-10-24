package serve

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/restart"
)

func (s *serveServer) handleSessionRuntimeCompact(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.store == nil || s.sessionMgr == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session runtime is unavailable")
		return
	}
	sess, err := s.store.Get(r.Context(), sessionID)
	if err != nil || sess == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	providerKey := strings.TrimSpace(sess.ProviderKey)
	if providerKey == "" {
		providerKey = appruntime.ResolveSessionProviderKey(s.cfgRef, sess)
	}
	rt, _, err := s.runtimeForProviderModelRequest(r.Context(), sessionID, providerKey, strings.TrimSpace(sess.Model))
	if err != nil {
		writeCompactionError(w, err)
		return
	}
	if s.foreignTurnActive(r.Context(), sessionID) {
		writeCompactionError(w, errServeSessionBusy)
		return
	}
	// Acceptance, not the HTTP connection, owns the operation. Root seals new
	// user admission during reload; Go retains its descendant until completion.
	workCtx, releaseRoot, err := restart.Default.Root(context.Background())
	if err != nil {
		writeCompactionError(w, err)
		return
	}
	defer releaseRoot()
	prepared, releaseAdmission, err := s.prepareSessionCompaction(r.Context(), sessionID, rt)
	if err != nil {
		writeCompactionError(w, err)
		return
	}
	workCtx, finishTask := restart.Default.Cancellable(workCtx)
	s.bindRuntimeActivity(rt, sessionID)
	rt.beginCompactionActivity()
	accepted := rt.activitySnapshot()
	finish := func(err error) {
		defer finishTask()
		rt.finishCompactionActivityWithRelease(err, func() {
			prepared.release()
			releaseAdmission()
		})
	}
	if err := restart.Default.Go(workCtx, func(ctx context.Context) {
		var workErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("manual session compaction panicked", "session_id", sessionID, "panic", recovered, "stack", string(debug.Stack()))
				workErr = errors.New("conversation compaction failed unexpectedly")
			}
			finish(workErr)
		}()
		result, err := prepared.execute(ctx)
		workErr = err
		if err == nil && result != nil { // native compaction leaves the transcript unchanged
			s.publishEvent(serveEventInput{Type: serveEventSessionTranscriptChanged, SessionID: sessionID, Reason: "compaction"})
		}
	}); err != nil {
		finish(err)
		writeCompactionError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"activity": accepted})
}

// The same pin used by response setup closes both directions of the admission
// gap: a registered response rejects compaction before it has taken rt.mu, and
// compacting rejects new response admission before it can persist user input.
func (s *serveServer) prepareSessionCompaction(ctx context.Context, sessionID string, rt *serveRuntime) (*preparedSessionCompaction, func(), error) {
	releasePin, err := s.sessionMgr.pinCurrentRuntime(sessionID, rt)
	if err != nil {
		return nil, nil, err
	}
	defer releasePin()
	if s.responseRuns != nil && (s.responseRuns.activeRunID(sessionID) != "" || s.responseRuns.steeringTransition(sessionID) != nil) {
		return nil, nil, errServeSessionBusy
	}
	prepared, err := rt.prepareCompaction(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	rt.admittedActivity.Add(1)
	return prepared, func() { rt.admittedActivity.Add(-1) }, nil
}

func writeCompactionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errServeSessionBusy):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "session is busy; wait for the active request to finish")
	case errors.Is(err, errServeCompactionTooSmall):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
	case errors.Is(err, errServeCompactionUnavailable):
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", err.Error())
	default:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
	}
}
