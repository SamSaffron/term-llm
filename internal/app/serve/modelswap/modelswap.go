package modelswap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
)

// VisibleOutput reports whether ev is output the user has seen, after which a
// failed attempt cannot silently fall back to a handover.
func VisibleOutput(ev core.Event) bool {
	switch ev.Type {
	case core.EventTextDelta:
		return ev.Text != ""
	case core.EventToolCall, core.EventToolExecStart, core.EventToolExecEnd, core.EventSteering, core.EventImageGenerated:
		return true
	default:
		return false
	}
}

// Label names one end of a switch as provider:model, or whichever is known.
func Label(provider, model string) string {
	provider, model = strings.TrimSpace(provider), strings.TrimSpace(model)
	switch {
	case provider != "" && model != "":
		return provider + ":" + model
	case provider != "":
		return provider
	case model != "":
		return model
	default:
		return "model"
	}
}

// StartMessage is the live progress text for a switch between two labelled
// runtimes. Efforts are normalized; empty means the provider default.
func StartMessage(previous, target, previousEffort, targetEffort string) string {
	effortLabel := func(effort string) string {
		if effort == "" {
			return "auto"
		}
		return effort
	}
	effortChanged := !strings.EqualFold(previousEffort, targetEffort)
	if previous == target && effortChanged {
		return fmt.Sprintf("Switching reasoning effort on %s: %s → %s; trying existing context…", previous, effortLabel(previousEffort), effortLabel(targetEffort))
	}
	if effortChanged {
		previous = fmt.Sprintf("%s / %s", previous, effortLabel(previousEffort))
		target = fmt.Sprintf("%s / %s", target, effortLabel(targetEffort))
	}
	return fmt.Sprintf("Switching model: %s → %s; trying existing context…", previous, target)
}

// Responded reports whether ev shows the target accepted the carried-over
// context and began answering, reasoning included. Stopping a turn after that
// ends the turn, not the switch.
func Responded(ev core.Event) bool {
	return ev.Type == core.EventReasoningDelta || VisibleOutput(ev)
}

// Stopped reports whether err ends a turn by cancellation or deadline rather
// than by a provider failure.
func Stopped(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// FallbackEligible reports whether a failed naive continuation looks like a
// carried-over context the target rejected, which a handover can repair.
// Cancellation, authentication, rate limits and unknown models are not.
func FallbackEligible(err error) bool {
	if err == nil || Stopped(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, deny := range []string{"401", "403", "429", "unauthorized", "forbidden", "rate limit", "ratelimit", "quota", "insufficient", "model not found", "unknown model"} {
		if strings.Contains(msg, deny) {
			return false
		}
	}
	for _, allow := range []string{"400", "invalid_request", "invalid request", "messages", "reasoning", "thinking", "encrypted", "tool_use", "tool_result", "function_call", "role", "signature"} {
		if strings.Contains(msg, allow) {
			return true
		}
	}
	return strings.Contains(msg, "invalid")
}

// CombinedError reports both failures of a naive attempt and its handover
// retry, keeping the retry error unwrappable.
func CombinedError(naiveErr, retryErr error) error {
	if naiveErr == nil {
		return retryErr
	}
	if retryErr == nil {
		return naiveErr
	}
	return fmt.Errorf("model swap naive continuation failed: %v; handover retry failed: %w", naiveErr, retryErr)
}

// InsertMarker places a switch marker after the latest user message, where
// live progress renders it: after the submitted input, before the reply. The
// trigger is the latest user message in both naive and handover histories, so
// anchoring there never matches an older turn whose prompt was repeated.
func InsertMarker(history []core.Message, marker core.Message) []core.Message {
	return insertAfterLatestUser(history, marker, func(m core.Message) core.Role { return m.Role })
}

// InsertMarkerRow is InsertMarker for stored rows. Rewriting stored rows keeps
// the turn, timing, compaction-tail and persistence-only parts that a round
// trip through provider messages would drop.
func InsertMarkerRow(rows []session.Message, marker session.Message) []session.Message {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Role == core.RoleUser {
			marker.TurnIndex = rows[i].TurnIndex // the switch belongs to the trigger's turn
			break
		}
	}
	return insertAfterLatestUser(rows, marker, func(m session.Message) core.Role { return m.Role })
}

// MarkerRows returns the session's stored active context with marker inserted
// by InsertMarkerRow, ready for an active-context replacement. The caller must
// hold the session until that write commits.
func MarkerRows(ctx context.Context, store session.Store, sessionID string, marker session.Message) ([]session.Message, error) {
	sess, err := store.Get(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	stored, err := session.LoadActiveMessages(ctx, store, sess)
	if err != nil {
		return nil, fmt.Errorf("load active transcript: %w", err)
	}
	if len(stored) == 0 {
		return nil, errors.New("active transcript is empty")
	}
	return InsertMarkerRow(stored, marker), nil
}

func insertAfterLatestUser[T any](items []T, marker T, role func(T) core.Role) []T {
	insertAt := len(items)
	for i := len(items) - 1; i >= 0; i-- {
		if role(items[i]) == core.RoleUser {
			insertAt = i + 1
			break
		}
	}
	updated := make([]T, 0, len(items)+1)
	updated = append(updated, items[:insertAt]...)
	updated = append(updated, marker)
	return append(updated, items[insertAt:]...)
}
