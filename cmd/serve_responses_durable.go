package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const durableResponseMessagePrefix = "resp_msg_"

type latestVisibleMessageIDStore interface {
	GetLatestVisibleMessageID(ctx context.Context, sessionID string) (int64, error)
}

func durableResponseIDForMessageID(id int64) string {
	if id <= 0 {
		return ""
	}
	return fmt.Sprintf("%s%d", durableResponseMessagePrefix, id)
}

func parseDurableResponseMessageID(responseID string) (int64, bool) {
	trimmed := strings.TrimSpace(responseID)
	if !strings.HasPrefix(trimmed, durableResponseMessagePrefix) {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(trimmed, durableResponseMessagePrefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// parseBranchDurableResponseMessageID additionally accepts resp_msg_0 as the
// branch-only representation of an empty copied transcript. Ordinary response
// continuation must keep rejecting zero because it has no durable message row.
func parseBranchDurableResponseMessageID(responseID string) (int64, bool) {
	if strings.TrimSpace(responseID) == durableResponseMessagePrefix+"0" {
		return 0, true
	}
	return parseDurableResponseMessageID(responseID)
}

func isVisibleContinuationRole(role llm.Role) bool {
	return role == llm.RoleUser || role == llm.RoleAssistant
}

func latestVisibleMessage(messages []session.Message) (session.Message, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if isVisibleContinuationRole(messages[i].Role) && !messages[i].CompactionTail {
			return messages[i], true
		}
	}
	return session.Message{}, false
}

func latestVisibleMessageID(ctx context.Context, store session.Store, sessionID string) (int64, error) {
	if store == nil || sessionID == "" {
		return 0, nil
	}
	if getter, ok := store.(latestVisibleMessageIDStore); ok {
		return getter.GetLatestVisibleMessageID(ctx, sessionID)
	}
	msgs, err := store.GetMessages(ctx, sessionID, 0, 0)
	if err != nil {
		return 0, err
	}
	msg, ok := latestVisibleMessage(msgs)
	if !ok {
		return 0, nil
	}
	return msg.ID, nil
}

func latestDurableResponseID(messages []session.Message) string {
	msg, ok := latestVisibleMessage(messages)
	if !ok {
		return ""
	}
	return durableResponseIDForMessageID(msg.ID)
}

type durablePreviousResponseResolution struct {
	sessionID string
	latestID  string
}

func (s *serveServer) lockBranchSourceRuntime(sessionID string) (func(), bool) {
	if s == nil {
		return func() {}, false
	}
	if s.responseRuns != nil && s.responseRuns.activeRunID(sessionID) != "" {
		return nil, true
	}
	if s.sessionMgr == nil {
		return func() {}, false
	}
	runtime, ok := s.sessionMgr.Get(sessionID)
	if !ok || runtime == nil {
		return func() {}, false
	}
	if runtime.hasActiveActivity() || !runtime.mu.TryLock() {
		return nil, true
	}
	if runtime.hasActiveActivity() {
		runtime.mu.Unlock()
		return nil, true
	}
	return runtime.mu.Unlock, false
}

func (s *serveServer) resolveDurableBranch(ctx context.Context, previousResponseID, headerSessionID string, inputMessages []llm.Message, allowIdentifiedUserBatch bool, expectedRev *int64, idempotencyKey string) (durablePreviousResponseResolution, int, string) {
	msgID, ok := parseBranchDurableResponseMessageID(previousResponseID)
	if !ok {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, "branch requires a durable previous_response_id"
	}
	if s.store == nil {
		return durablePreviousResponseResolution{}, http.StatusConflict, "conversation branching is unavailable"
	}
	if err := validateDurableContinuationInput(inputMessages, allowIdentifiedUserBatch); err != nil {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, err.Error()
	}

	sourceSessionID := strings.TrimSpace(headerSessionID)
	if msgID > 0 {
		msg, err := getMessageByID(ctx, s.store, msgID)
		if err != nil || msg.ID == 0 {
			return durablePreviousResponseResolution{}, http.StatusBadRequest, fmt.Sprintf("previous_response_id %q not found", previousResponseID)
		}
		if !isVisibleContinuationRole(msg.Role) {
			return durablePreviousResponseResolution{}, http.StatusBadRequest, "previous_response_id must refer to a user or assistant message"
		}
		if sourceSessionID != "" && sourceSessionID != msg.SessionID {
			return durablePreviousResponseResolution{}, http.StatusConflict, fmt.Sprintf("session_id %q conflicts with previous_response_id session %q", sourceSessionID, msg.SessionID)
		}
		sourceSessionID = msg.SessionID
	} else if sourceSessionID == "" {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, "empty-transcript branch requires session_id"
	}

	unlockSource, busy := s.lockBranchSourceRuntime(sourceSessionID)
	if busy {
		return durablePreviousResponseResolution{}, http.StatusConflict, "cannot branch while source work is active"
	}
	defer unlockSource()

	branchStore, ok := s.store.(session.ConversationBranchStore)
	if !ok {
		return durablePreviousResponseResolution{}, http.StatusConflict, "conversation branching is unavailable"
	}
	result, err := branchStore.CreateBranch(ctx, sourceSessionID, session.CreateBranchOptions{
		AnchorMessageID: msgID,
		ExpectedRev:     expectedRev,
		IdempotencyKey:  strings.TrimSpace(idempotencyKey),
	})
	switch {
	case errors.Is(err, session.ErrBranchConflict):
		return durablePreviousResponseResolution{}, http.StatusConflict, "conversation changed in another client; refresh and try again"
	case errors.Is(err, session.ErrBranchingUnsupported):
		return durablePreviousResponseResolution{}, http.StatusConflict, "conversation branching is unavailable"
	case errors.Is(err, session.ErrNotFound):
		return durablePreviousResponseResolution{}, http.StatusBadRequest, "branch source or anchor was not found"
	case err != nil:
		return durablePreviousResponseResolution{}, http.StatusInternalServerError, "failed to create conversation branch"
	case result.Session == nil || strings.TrimSpace(result.Session.ID) == "":
		return durablePreviousResponseResolution{}, http.StatusInternalServerError, "failed to create conversation branch"
	}
	latestID := durableResponseIDForMessageID(result.AnchorMessageID)
	if result.AnchorMessageID == 0 {
		latestID = durableResponseMessagePrefix + "0"
	}
	return durablePreviousResponseResolution{
		sessionID: result.Session.ID,
		latestID:  latestID,
	}, 0, ""
}

func (s *serveServer) resolveDurablePreviousResponseID(ctx context.Context, previousResponseID, headerSessionID string, inputMessages []llm.Message, allowIdentifiedUserBatch bool) (durablePreviousResponseResolution, int, string) {
	msgID, ok := parseDurableResponseMessageID(previousResponseID)
	if !ok {
		return durablePreviousResponseResolution{}, 0, ""
	}
	if s.store == nil {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, fmt.Sprintf("previous_response_id %q not found (session history is unavailable)", previousResponseID)
	}
	if err := validateDurableContinuationInput(inputMessages, allowIdentifiedUserBatch); err != nil {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, err.Error()
	}

	msg, err := getMessageByID(ctx, s.store, msgID)
	if err != nil || msg.ID == 0 {
		if _, ok := s.responseToSession.Load(previousResponseID); ok {
			return durablePreviousResponseResolution{}, 0, ""
		}
		return durablePreviousResponseResolution{}, http.StatusBadRequest, fmt.Sprintf("previous_response_id %q not found", previousResponseID)
	}
	if !isVisibleContinuationRole(msg.Role) {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, "previous_response_id must refer to a user or assistant message"
	}
	if headerSessionID != "" && headerSessionID != msg.SessionID {
		return durablePreviousResponseResolution{}, http.StatusConflict, fmt.Sprintf("session_id %q conflicts with previous_response_id session %q", headerSessionID, msg.SessionID)
	}
	latestMsgID, err := latestVisibleMessageID(ctx, s.store, msg.SessionID)
	if err != nil {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, fmt.Sprintf("previous_response_id %q not found", previousResponseID)
	}
	if latestMsgID == 0 || latestMsgID != msg.ID {
		latestID := durableResponseIDForMessageID(latestMsgID)
		if latestID == "" {
			latestID = "unknown"
		}
		return durablePreviousResponseResolution{}, http.StatusConflict, fmt.Sprintf("previous_response_id %q is stale; latest is %q", previousResponseID, latestID)
	}
	return durablePreviousResponseResolution{sessionID: msg.SessionID, latestID: durableResponseIDForMessageID(msg.ID)}, 0, ""
}

func validateDurableContinuationInput(inputMessages []llm.Message, allowIdentifiedUserBatch bool) error {
	if len(inputMessages) == 1 && inputMessages[0].Role == llm.RoleUser {
		return nil
	}
	if allowIdentifiedUserBatch && len(inputMessages) > 1 {
		seen := make(map[string]struct{}, len(inputMessages))
		for i := range inputMessages {
			id := strings.TrimSpace(inputMessages[i].ClientMessageID)
			if inputMessages[i].Role != llm.RoleUser || id == "" {
				return fmt.Errorf("identified user batch requires only user messages with client_message_id")
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("identified user batch contains duplicate client_message_id %q", id)
			}
			seen[id] = struct{}{}
		}
		return nil
	}
	if len(inputMessages) > 0 {
		allTools := true
		for _, msg := range inputMessages {
			if msg.Role != llm.RoleTool {
				allTools = false
				break
			}
		}
		if allTools {
			return nil
		}
	}
	return fmt.Errorf("message-backed previous_response_id requires exactly one new user message or one or more tool results")
}

func getMessageByID(ctx context.Context, store session.Store, msgID int64) (session.Message, error) {
	msg, err := store.GetMessageByID(ctx, msgID)
	if msg != nil {
		return *msg, err
	}
	return session.Message{}, err
}

func (s *serveServer) latestDurableResponseIDForSession(ctx context.Context, sessionID string) string {
	if s == nil || s.store == nil || sessionID == "" {
		return ""
	}
	msgID, err := latestVisibleMessageID(ctx, s.store, sessionID)
	if err != nil {
		return ""
	}
	return durableResponseIDForMessageID(msgID)
}

func (s *serveServer) latestDurableResponseIDForSessionBestEffort(ctx context.Context, sessionID string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), durableResponseLookupLimit)
	defer cancel()
	return s.latestDurableResponseIDForSession(lookupCtx, sessionID)
}
