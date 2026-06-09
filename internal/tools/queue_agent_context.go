package tools

import (
	"context"
	"strings"
)

const (
	QueueAgentOriginWeb      = "web"
	QueueAgentOriginTelegram = "telegram"

	QueueAgentNotifyOriginHeader         = "X-Term-LLM-Queue-Agent-Origin"
	QueueAgentNotifySessionIDHeader      = "X-Term-LLM-Queue-Agent-Session-ID"
	QueueAgentNotifyTelegramChatIDHeader = "X-Term-LLM-Queue-Agent-Telegram-Chat-ID"
)

// QueueAgentOriginContext carries the trusted runtime origin for queue_agent
// completion notifications. It is intentionally supplied by runtime code rather
// than by queue_agent tool arguments, so callers can only opt in to notification,
// not choose an arbitrary target.
type QueueAgentOriginContext struct {
	Origin         string
	SessionID      string
	TelegramChatID int64
}

type queueAgentOriginContextKey struct{}

// ContextWithQueueAgentOrigin stores trusted queue_agent notification origin
// metadata in ctx for the duration of a request.
func ContextWithQueueAgentOrigin(ctx context.Context, origin QueueAgentOriginContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	origin.Origin = strings.TrimSpace(origin.Origin)
	origin.SessionID = strings.TrimSpace(origin.SessionID)
	if origin.Origin == "" {
		return ctx
	}
	return context.WithValue(ctx, queueAgentOriginContextKey{}, origin)
}

// QueueAgentOriginFromContext returns trusted queue_agent notification origin
// metadata previously stored by ContextWithQueueAgentOrigin.
func QueueAgentOriginFromContext(ctx context.Context) (QueueAgentOriginContext, bool) {
	if ctx == nil {
		return QueueAgentOriginContext{}, false
	}
	origin, ok := ctx.Value(queueAgentOriginContextKey{}).(QueueAgentOriginContext)
	if !ok || strings.TrimSpace(origin.Origin) == "" {
		return QueueAgentOriginContext{}, false
	}
	origin.Origin = strings.TrimSpace(origin.Origin)
	origin.SessionID = strings.TrimSpace(origin.SessionID)
	return origin, true
}
