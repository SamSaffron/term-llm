package session

import (
	"context"
	"database/sql"
	"time"

	"github.com/samsaffron/term-llm/internal/session/notificationdb"
)

func notificationTerminal(t ResponseRunTerminal) notificationdb.ResponseRunTerminal {
	return notificationdb.ResponseRunTerminal{Recoverable: t.Recoverable, PendingClientCallIDs: t.PendingClientCallIDs, ResponseID: t.ResponseID, Outcome: t.Outcome, PendingClientCalls: t.PendingClientCalls, AwaitingAgentJoin: t.AwaitingAgentJoin, AwaitedEvents: notificationAgentFacts(t.AwaitedEvents)}
}
func prepareNotificationTerminalTx(ctx context.Context, tx *sql.Tx, t ResponseRunTerminal) (bool, error) {
	return notificationdb.PrepareTerminalTx(ctx, tx, notificationTerminal(t))
}
func insertNotificationCandidateTx(ctx context.Context, tx *sql.Tx, t ResponseRunTerminal, a AttentionState, now time.Time) error {
	return notificationdb.InsertCandidateTx(ctx, tx, notificationTerminal(t), notificationdb.AttentionState{Changed: a.Changed, StoreInstanceID: a.StoreInstanceID, LatestAttentionSeq: a.LatestAttentionSeq, RunEpoch: a.RunEpoch}, now)
}
