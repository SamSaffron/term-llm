package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	requestNotificationVersionHeader  = "X-Term-LLM-Notification-Version"
	requestNotificationWindowHeader   = "X-Term-LLM-Window-ID"
	requestNotificationDocumentHeader = "X-Term-LLM-Document-ID"
	requestNotificationSourceHeader   = "X-Term-LLM-Notification-Source"
)

// notificationOrigin is attribution only. Durable admission additionally checks
// the authenticated document/subscription binding; unknown clients fail closed.
func (s *serveServer) notificationOrigin(r *http.Request) session.NotificationOrigin {
	if r == nil || r.Header.Get(requestNotificationVersionHeader) != "2" || !isFirstPartyUIResponseRequest(r) {
		return session.NotificationOrigin{}
	}
	origin := session.NotificationOrigin{
		SubscriptionID: strings.TrimSpace(r.Header.Get(requestPushSubscriptionHeader)),
		WindowID:       strings.TrimSpace(r.Header.Get(requestNotificationWindowHeader)),
		DocumentID:     strings.TrimSpace(r.Header.Get(requestNotificationDocumentHeader)),
		Source:         strings.TrimSuffix(strings.TrimSpace(r.Header.Get(requestNotificationSourceHeader)), "/"),
	}
	if !validNotificationSource(origin.Source) {
		return session.NotificationOrigin{}
	}
	if origin.SubscriptionID == "" || !validNotificationID(origin.WindowID) || !validNotificationID(origin.DocumentID) || len(origin.SubscriptionID) > 100 || len(origin.Source) > 256 {
		return session.NotificationOrigin{}
	}
	if _, err := s.validateCompletionPushTarget(r.Context(), origin.SubscriptionID); err != nil {
		return session.NotificationOrigin{}
	}
	return origin
}

func validNotificationSource(value string) bool {
	if len(value) == 0 || len(value) > 256 || strings.ContainsAny(value, "?#") || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return false
	}
	u, err := url.ParseRequestURI(value)
	return err == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == path.Clean(u.Path) && !strings.ContainsAny(u.Path, "\\\\\x00")
}

func validNotificationID(value string) bool {
	if len(value) < 8 || len(value) > 100 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func (s *serveServer) notificationTurnAdmission(ctx context.Context, messages []core.Message, previous string) *session.NotificationTurnAdmission {
	admission := &session.NotificationTurnAdmission{PreviousResponseID: previous}
	for _, msg := range messages {
		if msg.Role == core.RoleUser && !core.IsGoalSteeringMessage(msg) {
			admission.Human = true
		}
	}
	if !admission.Human {
		// Tool-only input is eligible solely through a validated pending predecessor.
		for _, msg := range messages {
			if msg.Role != core.RoleTool {
				return nil
			}
			for _, part := range msg.Parts {
				if part.ToolResult != nil {
					admission.ClientResultIDs = append(admission.ClientResultIDs, part.ToolResult.ID)
				}
			}
		}
		if id, ok := parseDurableResponseMessageID(previous); ok {
			if s.store == nil {
				return nil
			}
			message, err := getMessageByID(ctx, s.store, id)
			if err != nil || message.ResponseID == "" {
				return nil
			}
			admission.PreviousResponseID = message.ResponseID
		}
		if previous == "" {
			return nil
		}
	} else if origin, ok := session.NotificationOriginFromContext(ctx); ok {
		admission.Origin = origin
	}
	return admission
}

func (s *serveServer) handleNotificationPresence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if err := requireJSONContentType(r); err != nil {
		writeOpenAIError(w, http.StatusUnsupportedMediaType, "invalid_request_error", err.Error())
		return
	}
	store, ok := session.AsNotificationDeliveryStore(s.store)
	if !ok {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unsupported_error", "notification ownership unavailable")
		return
	}
	var presence session.NotificationPresence
	if err := decodeJSONBody(r, &presence); err != nil || !sameOriginShellRequest(r) || !validNotificationSource(presence.Source) || presence.Version != 2 || !validNotificationID(presence.WindowID) || !validNotificationID(presence.DocumentID) || len(presence.SubscriptionID) > 100 || presence.Sequence <= 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid notification presence")
		return
	}
	if _, err := s.validateCompletionPushTarget(r.Context(), presence.SubscriptionID); err != nil {
		writeOpenAIError(w, http.StatusConflict, "notification_origin_conflict", "notification enrollment changed")
		return
	}
	if err := store.UpdateNotificationPresence(r.Context(), presence); err != nil {
		status := http.StatusConflict
		if errors.Is(err, context.Canceled) {
			status = http.StatusRequestTimeout
		}
		writeOpenAIError(w, status, "notification_origin_conflict", "notification document binding changed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": 2})
}

// Include immutable human origin in idempotency admission without turning a
// reload's fresh document incarnation into a different user request.
func notificationRequestFingerprint(r *http.Request, fingerprint string) string {
	if fingerprint == "" || r.Header.Get(requestNotificationVersionHeader) != "2" {
		return fingerprint
	}
	sum := sha256.Sum256([]byte(fingerprint + "\x00" + r.Header.Get(requestNotificationWindowHeader) + "\x00" + r.Header.Get(requestPushSubscriptionHeader) + "\x00" + r.Header.Get(requestNotificationSourceHeader)))
	return hex.EncodeToString(sum[:])
}

func (s *serveServer) validateResolvedResponsesAdmission(w http.ResponseWriter, r *http.Request, ctx context.Context, draftID, sessionID string) bool {
	// Fence the resolved identity, not just the header. previous_response_id
	// reaches a session by a different key, and L0 made child transcripts — and
	// therefore their durable response IDs — ordinary things a client now holds.
	if delegated, delegatedErr := s.sessionIsDelegatedRun(ctx, sessionID); delegatedErr != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "failed to look up session")
		return true
	} else if delegated {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", delegatedSessionWriteMessage)
		return true
	}

	if draftID != "" {
		if err := s.promoteDraftMCPSelection(ctx, draftID, sessionID); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
			return true
		}
	}

	if _, branchContextActive := s.branchNotes.Load(sessionID); branchContextActive {
		writeOpenAIError(w, http.StatusConflict, "conflict_error", "branch context is still being prepared")
		return true
	}

	return false
}
