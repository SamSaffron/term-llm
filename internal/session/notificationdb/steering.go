package notificationdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func SaveSteeringOriginTx(ctx context.Context, q QueryExecer, sessionID, steeringID string, origin *NotificationOrigin, human bool) error {
	// Persist even an empty origin for actual humans: absence must silence the
	// previous destination when this input is eventually incorporated.
	if !human {
		return nil
	}
	if origin == nil {
		origin = &NotificationOrigin{}
	}
	data, err := json.Marshal(origin)
	if err != nil {
		return err
	}
	var stored string
	err = q.QueryRowContext(ctx, `SELECT origin FROM notification_steering_origins WHERE session_id=? AND steering_id=?`, sessionID, steeringID).Scan(&stored)
	if err == nil {
		if stored != string(data) {
			return ErrSteeringOriginConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	captured, err := ValidOriginTx(ctx, q, *origin)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO notification_steering_origins(session_id,steering_id,origin,validated) VALUES(?,?,?,?)`, sessionID, steeringID, string(data), captured.SubscriptionID != "")
	return err
}

func IncorporateSteeringTx(ctx context.Context, q QueryExecer, sessionID, steeringID, responseID string) error {
	var provenance, encoded string
	var validated bool
	err := q.QueryRowContext(ctx, `SELECT p.origin,COALESCE(n.origin,''),COALESCE(n.validated,0) FROM session_pending_steering p LEFT JOIN notification_steering_origins n ON n.session_id=p.session_id AND n.steering_id=p.id WHERE p.session_id=? AND p.id=?`, sessionID, steeringID).Scan(&provenance, &encoded, &validated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if provenance != "user" {
		return nil
	}
	var origin NotificationOrigin
	if encoded != "" && validated {
		if err = json.Unmarshal([]byte(encoded), &origin); err != nil {
			return err
		}
	}
	if responseID == "" {
		return SupersedeTx(ctx, q, sessionID)
	}
	return capturedTurnTx(ctx, q, sessionID, responseID, origin)
}

var ErrSteeringOriginConflict = errors.New("session: conflicting steering notification origin")

func SaveRushOriginTx(ctx context.Context, q QueryExecer, sessionID, requestID string, origin NotificationOrigin) error {
	captured, err := ValidOriginTx(ctx, q, origin)
	if err != nil {
		return err
	}
	data, err := json.Marshal(captured)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO notification_rush_origins(session_id,request_id,origin) VALUES(?,?,?)`, sessionID, requestID, string(data))
	return err
}
func CommitRushOriginTx(ctx context.Context, q QueryExecer, sessionID, requestID, responseID string) error {
	var encoded string
	err := q.QueryRowContext(ctx, `SELECT origin FROM notification_rush_origins WHERE session_id=? AND request_id=?`, sessionID, requestID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var origin NotificationOrigin
	if err = json.Unmarshal([]byte(encoded), &origin); err != nil {
		return err
	}
	if responseID == "" {
		return SupersedeTx(ctx, q, sessionID)
	}
	return capturedTurnTx(ctx, q, sessionID, responseID, origin)
}
