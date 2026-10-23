package notificationdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"
)

type NotificationOrigin struct {
	SubscriptionID string `json:"subscription_id"`
	WindowID       string `json:"window_id"`
	DocumentID     string `json:"document_id"`
	Source         string `json:"source"`
}
type NotificationPresence struct {
	PreviousDocumentID string    `json:"previous_document_id,omitempty"`
	Source             string    `json:"source"`
	Version            int       `json:"version"`
	WindowID           string    `json:"window_id"`
	DocumentID         string    `json:"document_id"`
	SubscriptionID     string    `json:"subscription_id"`
	Sequence           int64     `json:"sequence"`
	Active             bool      `json:"active"`
	ExpiresAt          time.Time `json:"expires_at"`
}
type CompletionNotification struct {
	ID                                                                                                    int64
	EventID, TurnID, ResponseID, SubscriptionID, WindowID, DocumentID, Source, StoreInstanceID, SessionID string
	AttentionSeq, RunEpoch                                                                                int64
	Outcome                                                                                               ResponseRunState
	CreatedAt, ExpiresAt                                                                                  time.Time
	AttemptCount                                                                                          int
	ClaimOwner, ClaimToken                                                                                string
	LeaseExpiresAt                                                                                        time.Time
}

var ErrNotificationLineage = errors.New("session: invalid notification continuation lineage")
var ErrNotificationPresenceConflict = errors.New("session: notification window document conflict")
var ErrNotificationClaimLost = errors.New("session: notification claim lost")

type ResponseRunState string

const (
	ResponseRunCompleted ResponseRunState = "completed"
	ResponseRunFailed    ResponseRunState = "failed"
	ResponseRunCancelled ResponseRunState = "cancelled"
)

type AgentRun struct {
	ID            string
	RunGeneration int64
}
type NotificationTurnAdmission struct {
	ClientResultIDs    []string
	Human              bool
	PreviousResponseID string
	Origin             NotificationOrigin
	AwaitedEvents      []AgentRun
}
type ResponseRunAdmission struct {
	Turn                  *NotificationTurnAdmission
	ResponseID, SessionID string
	AgentEvents           []AgentRun
}
type ResponseRunTerminal struct {
	PendingClientCallIDs                  []string
	Recoverable                           bool
	ResponseID                            string
	Outcome                               ResponseRunState
	PendingClientCalls, AwaitingAgentJoin bool
	AwaitedEvents                         []AgentRun
}
type AttentionState struct {
	Changed                      bool
	StoreInstanceID              string
	LatestAttentionSeq, RunEpoch int64
}
type QueryExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func NewID() string { return rand.Text() }
