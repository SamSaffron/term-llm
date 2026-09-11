package cmd

import (
	"net/http"
	"strings"

	"github.com/samsaffron/term-llm/internal/session"
)

func (s *serveServer) routeSessionCore(w http.ResponseWriter, r *http.Request, sessionID, requestedSessionID, suffix string) bool {
	if suffix == "shell" || strings.HasPrefix(suffix, "shell/") {
		s.handleSessionShell(w, r, sessionID, suffix)
		return true
	}

	if suffix == "attention/seen" {
		s.handleSessionAttentionSeen(w, r, sessionID)
		return true
	}

	if suffix == "children" {
		s.handleSessionChildren(w, r, sessionID)
		return true
	}

	if suffix == "project" {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			writeProjectError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return true
		}
		s.handleSessionProjectAssignment(w, r, sessionID)
		return true
	}

	if suffix == "skills" || suffix == "skills/invoke" || strings.HasPrefix(suffix, "skill-runs/") {
		s.handleSessionSkills(w, r, sessionID, requestedSessionID, suffix)
		return true
	}

	if suffix == "" && r.Method == http.MethodPatch {
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionMetadataPatch(w, r, sessionID)
		return true
	}

	if suffix == "interrupt" || suffix == "steering" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionInterrupt(w, r, sessionID)
		return true
	}

	return false
}

func (s *serveServer) routeSessionRuntime(w http.ResponseWriter, r *http.Request, sessionID, requestedSessionID, suffix string) bool {
	if suffix == "runtime/approvals" {
		s.handleSessionApprovalMode(w, r, sessionID)
		return true
	}

	if suffix == "runtime/compact" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionRuntimeCompact(w, r, sessionID)
		return true
	}

	if suffix == "runtime/undo" || suffix == "runtime/redo" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionUndoRedo(w, r, sessionID, suffix == "runtime/redo")
		return true
	}

	if suffix == "runtime/effort" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionRuntimeEffort(w, r, sessionID)
		return true
	}

	if suffix == "runtime/goal" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionRuntimeGoal(w, r, sessionID)
		return true
	}

	if suffix == "steering/rush" || strings.HasPrefix(suffix, "steering/rush/") {
		s.handleSteeringRush(w, r, sessionID, strings.TrimPrefix(strings.TrimPrefix(suffix, "steering/rush"), "/"))
		return true
	}
	if strings.HasPrefix(suffix, "interjections/") {
		suffix = "steering/" + strings.TrimPrefix(suffix, "interjections/")
	}
	if strings.HasPrefix(suffix, "steering/") {
		id := strings.TrimPrefix(suffix, "steering/")
		id = strings.TrimSuffix(id, "/cancel")
		if r.Method != http.MethodDelete && r.Method != http.MethodPost {
			w.Header().Set("Allow", "DELETE, POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleSessionSteeringCancel(w, r, sessionID, id)
		return true
	}

	if suffix == "ask_user" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionAskUser(w, r, sessionID)
		return true
	}

	if suffix == "approval" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleSessionApproval(w, r, sessionID)
		return true
	}
	return false
}

func (s *serveServer) routeSessionState(w http.ResponseWriter, r *http.Request, sessionID, requestedSessionID, suffix string) bool {
	if suffix == "title/refine" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleSessionTitleRefine(w, r, sessionID)
		return true
	}

	if suffix == "state" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleSessionState(w, r, sessionID)
		return true
	}

	if serverName, action, ok := parseSessionMCPOAuthSuffix(suffix); ok {
		s.handleSessionMCPOAuth(w, r, sessionID, serverName, action)
		return true
	}

	if suffix == "mcp" {
		if r.Method != http.MethodGet && r.Method != http.MethodPatch {
			w.Header().Set("Allow", "GET, PATCH")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if r.Method == http.MethodPatch {
			if err := requireJSONContentType(r); err != nil {
				writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
				return true
			}
		}
		s.handleSessionMCP(w, r, sessionID)
		return true
	}

	if suffix == "diff-comments" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleSessionDiffComments(w, r, sessionID)
		return true
	}
	return false
}

func (s *serveServer) routeSessionCommit(w http.ResponseWriter, r *http.Request, sessionID, requestedSessionID, suffix string) bool {
	if suffix == "commit/publish-plan" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleCommitPublishPlan(w, r, sessionID)
		return true
	}
	if suffix == "commit/status" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleCommitStatus(w, r, sessionID)
		return true
	}
	if suffix == "commit/stage" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleCommitStage(w, r, sessionID)
		return true
	}
	if suffix == "commit-runs" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleCreateCommitRun(w, r, sessionID)
		return true
	}
	if strings.HasPrefix(suffix, "commit-runs/") {
		rest := strings.TrimPrefix(suffix, "commit-runs/")
		if strings.HasSuffix(rest, "/events") {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
				return true
			}
			s.handleCommitRunEvents(w, r, sessionID, strings.TrimSuffix(rest, "/events"))
			return true
		}
		switch r.Method {
		case http.MethodGet:
			s.handleGetCommitRun(w, r, sessionID, rest)
		case http.MethodDelete:
			s.handleCancelCommitRun(w, r, sessionID, rest)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		}
		return true
	}
	if suffix == "commit-operations" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleCreateCommitOperation(w, r, sessionID)
		return true
	}
	if strings.HasPrefix(suffix, "commit-operations/") {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleGetCommitOperation(w, r, sessionID, strings.TrimPrefix(suffix, "commit-operations/"))
		return true
	}
	return false
}

func (s *serveServer) routeSessionResources(w http.ResponseWriter, r *http.Request, sessionID, requestedSessionID, suffix string) bool {
	if suffix == "file-changes" || suffix == "file-changes/diff" || suffix == "file-changes/content" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		switch suffix {
		case "file-changes":
			s.handleSessionFileChanges(w, r, sessionID)
		case "file-changes/diff":
			s.handleSessionFileChangeDiff(w, r, sessionID)
		default:
			s.handleSessionFileChangeContent(w, r, sessionID)
		}
		return true
	}

	if suffix == "tree" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		s.handleSessionTree(w, r, sessionID)
		return true
	}

	if suffix == "shares" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		s.handleCreateSessionShare(w, r, sessionID)
		return true
	}

	if suffix == "branches" || suffix == "path-notes" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if err := requireJSONContentType(r); err != nil {
			writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
			return true
		}
		if suffix == "branches" {
			s.handleCreateSessionBranch(w, r, sessionID)
		} else {
			s.handleSessionPathNotes(w, r, sessionID)
		}
		return true
	}

	if suffix == "transcript" || suffix == "transcript/bodies" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return true
		}
		if suffix == "transcript" {
			s.handleSessionTranscript(w, r, sessionID)
		} else {
			s.handleSessionTranscriptBodies(w, r, sessionID)
		}
		return true
	}
	return false
}

func (s *serveServer) handleSessionMessagesRoute(w http.ResponseWriter, r *http.Request, sessionID, suffix string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	if suffix != "messages" {
		http.NotFound(w, r)
		return
	}
	// Keep the legacy messages endpoint for non-UI consumers and cache-first
	// service-worker skew: an older cached web shell may run briefly against a
	// revision-aware server. New clients use /transcript (and only fall back here
	// when that endpoint is absent on an older server).
	if s.store == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "session history is unavailable")
		return
	}

	query := r.URL.Query()
	limit := parseSessionMessagesLimit(query.Get("limit"))
	_, hasBeforeSeqParam := query["before_seq"]
	reverseMode := parseSessionMessagesTail(query.Get("tail")) || hasBeforeSeqParam

	var msgs []session.Message
	var hasMore bool
	var nextOffset int
	var nextBeforeSeq int
	var err error

	if reverseMode {
		beforeSeq := parseSessionMessagesBeforeSeq(query.Get("before_seq"))
		descending, pageErr := s.getSessionMessagesPageDescending(r.Context(), sessionID, beforeSeq, limit+1)
		if pageErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get messages")
			return
		}
		hasMore = len(descending) > limit
		if hasMore {
			descending = descending[:limit]
		}
		if hasMore && len(descending) > 0 {
			nextBeforeSeq = descending[len(descending)-1].Sequence
		}
		msgs = make([]session.Message, len(descending))
		for i := range descending {
			msgs[len(descending)-1-i] = descending[i]
		}
	} else {
		offset := parseSessionMessagesOffset(query.Get("offset"))
		msgs, err = s.store.GetMessages(r.Context(), sessionID, limit+1, offset)
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to get messages")
			return
		}
		hasMore = len(msgs) > limit
		if hasMore {
			msgs = msgs[:limit]
		}
		nextOffset = offset + len(msgs)
	}

	compactionSeq := -1
	compactionCount := 0
	if meta, metaErr := s.store.Get(r.Context(), sessionID); metaErr == nil && session.HasCompactionBoundary(meta) {
		compactionSeq = meta.CompactionSeq
		compactionCount = meta.CompactionCount
	}

	resp := sessionMessagesResponse{
		LastResponseID: s.latestDurableResponseIDForSession(r.Context(), sessionID),
		Messages:       s.sessionMessageEntries(msgs),
		HasMore:        hasMore,
	}
	if compactionSeq >= 0 {
		resp.CompactionSeq = &compactionSeq
		resp.CompactionCount = compactionCount
	}
	if hasMore {
		if reverseMode {
			resp.NextBeforeSeq = nextBeforeSeq
		} else {
			resp.NextOffset = nextOffset
		}
	}
	s.writeSessionMessagesResponse(w, r, resp)
}
