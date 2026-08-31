package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/samsaffron/term-llm/internal/session"
)

type serveAttentionDiagnostics struct {
	LifecycleAdmissions  atomic.Uint64
	LifecycleFinalized   atomic.Uint64
	LeaseRenewFailures   atomic.Uint64
	OrphanRecoveries     atomic.Uint64
	MarkerWrites         atomic.Uint64
	SeenAcknowledgements atomic.Uint64
}

func (d *serveAttentionDiagnostics) snapshot() map[string]uint64 {
	if d == nil {
		return map[string]uint64{}
	}
	return map[string]uint64{
		"lifecycle_admissions":  d.LifecycleAdmissions.Load(),
		"lifecycle_finalized":   d.LifecycleFinalized.Load(),
		"lease_renew_failures":  d.LeaseRenewFailures.Load(),
		"orphan_recoveries":     d.OrphanRecoveries.Load(),
		"marker_writes":         d.MarkerWrites.Load(),
		"seen_acknowledgements": d.SeenAcknowledgements.Load(),
	}
}

type markAttentionSeenRequest struct {
	StoreInstanceID string `json:"store_instance_id"`
	ThroughSeq      int64  `json:"through_seq"`
}

func (s *serveServer) handleSessionAttentionSeen(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	store, ok := session.AsAttentionStore(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusNotImplemented, "unsupported_error", "durable attention is unavailable")
		return
	}
	var request markAttentionSeenRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.StoreInstanceID) == "" || request.ThroughSeq <= 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "store_instance_id and a positive through_seq are required")
		return
	}
	state, err := store.MarkAttentionSeen(r.Context(), sessionID, request.StoreInstanceID, request.ThroughSeq)
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	case errors.Is(err, session.ErrAttentionConflict):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "attention marker changed; reconcile authoritative session state")
		return
	case err != nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to acknowledge attention")
		return
	}
	s.attentionDiagnostics.SeenAcknowledgements.Add(1)
	writeJSON(w, http.StatusOK, state)
}

func (s *serveServer) handleAttention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	store, ok := session.AsAttentionStore(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusNotImplemented, "unsupported_error", "durable attention is unavailable")
		return
	}
	kind := session.AttentionKind(strings.TrimSpace(r.URL.Query().Get("kind")))
	if kind != session.AttentionKindUnseen && kind != session.AttentionKindRunning {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "kind must be unseen or running")
		return
	}
	limit := 200
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > 500 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "limit must be between 1 and 500")
			return
		}
		limit = parsed
	}
	var snapshotVersion int64
	if value := strings.TrimSpace(r.URL.Query().Get("snapshot_version")); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "snapshot_version must be positive")
			return
		}
		snapshotVersion = parsed
	}
	page, err := store.ListAttention(r.Context(), session.AttentionListOptions{
		Kind: kind, Limit: limit, Cursor: r.URL.Query().Get("cursor"), SnapshotVersion: snapshotVersion,
	})
	if errors.Is(err, session.ErrAttentionConflict) {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "attention snapshot changed; restart pagination")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to read attention snapshot")
		return
	}
	etag := fmt.Sprintf(`"attention-%s-%d-%s-%d"`, page.StoreInstanceID, page.SnapshotVersion, kind, limit)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.URL.Query().Get("cursor") == "" && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
