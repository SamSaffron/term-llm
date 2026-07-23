package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

const (
	transcriptBodiesMaxAnchors    = session.TranscriptMaterializationMaxRanges
	transcriptBodiesMaxQueryBytes = 4096
)

type transcriptRowsResponse struct {
	Seqs  []int   `json:"seqs"`
	IDs   []int64 `json:"ids"`
	Roles string  `json:"roles"`
	Flags []uint8 `json:"flags"`
}

type transcriptResponse struct {
	Rev              int64                  `json:"rev"`
	CompactionSeq    int                    `json:"compaction_seq"`
	CompactionCount  int                    `json:"compaction_count"`
	ActiveResponseID string                 `json:"active_response_id,omitempty"`
	StartedRev       int64                  `json:"started_rev,omitempty"`
	Rows             transcriptRowsResponse `json:"rows"`
}

type transcriptBodiesResponse struct {
	Rev      int64                 `json:"rev"`
	Messages []sessionMessageEntry `json:"messages"`
}

func transcriptRoleCode(role string) byte {
	switch role {
	case "user":
		return 'u'
	case "assistant":
		return 'a'
	case "tool":
		return 't'
	case "event":
		return 'e'
	default:
		return '?'
	}
}

func transcriptIndexerForWeb(store session.Store) (session.TranscriptIndexer, bool) {
	if store == nil {
		return nil, false
	}
	if reporter, ok := store.(session.TranscriptVersionReporter); ok && !reporter.TranscriptVersioned() {
		return nil, false
	}
	if loggingStore, ok := store.(*session.LoggingStore); ok {
		if _, supported := transcriptIndexerForWeb(loggingStore.Store); !supported {
			return nil, false
		}
	}
	indexer, ok := store.(session.TranscriptIndexer)
	return indexer, ok
}

func (s *serveServer) activeTranscriptRun(sessionID string) (string, int64) {
	if s.responseRuns == nil {
		return "", 0
	}
	id := s.responseRuns.activeRunID(sessionID)
	if id == "" {
		return "", 0
	}
	run, ok := s.responseRuns.get(id)
	if !ok || run == nil {
		return id, 0
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return id, run.startedRev
}

func writeTranscriptJSON(w http.ResponseWriter, r *http.Request, rev int64, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	etag := jsonPayloadETag(body)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Transcript-Rev", strconv.FormatInt(rev, 10))
	if uiETagMatches(r.Header.Get("If-None-Match"), etag) {
		w.Header().Set("Content-Type", "application/json")
		uiAddVary(w.Header(), "Accept-Encoding")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSONGzipBody(w, r, http.StatusOK, body)
}

func (s *serveServer) handleSessionTranscript(w http.ResponseWriter, r *http.Request, sessionID string) {
	indexer, ok := transcriptIndexerForWeb(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "revisioned transcript is unavailable")
		return
	}
	snapshot, err := indexer.GetTranscriptSnapshot(r.Context(), sessionID)
	if errors.Is(err, session.ErrNotFound) {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get transcript")
		return
	}
	rows := transcriptRowsResponse{
		Seqs:  make([]int, 0, len(snapshot.Items)),
		IDs:   make([]int64, 0, len(snapshot.Items)),
		Flags: make([]uint8, 0, len(snapshot.Items)),
	}
	var roles strings.Builder
	roles.Grow(len(snapshot.Items))
	for _, item := range snapshot.Items {
		rows.Seqs = append(rows.Seqs, item.Seq)
		rows.IDs = append(rows.IDs, item.ID)
		rows.Flags = append(rows.Flags, item.Flags)
		roles.WriteByte(transcriptRoleCode(item.Role))
	}
	rows.Roles = roles.String()
	activeResponseID, startedRev := s.activeTranscriptRun(sessionID)
	writeTranscriptJSON(w, r, snapshot.Rev, transcriptResponse{
		Rev:              snapshot.Rev,
		CompactionSeq:    snapshot.CompactionSeq,
		CompactionCount:  snapshot.CompactionCount,
		ActiveResponseID: activeResponseID,
		StartedRev:       startedRev,
		Rows:             rows,
	})
}

func parseTranscriptBodyAnchors(raw string) ([]int64, error) {
	if len(raw) > transcriptBodiesMaxQueryBytes {
		return nil, fmt.Errorf("transcript bodies query is too long")
	}
	parts := strings.Split(strings.TrimSpace(raw), ",")
	seen := make(map[int64]struct{}, len(parts))
	anchors := make([]int64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid transcript body anchor %q", part)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		anchors = append(anchors, id)
		if len(anchors) > transcriptBodiesMaxAnchors {
			return nil, fmt.Errorf("transcript bodies request exceeds %d turn anchors", transcriptBodiesMaxAnchors)
		}
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("ids is required")
	}
	return anchors, nil
}

func expandTranscriptTurnRanges(items []session.TranscriptIndexItem, requested []int64) []session.TranscriptRange {
	requestedSet := make(map[int64]struct{}, len(requested))
	for _, id := range requested {
		requestedSet[id] = struct{}{}
	}
	ranges := make([]session.TranscriptRange, 0, len(requested))
	for start := 0; start < len(items); {
		end := start + 1
		for end < len(items) && items[end].Role != "user" {
			end++
		}
		selected := false
		for ordinal := start; ordinal < end; ordinal++ {
			if _, ok := requestedSet[items[ordinal].ID]; ok {
				selected = true
				break
			}
		}
		if selected {
			last := end - 1
			ranges = append(ranges, session.TranscriptRange{
				StartSeq: items[start].Seq,
				StartID:  items[start].ID,
				EndSeq:   items[last].Seq,
				EndID:    items[last].ID,
			})
		}
		start = end
	}
	return ranges
}

func (s *serveServer) handleSessionTranscriptBodies(w http.ResponseWriter, r *http.Request, sessionID string) {
	indexer, ok := transcriptIndexerForWeb(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "revisioned transcript is unavailable")
		return
	}
	requested, err := parseTranscriptBodyAnchors(r.URL.Query().Get("ids"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	for attempt := 0; attempt < 3; attempt++ {
		snapshot, err := indexer.GetTranscriptSnapshot(r.Context(), sessionID)
		if errors.Is(err, session.ErrNotFound) {
			writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
			return
		}
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get transcript")
			return
		}
		ranges := expandTranscriptTurnRanges(snapshot.Items, requested)
		rev, messages, err := indexer.GetMessagesByTranscriptRanges(r.Context(), sessionID, ranges)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get transcript bodies")
			return
		}
		if rev != snapshot.Rev {
			continue
		}
		writeTranscriptJSON(w, r, rev, transcriptBodiesResponse{
			Rev:      rev,
			Messages: s.sessionMessageEntries(messages),
		})
		return
	}
	writeOpenAIError(w, http.StatusConflict, "conflict_error", "transcript changed while loading bodies; refresh the index")
}
