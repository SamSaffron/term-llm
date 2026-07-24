package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

const (
	transcriptBodiesMaxAnchors     = session.TranscriptMaterializationMaxRanges
	transcriptBodiesMaxQueryBytes  = 4096
	transcriptStartupViewportTurns = 9
)

type transcriptRowsResponse struct {
	Seqs                     []int    `json:"seqs"`
	IDs                      []int64  `json:"ids"`
	Roles                    string   `json:"roles"`
	Flags                    []int    `json:"flags"`
	ResponseIDs              []string `json:"response_ids"`
	AssistantSegmentOrdinals []int    `json:"assistant_segment_ordinals"`
}

type transcriptResponse struct {
	Rev              int64                  `json:"rev"`
	CompactionSeq    int                    `json:"compaction_seq"`
	CompactionCount  int                    `json:"compaction_count"`
	ActiveResponseID string                 `json:"active_response_id,omitempty"`
	RunEpoch         int64                  `json:"run_epoch,omitempty"`
	StartedRev       int64                  `json:"started_rev,omitempty"`
	Rows             transcriptRowsResponse `json:"rows"`
}

type transcriptBodiesResponse struct {
	Rev      int64                 `json:"rev"`
	Messages []sessionMessageEntry `json:"messages"`
}

type transcriptStartupSideload struct {
	Index     transcriptResponse       `json:"index"`
	IndexETag string                   `json:"index_etag"`
	Bodies    transcriptBodiesResponse `json:"bodies"`
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

func (s *serveServer) activeTranscriptRun(sessionID string) (string, int64, int64) {
	if s.responseRuns == nil {
		return "", 0, 0
	}
	id := s.responseRuns.activeRunID(sessionID)
	if id == "" {
		return "", 0, 0
	}
	run, ok := s.responseRuns.get(id)
	if !ok || run == nil {
		return id, 0, 0
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	return id, run.startedRev, run.runEpoch
}

func transcriptJSON(payload any) ([]byte, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	return body, jsonPayloadETag(body), nil
}

func writeTranscriptJSON(w http.ResponseWriter, r *http.Request, rev int64, payload any) {
	body, etag, err := transcriptJSON(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
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

func transcriptRowsFromSnapshot(snapshot session.TranscriptSnapshot) transcriptRowsResponse {
	rows := transcriptRowsResponse{
		Seqs:                     make([]int, 0, len(snapshot.Items)),
		IDs:                      make([]int64, 0, len(snapshot.Items)),
		Flags:                    make([]int, 0, len(snapshot.Items)),
		ResponseIDs:              make([]string, 0, len(snapshot.Items)),
		AssistantSegmentOrdinals: make([]int, 0, len(snapshot.Items)),
	}
	var roles strings.Builder
	roles.Grow(len(snapshot.Items))
	for _, item := range snapshot.Items {
		rows.Seqs = append(rows.Seqs, item.Seq)
		rows.IDs = append(rows.IDs, item.ID)
		rows.Flags = append(rows.Flags, int(item.Flags))
		rows.ResponseIDs = append(rows.ResponseIDs, item.ResponseID)
		rows.AssistantSegmentOrdinals = append(rows.AssistantSegmentOrdinals, item.AssistantSegmentOrdinal)
		roles.WriteByte(transcriptRoleCode(item.Role))
	}
	rows.Roles = roles.String()
	return rows
}

func (s *serveServer) transcriptResponseFromSnapshot(sessionID string, snapshot session.TranscriptSnapshot) transcriptResponse {
	activeResponseID, startedRev, runEpoch := s.activeTranscriptRun(sessionID)
	return transcriptResponse{
		Rev:              snapshot.Rev,
		CompactionSeq:    snapshot.CompactionSeq,
		CompactionCount:  snapshot.CompactionCount,
		ActiveResponseID: activeResponseID,
		RunEpoch:         runEpoch,
		StartedRev:       startedRev,
		Rows:             transcriptRowsFromSnapshot(snapshot),
	}
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
	payload := s.transcriptResponseFromSnapshot(sessionID, snapshot)
	writeTranscriptJSON(w, r, snapshot.Rev, payload)
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

func transcriptStartupBodyAnchors(items []session.TranscriptIndexItem) []int64 {
	if len(items) == 0 {
		return nil
	}
	anchors := make([]int64, 0, transcriptStartupViewportTurns)
	for ordinal := len(items) - 1; ordinal >= 0 && len(anchors) < transcriptStartupViewportTurns; ordinal-- {
		if ordinal == 0 || items[ordinal].Role == "user" {
			anchors = append(anchors, items[ordinal].ID)
		}
	}
	for left, right := 0, len(anchors)-1; left < right; left, right = left+1, right-1 {
		anchors[left], anchors[right] = anchors[right], anchors[left]
	}
	return anchors
}

var errTranscriptProjectionChanged = errors.New("transcript changed while loading bodies")

func readCoherentTranscriptProjection(
	ctx context.Context,
	indexer session.TranscriptIndexer,
	sessionID string,
	selectRanges func(session.TranscriptSnapshot) []session.TranscriptRange,
) (session.TranscriptSnapshot, []session.Message, error) {
	for attempt := 0; attempt < 3; attempt++ {
		snapshot, err := indexer.GetTranscriptSnapshot(ctx, sessionID)
		if err != nil {
			return session.TranscriptSnapshot{}, nil, fmt.Errorf("get transcript snapshot: %w", err)
		}
		rev, messages, err := indexer.GetMessagesByTranscriptRanges(ctx, sessionID, selectRanges(snapshot))
		if err != nil {
			return session.TranscriptSnapshot{}, nil, fmt.Errorf("get transcript bodies: %w", err)
		}
		if rev == snapshot.Rev {
			return snapshot, messages, nil
		}
	}
	return session.TranscriptSnapshot{}, nil, errTranscriptProjectionChanged
}

func (s *serveServer) selectedTranscriptStartupSideload(ctx context.Context, sessionID string) (*transcriptStartupSideload, error) {
	indexer, ok := transcriptIndexerForWeb(s.store)
	if !ok {
		return nil, nil
	}
	snapshot, messages, err := readCoherentTranscriptProjection(ctx, indexer, sessionID, func(snapshot session.TranscriptSnapshot) []session.TranscriptRange {
		return expandTranscriptTurnRanges(snapshot.Items, transcriptStartupBodyAnchors(snapshot.Items))
	})
	if errors.Is(err, session.ErrNotFound) || errors.Is(err, errTranscriptProjectionChanged) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load startup transcript projection: %w", err)
	}
	index := s.transcriptResponseFromSnapshot(sessionID, snapshot)
	_, etag, err := transcriptJSON(index)
	if err != nil {
		return nil, fmt.Errorf("serialize startup transcript index: %w", err)
	}
	return &transcriptStartupSideload{
		Index:     index,
		IndexETag: etag,
		Bodies: transcriptBodiesResponse{
			Rev:      snapshot.Rev,
			Messages: s.sessionMessageEntries(messages),
		},
	}, nil
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

	snapshot, messages, err := readCoherentTranscriptProjection(r.Context(), indexer, sessionID, func(snapshot session.TranscriptSnapshot) []session.TranscriptRange {
		return expandTranscriptTurnRanges(snapshot.Items, requested)
	})
	if errors.Is(err, session.ErrNotFound) {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	if errors.Is(err, errTranscriptProjectionChanged) {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "transcript changed while loading bodies; refresh the index")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get transcript bodies")
		return
	}
	writeTranscriptJSON(w, r, snapshot.Rev, transcriptBodiesResponse{
		Rev:      snapshot.Rev,
		Messages: s.sessionMessageEntries(messages),
	})
}
