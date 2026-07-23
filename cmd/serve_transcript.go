package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

const transcriptBodiesMaxIDs = 512

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

func parseTranscriptBodyIDs(raw string) ([]int64, error) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	seen := make(map[int64]struct{})
	ids := make([]int64, 0)
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		startText, endText := part, part
		if dash := strings.IndexByte(part, '-'); dash >= 0 {
			startText = strings.TrimSpace(part[:dash])
			endText = strings.TrimSpace(part[dash+1:])
		}
		start, err := strconv.ParseInt(startText, 10, 64)
		if err != nil || start <= 0 {
			return nil, fmt.Errorf("invalid transcript body id %q", part)
		}
		end, err := strconv.ParseInt(endText, 10, 64)
		if err != nil || end < start {
			return nil, fmt.Errorf("invalid transcript body id range %q", part)
		}
		if end-start+1 > transcriptBodiesMaxIDs {
			return nil, fmt.Errorf("transcript bodies request exceeds %d IDs", transcriptBodiesMaxIDs)
		}
		for id := start; ; id++ {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
				if len(ids) > transcriptBodiesMaxIDs {
					return nil, fmt.Errorf("transcript bodies request exceeds %d IDs", transcriptBodiesMaxIDs)
				}
			}
			if id == end {
				break
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("ids is required")
	}
	return ids, nil
}

func expandTranscriptTurnIDs(items []session.TranscriptIndexItem, requested []int64) []int64 {
	requestedSet := make(map[int64]struct{}, len(requested))
	for _, id := range requested {
		requestedSet[id] = struct{}{}
	}
	selected := make(map[int64]struct{})
	for ordinal, item := range items {
		if _, ok := requestedSet[item.ID]; !ok {
			continue
		}
		start := ordinal
		for start > 0 && items[start].Role != "user" {
			start--
		}
		if items[start].Role != "user" {
			start = 0
		}
		end := ordinal + 1
		for end < len(items) && items[end].Role != "user" {
			end++
		}
		for i := start; i < end; i++ {
			selected[items[i].ID] = struct{}{}
		}
	}
	ids := make([]int64, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *serveServer) handleSessionTranscriptBodies(w http.ResponseWriter, r *http.Request, sessionID string) {
	indexer, ok := transcriptIndexerForWeb(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "revisioned transcript is unavailable")
		return
	}
	requested, err := parseTranscriptBodyIDs(r.URL.Query().Get("ids"))
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
		expanded := expandTranscriptTurnIDs(snapshot.Items, requested)
		if len(expanded) > transcriptBodiesMaxIDs {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("transcript turn exceeds %d display rows", transcriptBodiesMaxIDs))
			return
		}
		rev, messages, err := indexer.GetMessagesByIDs(r.Context(), sessionID, expanded)
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
