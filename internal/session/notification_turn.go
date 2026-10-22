package session

import (
	"context"
	"database/sql"
	"errors"

	"github.com/samsaffron/term-llm/internal/session/notificationdb"
)

// SupersedeNotificationTurn is for incorporated humans, never queued inputs.
func (s *SQLiteStore) SupersedeNotificationTurn(ctx context.Context, fence ResponseRunFence, origin NotificationOrigin) error {
	err := notificationdb.TransferTurn(ctx, s.db, fence.ResponseID, fence.OwnerInstanceID, fence.FencingToken, origin)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrResponseRunLeaseLost
	}
	return err
}

func notificationAgentFacts(events []AgentRun) []notificationdb.AgentRun {
	result := make([]notificationdb.AgentRun, len(events))
	for i, e := range events {
		result[i] = notificationdb.AgentRun{ID: e.ID, RunGeneration: e.RunGeneration}
	}
	return result
}
func admitNotificationTurnTx(ctx context.Context, tx *sql.Tx, a ResponseRunAdmission) error {
	value := notificationdb.ResponseRunAdmission{SessionID: a.SessionID, ResponseID: a.ResponseID, AgentEvents: notificationAgentFacts(a.AgentEvents)}
	if a.Turn != nil {
		value.Turn = &notificationdb.NotificationTurnAdmission{ClientResultIDs: a.Turn.ClientResultIDs, Human: a.Turn.Human, PreviousResponseID: a.Turn.PreviousResponseID, Origin: a.Turn.Origin, AwaitedEvents: notificationAgentFacts(a.Turn.AwaitedEvents)}
	}
	return notificationdb.AdmitTx(ctx, tx, value)
}

func (s *SQLiteStore) CancelNotificationTurn(ctx context.Context, responseID string) error {
	err := notificationdb.CancelTurn(ctx, s.db, responseID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
