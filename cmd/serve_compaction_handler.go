package cmd

import (
	"errors"
	"net/http"
	"strings"
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
		providerKey = resolveSessionProviderKey(s.cfgRef, sess)
	}
	rt, _, err := s.runtimeForProviderModelRequest(r.Context(), sessionID, providerKey, strings.TrimSpace(sess.Model))
	if err != nil {
		status := http.StatusInternalServerError
		errorType := "server_error"
		if errors.Is(err, errServeSessionBusy) {
			status = http.StatusConflict
			errorType = "conflict_error"
		}
		writeOpenAIError(w, status, errorType, err.Error())
		return
	}
	result, err := rt.compactSession(r.Context(), sessionID)
	if err != nil {
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
		return
	}
	s.publishEvent(serveEventInput{Type: serveEventSessionTranscriptChanged, SessionID: sessionID, Reason: "compaction"})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"original_messages":  result.OriginalCount,
		"compacted_messages": result.CompactedCount,
		"model":              result.Model,
		"usage":              result.Usage,
	})
}
