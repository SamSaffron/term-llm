package cmd

import (
	"context"
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

func (s *serveServer) resolveDurablePreviousResponseID(ctx context.Context, previousResponseID, headerSessionID string, inputMessages []llm.Message) (durablePreviousResponseResolution, int, string) {
	msgID, ok := parseDurableResponseMessageID(previousResponseID)
	if !ok {
		return durablePreviousResponseResolution{}, 0, ""
	}
	if s.store == nil {
		return durablePreviousResponseResolution{}, http.StatusBadRequest, fmt.Sprintf("previous_response_id %q not found (session history is unavailable)", previousResponseID)
	}
	if err := validateDurableContinuationInput(inputMessages); err != nil {
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

func validateDurableContinuationInput(inputMessages []llm.Message) error {
	if len(inputMessages) != 1 {
		return fmt.Errorf("message-backed previous_response_id requires exactly one new user message")
	}
	if inputMessages[0].Role != llm.RoleUser && inputMessages[0].Role != llm.RoleTool {
		return fmt.Errorf("message-backed previous_response_id only accepts one new user message or tool result")
	}
	return nil
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
