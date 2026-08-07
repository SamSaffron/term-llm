package cmd

import (
	"errors"
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type webBranchTreePoint struct {
	MessageID         int64  `json:"message_id"`
	AnchorMessageID   int64  `json:"anchor_message_id,omitempty"`
	Sequence          int    `json:"sequence"`
	Role              string `json:"role"`
	Preview           string `json:"preview"`
	Prefill           string `json:"prefill,omitempty"`
	LaterMessageCount int    `json:"later_message_count"`
}

type webBranchTreeResponse struct {
	session.BranchTree
	BranchPoints []webBranchTreePoint `json:"branch_points"`
}

type createSessionBranchRequest struct {
	AnchorMessageID int64  `json:"anchor_message_id"`
	ExpectedRev     *int64 `json:"expected_rev,omitempty"`
	IdempotencyKey  string `json:"idempotency_key"`
}

type createSessionBranchResponse struct {
	Session               webSessionEntry `json:"session"`
	ParentSessionID       string          `json:"parent_session_id"`
	ParentTitle           string          `json:"parent_title"`
	ForkAfterMessageID    int64           `json:"fork_after_message_id,omitempty"`
	CopiedAnchorMessageID int64           `json:"copied_anchor_message_id,omitempty"`
	Reused                bool            `json:"reused,omitempty"`
}

type prepareSessionPathNotesRequest struct {
	Mode  string `json:"mode"`
	Focus string `json:"focus,omitempty"`
}

type prepareSessionPathNotesResponse struct {
	Ready  bool `json:"ready"`
	Reused bool `json:"reused,omitempty"`
}

func branchContextSourceMessage(message session.Message) bool {
	if message.CompactionTail || llm.IsInternalCompactionSummaryText(message.TextContent) {
		return false
	}
	switch message.Role {
	case llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
		return true
	case llm.RoleDeveloper:
		_, ok := message.PathNoteProvenance()
		return ok
	default:
		return false
	}
}

func webBranchTreePoints(messages []session.Message) []webBranchTreePoint {
	after := make(map[int64]int, len(messages))
	contextCount := 0
	for i := len(messages) - 1; i >= 0; i-- {
		after[messages[i].ID] = contextCount
		if branchContextSourceMessage(messages[i]) {
			contextCount++
		}
	}
	points := make([]webBranchTreePoint, 0, len(messages))
	previousContinuationID := int64(0)
	for _, message := range messages {
		if message.CompactionTail || llm.IsInternalCompactionSummaryText(message.TextContent) {
			continue
		}
		if message.Role == llm.RoleAssistant {
			previousContinuationID = message.ID
			continue
		}
		if message.Role != llm.RoleUser {
			continue
		}
		anchorID := previousContinuationID
		laterCount := contextCount
		if anchorID > 0 {
			laterCount = after[anchorID]
		}
		if laterCount > 0 {
			laterCount--
		}
		preview := session.TruncateSummary(strings.Join(strings.Fields(message.TextContent), " "))
		if preview == "" {
			preview = "(attachment content)"
		}
		points = append(points, webBranchTreePoint{
			MessageID: message.ID, AnchorMessageID: anchorID, Sequence: message.Sequence,
			Role: string(llm.RoleUser), Preview: preview, Prefill: message.TextContent, LaterMessageCount: laterCount,
		})
	}
	return points
}

func (s *serveServer) handleSessionTree(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s == nil || s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	store, ok := s.store.(session.ConversationBranchStore)
	if !ok {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
		return
	}
	tree, err := store.GetBranchTree(r.Context(), sessionID)
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
	case errors.Is(err, session.ErrBranchingUnsupported):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
	case err != nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load conversation tree")
	default:
		response := webBranchTreeResponse{BranchTree: tree}
		if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_branch_points")), "1") ||
			strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_branch_points")), "true") {
			messages, messageErr := s.store.GetMessages(r.Context(), sessionID, 0, 0)
			if messageErr != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load conversation branch points")
				return
			}
			response.BranchPoints = webBranchTreePoints(messages)
		}
		writeJSON(w, http.StatusOK, response)
	}
}

func (s *serveServer) handleCreateSessionBranch(w http.ResponseWriter, r *http.Request, sourceSessionID string) {
	if s == nil || s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	branchStore, ok := s.store.(session.ConversationBranchStore)
	if !ok {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
		return
	}
	var req createSessionBranchRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.IdempotencyKey == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "idempotency_key is required")
		return
	}
	source, err := s.store.Get(r.Context(), sourceSessionID)
	if err != nil || source == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session not found")
		return
	}
	if replayStore, ok := s.store.(session.ConversationBranchReplayStore); ok {
		replay, found, replayErr := replayStore.GetBranchByIdempotencyKey(r.Context(), sourceSessionID, req.IdempotencyKey)
		switch {
		case errors.Is(replayErr, session.ErrBranchingUnsupported):
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
			return
		case replayErr != nil:
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to resolve conversation branch")
			return
		case found && replay.Session != nil:
			if replay.ForkAfterMessageID != req.AnchorMessageID {
				writeOpenAIError(w, http.StatusConflict, "conflict_error", "idempotency key was already used for a different branch point")
				return
			}
			writeJSON(w, http.StatusOK, createSessionBranchResponse{
				Session:               s.webSessionEntryFromSession(replay.Session),
				ParentSessionID:       sourceSessionID,
				ParentTitle:           source.PreferredShortTitle(),
				ForkAfterMessageID:    replay.ForkAfterMessageID,
				CopiedAnchorMessageID: replay.AnchorMessageID,
				Reused:                true,
			})
			return
		}
	}
	var expectedState *session.TranscriptMutationState
	if req.ExpectedRev == nil {
		mutationStore, ok := s.store.(session.TranscriptUndoRedoStore)
		if !ok {
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "revision-safe conversation branching is unavailable")
			return
		}
		state, stateErr := mutationStore.TranscriptMutationState(r.Context(), sourceSessionID)
		if stateErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to inspect conversation revision")
			return
		}
		expectedState = &state
	}
	unlock, busy := s.lockBranchSourceRuntime(sourceSessionID)
	if busy {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot branch while source work is active")
		return
	}
	defer unlock()
	result, err := branchStore.CreateBranch(r.Context(), sourceSessionID, session.CreateBranchOptions{
		AnchorMessageID: req.AnchorMessageID,
		ExpectedState:   expectedState,
		ExpectedRev:     req.ExpectedRev,
		IdempotencyKey:  req.IdempotencyKey,
	})
	switch {
	case errors.Is(err, session.ErrBranchConflict):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation changed in another client; refresh and try again")
	case errors.Is(err, session.ErrBranchIdempotencyConflict):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "idempotency key was already used for a different branch point")
	case errors.Is(err, session.ErrBranchingUnsupported):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
	case errors.Is(err, session.ErrNotFound):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "branch source or anchor was not found")
	case err != nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to create conversation branch")
	case result.Session == nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to create conversation branch")
	default:
		writeJSON(w, http.StatusCreated, createSessionBranchResponse{
			Session:               s.webSessionEntryFromSession(result.Session),
			ParentSessionID:       sourceSessionID,
			ParentTitle:           source.PreferredShortTitle(),
			ForkAfterMessageID:    result.ForkAfterMessageID,
			CopiedAnchorMessageID: result.AnchorMessageID,
			Reused:                result.Reused,
		})
	}
}

func directBranchNode(tree session.BranchTree, childSessionID string) (session.BranchTreeNode, bool) {
	for _, node := range tree.Nodes {
		if node.SessionID == childSessionID && node.ParentSessionID != "" {
			return node, true
		}
	}
	return session.BranchTreeNode{}, false
}

func existingPathNote(messages []session.Message, sourceSessionID string, anchorMessageID int64) bool {
	for i := range messages {
		provenance, ok := messages[i].PathNoteProvenance()
		if ok && provenance.SourceSessionID == sourceSessionID && provenance.AnchorMessageID == anchorMessageID {
			return true
		}
	}
	return false
}

func (s *serveServer) handleSessionPathNotes(w http.ResponseWriter, r *http.Request, childSessionID string) {
	if s == nil || s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}
	branchStore, ok := s.store.(session.ConversationBranchStore)
	if !ok {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
		return
	}
	var req prepareSessionPathNotesRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != "notes" && mode != "focused" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "mode must be notes or focused")
		return
	}
	if _, loaded := s.branchNotes.LoadOrStore(childSessionID, struct{}{}); loaded {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "branch context generation is already active")
		return
	}
	defer s.branchNotes.Delete(childSessionID)

	tree, err := branchStore.GetBranchTree(r.Context(), childSessionID)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "conversation branch was not found")
		return
	}
	edge, ok := directBranchNode(tree, childSessionID)
	if !ok {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "session is not a conversation branch")
		return
	}
	messages, err := s.store.GetMessages(r.Context(), childSessionID, 0, 0)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to inspect branch context")
		return
	}
	if existingPathNote(messages, edge.ParentSessionID, edge.ForkAfterMessageID) {
		writeJSON(w, http.StatusOK, prepareSessionPathNotesResponse{Ready: true, Reused: true})
		return
	}
	unlockSource, busy := s.lockBranchSourceRuntime(edge.ParentSessionID)
	if busy {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot prepare branch context while source work is active")
		return
	}
	defer unlockSource()
	workCtx, cancelWork := detachedBranchWorkContext(r.Context())
	defer cancelWork()
	pathNote, status, message := s.prepareBranchPathNote(workCtx, edge.ParentSessionID, edge.ForkAfterMessageID, &responsesBranchContextRequest{Mode: mode, Focus: req.Focus})
	if status != 0 {
		errType := "invalid_request_error"
		if status == http.StatusConflict {
			errType = "conflict_error"
		} else if status >= http.StatusInternalServerError {
			errType = "server_error"
		}
		writeOpenAIError(w, status, errType, message)
		return
	}
	if pathNote != nil {
		pathNote.Provenance.SourceSessionID = edge.ParentSessionID
		pathNote.Provenance.AnchorMessageID = edge.ForkAfterMessageID
		note := session.NewPathNoteMessage(childSessionID, pathNote.Text, pathNote.Provenance, -1)
		if err := s.store.AddMessage(workCtx, childSessionID, note); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to save branch context")
			return
		}
	}
	writeJSON(w, http.StatusOK, prepareSessionPathNotesResponse{Ready: true})
}
