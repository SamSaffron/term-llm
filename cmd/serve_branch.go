package cmd

import (
	"context"
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
	AnchorMessageID int64 `json:"anchor_message_id"`
	// LegacyExpectedRev is accepted and ignored for backward wire compatibility.
	// Deprecated: this Web endpoint identifies an immutable transcript prefix by
	// anchor_message_id; lower-level CreateBranch and /v1/responses retain their
	// distinct full-head compare-and-swap contracts.
	LegacyExpectedRev *int64 `json:"expected_rev,omitempty"`
	IdempotencyKey    string `json:"idempotency_key"`
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
	Ready   bool   `json:"ready"`
	Reused  bool   `json:"reused,omitempty"`
	Limited bool   `json:"limited,omitempty"`
	Message string `json:"message,omitempty"`
}

func branchContextSourceMessage(message session.Message) bool {
	if session.IsBranchableMessage(message) {
		return true
	}
	if message.Role == llm.RoleDeveloper {
		_, ok := message.PathNoteProvenance()
		return ok
	}
	return false
}

func activeWebBranchAnchorRowID(messages []session.Message, activeResponseID string, sampledAnchorRowID int64) int64 {
	if strings.TrimSpace(activeResponseID) == "" {
		return 0
	}
	if sampledAnchorRowID <= 0 {
		return -1
	}
	for _, message := range messages {
		if message.ID == sampledAnchorRowID && session.IsBranchableMessage(message) {
			return sampledAnchorRowID
		}
	}
	return -1
}

func pruneActiveWebBranchOutput(messages []session.Message, activeResponseID string, activeAnchorRowID int64) []session.Message {
	if strings.TrimSpace(activeResponseID) == "" {
		return messages
	}
	if activeAnchorRowID <= 0 {
		return nil
	}
	for i := range messages {
		if messages[i].ID == activeAnchorRowID {
			return append([]session.Message(nil), messages[:i+1]...)
		}
	}
	return nil
}

type activeWebBranchAnchorStatus uint8

const (
	activeWebBranchAnchorSafe activeWebBranchAnchorStatus = iota
	activeWebBranchAnchorMissing
	activeWebBranchAnchorInvalid
	activeWebBranchAnchorUnstable
)

func activeWebBranchAnchorSafety(messages []session.Message, activeResponseID string, sampledAnchorRowID, requestedAnchorRowID int64) activeWebBranchAnchorStatus {
	if requestedAnchorRowID == 0 {
		return activeWebBranchAnchorSafe
	}
	var requested *session.Message
	for i := range messages {
		if messages[i].ID == requestedAnchorRowID {
			requested = &messages[i]
			break
		}
	}
	if requested == nil {
		return activeWebBranchAnchorMissing
	}
	if !session.IsBranchableMessage(*requested) {
		return activeWebBranchAnchorInvalid
	}
	activeAnchorRowID := activeWebBranchAnchorRowID(messages, activeResponseID, sampledAnchorRowID)
	if activeAnchorRowID <= 0 {
		return activeWebBranchAnchorUnstable
	}
	var activeAnchor *session.Message
	for i := range messages {
		if messages[i].ID == activeAnchorRowID {
			activeAnchor = &messages[i]
			break
		}
	}
	if activeAnchor == nil || requested.Sequence > activeAnchor.Sequence {
		return activeWebBranchAnchorUnstable
	}
	return activeWebBranchAnchorSafe
}

const webBranchSnapshotMaxAttempts = 3

var errWebBranchSnapshotUnstable = errors.New("conversation branch snapshot changed while loading")

type webBranchContextSnapshot struct {
	messages          []session.Message
	activeAnchorRowID int64
	limited           bool
}

func (s *serveServer) loadWebBranchContextSnapshot(ctx context.Context, sessionID string) (webBranchContextSnapshot, error) {
	for attempt := 0; attempt < webBranchSnapshotMaxAttempts; attempt++ {
		activeResponseID, _, activeEpoch, _, sampledAnchorRowID := s.activeTranscriptRun(sessionID)
		messages, err := s.store.GetMessages(ctx, sessionID, 0, 0)
		if err != nil {
			return webBranchContextSnapshot{}, err
		}
		checkResponseID, _, checkEpoch, _, checkAnchorRowID := s.activeTranscriptRun(sessionID)
		if checkResponseID != activeResponseID || checkEpoch != activeEpoch || checkAnchorRowID != sampledAnchorRowID {
			if err := ctx.Err(); err != nil {
				return webBranchContextSnapshot{}, err
			}
			continue
		}
		if activeResponseID == "" {
			return webBranchContextSnapshot{messages: messages}, nil
		}
		activeAnchorRowID := activeWebBranchAnchorRowID(messages, activeResponseID, sampledAnchorRowID)
		safeMessages := pruneActiveWebBranchOutput(messages, activeResponseID, activeAnchorRowID)
		return webBranchContextSnapshot{
			messages:          safeMessages,
			activeAnchorRowID: activeAnchorRowID,
			limited:           len(safeMessages) < len(messages),
		}, nil
	}
	return webBranchContextSnapshot{}, errWebBranchSnapshotUnstable
}

func webBranchTreePointsForActiveRun(messages []session.Message, activeAnchorRowID int64) []webBranchTreePoint {
	if activeAnchorRowID < 0 {
		return nil
	}
	if activeAnchorRowID > 0 {
		activeAnchorIndex := -1
		for i := range messages {
			if messages[i].ID == activeAnchorRowID {
				activeAnchorIndex = i
				break
			}
		}
		if activeAnchorIndex < 0 {
			return nil
		}
		messages = messages[:activeAnchorIndex+1]
	}
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

func webBranchTreePoints(messages []session.Message) []webBranchTreePoint {
	return webBranchTreePointsForActiveRun(messages, 0)
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
			snapshot, snapshotErr := s.loadWebBranchContextSnapshot(r.Context(), sessionID)
			switch {
			case errors.Is(snapshotErr, errWebBranchSnapshotUnstable):
				writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branch points are changing; refresh and try again")
				return
			case snapshotErr != nil:
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load conversation branch points")
				return
			}
			response.BranchPoints = webBranchTreePointsForActiveRun(snapshot.messages, snapshot.activeAnchorRowID)
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
	activeResponseID, _, _, _, activeAnchorRowID := s.activeTranscriptRun(sourceSessionID)
	activeSource := activeResponseID != ""
	unlock := func() {}
	validateActiveAnchor := func() bool {
		messages, loadErr := s.store.GetMessages(r.Context(), sourceSessionID, 0, 0)
		if loadErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to validate active conversation branch")
			return false
		}
		status := activeWebBranchAnchorSafety(messages, activeResponseID, activeAnchorRowID, req.AnchorMessageID)
		switch status {
		case activeWebBranchAnchorMissing:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "branch source or anchor was not found")
			return false
		case activeWebBranchAnchorInvalid:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "branch anchor is not a durable continuation boundary")
			return false
		case activeWebBranchAnchorUnstable:
			writeOpenAIError(w, http.StatusConflict, "conflict_error", "branch point is not stable while source work is active")
			return false
		default:
			return true
		}
	}
	if activeSource {
		if !validateActiveAnchor() {
			return
		}
	} else {
		var busy bool
		unlock, busy = s.lockBranchSourceRuntime(sourceSessionID)
		if busy {
			// Work may have become active after the initial sample. Prefer its
			// published durable boundary over rejecting an otherwise safe prefix.
			activeResponseID, _, _, _, activeAnchorRowID = s.activeTranscriptRun(sourceSessionID)
			activeSource = activeResponseID != ""
			if !activeSource {
				writeOpenAIError(w, http.StatusConflict, "conflict_error", "cannot branch while source work is active")
				return
			}
			unlock = func() {}
			if !validateActiveAnchor() {
				return
			}
		}
	}
	defer unlock()
	// This Web API is anchor/prefix-based. LegacyExpectedRev is decoded only for
	// backward wire compatibility and intentionally has no precondition effect:
	// unrelated suffix drift cannot mutate the immutable row-ID prefix. The
	// transaction still resolves and validates that anchor; lower-level callers
	// retain ExpectedRev/ExpectedState compare-and-swap behavior.
	result, err := branchStore.CreateBranch(r.Context(), sourceSessionID, session.CreateBranchOptions{
		AnchorMessageID: req.AnchorMessageID,
		IdempotencyKey:  req.IdempotencyKey,
	})
	switch {
	case errors.Is(err, session.ErrBranchConflict):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation changed in another client; refresh and try again")
	case errors.Is(err, session.ErrBranchIdempotencyConflict):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "idempotency key was already used for a different branch point")
	case errors.Is(err, session.ErrBranchingUnsupported):
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "conversation branching is unavailable")
	case errors.Is(err, session.ErrNotFound) && activeSource:
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "the active branch boundary changed; refresh and try again")
	case errors.Is(err, session.ErrNotFound):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "branch source or anchor was not found")
	case err != nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to create conversation branch")
	case result.Session == nil:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to create conversation branch")
	default:
		if !result.Reused {
			s.publishEvent(serveEventInput{Type: serveEventSessionCreated, SessionID: result.Session.ID, ParentSessionID: sourceSessionID, Reason: "branch"})
			s.publishEvent(serveEventInput{Type: serveEventChildrenChanged, SessionID: result.Session.ID, ParentSessionID: sourceSessionID, Reason: "branch_created"})
		}
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
	mode, focus, status, message := branchContextRequestValues(&responsesBranchContextRequest{Mode: mode, Focus: req.Focus})
	if status != 0 {
		writeOpenAIError(w, status, "invalid_request_error", message)
		return
	}
	workCtx, cancelWork := detachedBranchWorkContext(r.Context())
	defer cancelWork()
	snapshot, err := s.loadWebBranchContextSnapshot(workCtx, edge.ParentSessionID)
	if errors.Is(err, errWebBranchSnapshotUnstable) {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "branch context is changing; try again")
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to load branch context")
		return
	}
	source, sourceErr := session.MessagesAfterBranchAnchor(snapshot.messages, edge.ForkAfterMessageID)
	if sourceErr != nil && !snapshot.limited {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "branch source or anchor was not found")
		return
	}
	limitedMessage := ""
	if snapshot.limited {
		limitedMessage = "Context was prepared from the latest durable completed part of the active source; in-progress output was omitted."
	}
	if sourceErr != nil || len(source) == 0 {
		if snapshot.limited {
			limitedMessage = "The path was created without additional notes because no later source context was available."
		}
		writeJSON(w, http.StatusOK, prepareSessionPathNotesResponse{Ready: true, Limited: snapshot.limited, Message: limitedMessage})
		return
	}
	pathNote, status, message := s.generateBranchPathNote(workCtx, edge.ParentSessionID, source, mode, focus)
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
		s.publishEvent(serveEventInput{Type: serveEventSessionTranscriptChanged, SessionID: childSessionID, ParentSessionID: edge.ParentSessionID, Reason: "branch_path_note"})
	}
	writeJSON(w, http.StatusOK, prepareSessionPathNotesResponse{Ready: true, Limited: snapshot.limited, Message: limitedMessage})
}
