package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

var (
	// ErrPinnedOrderUnsupported means the store cannot persist pin state with
	// an explicit order (for example a read-only or pre-v60 database).
	ErrPinnedOrderUnsupported = errors.New("session: pinned order unsupported")
	// ErrPinnedOrderInvalid means a reorder request was empty, oversized, or
	// listed a session more than once.
	ErrPinnedOrderInvalid = errors.New("session: invalid pinned order")
	// ErrPinnedOrderConflict means a reorder named a session that is no longer
	// pinned; the caller's view is stale and must be refreshed.
	ErrPinnedOrderConflict = errors.New("session: pinned order conflict")
)

// MaxPinnedOrderIDs bounds one reorder request.
const MaxPinnedOrderIDs = 1000

// unrankedPinKey is the Go value of unrankedPinKeySQL.
const unrankedPinKey int64 = math.MaxInt64

// PinState is the committed pin state of one session.
type PinState struct {
	Pinned   bool
	PinOrder int64
	// Changed reports whether this call modified the stored state.
	Changed bool
}

// PinnedPosition is one pinned session's persisted 1-based rank.
type PinnedPosition struct {
	ID       string `json:"id"`
	PinOrder int64  `json:"pin_order"`
}

// PinnedOrder is the complete pinned order committed by a reorder.
type PinnedOrder struct {
	// Positions lists every pinned session, including ones the caller did not
	// name, in rank order.
	Positions []PinnedPosition
	// Changed lists the sessions whose stored rank changed.
	Changed []string
}

// PinnedSessionStore owns pinned state and its explicit order. Full-session
// Update writes neither, so a stale in-memory Session snapshot held by a
// long-running runtime cannot unpin, re-pin, or reorder a conversation.
type PinnedSessionStore interface {
	// SetSessionPinned appends a newly pinned session after every existing
	// pin, keeps an already pinned session's rank, and clears the rank on
	// unpin. Repeating a request is a no-op.
	SetSessionPinned(ctx context.Context, id string, pinned bool) (PinState, error)
	// ReorderPinnedSessions atomically places the listed pinned sessions, in
	// the given order, into the ranks they currently occupy. Pinned sessions
	// that are not listed (for example archived pins the caller cannot see)
	// keep their positions, so a caller may send just its visible pins. Unknown
	// IDs return ErrNotFound and unpinned IDs ErrPinnedOrderConflict without
	// changing any rank. Repeating a request is a no-op.
	ReorderPinnedSessions(ctx context.Context, orderedIDs []string) (PinnedOrder, error)
}

// AsPinnedSessionStore resolves the pin capability through decorators without
// advertising it for stores that cannot write an explicit order.
func AsPinnedSessionStore(store Store) (PinnedSessionStore, bool) {
	if store == nil {
		return nil, false
	}
	if logging, ok := store.(*LoggingStore); ok {
		return AsPinnedSessionStore(logging.Store)
	}
	if sqlite, ok := store.(*SQLiteStore); ok && !sqlite.pinOrderWritable() {
		return nil, false
	}
	pins, ok := store.(PinnedSessionStore)
	return pins, ok
}

// SetSessionPinned persists pin state through PinnedSessionStore when the store
// supports it. Custom stores without the capability fall back to a full-session
// Update, which cannot maintain an explicit order.
func SetSessionPinned(ctx context.Context, store Store, id string, pinned bool) (PinState, error) {
	if pins, ok := AsPinnedSessionStore(store); ok {
		return pins.SetSessionPinned(ctx, id, pinned)
	}
	if store == nil || isSQLiteBacked(store) {
		// SQLite Update deliberately ignores pin state.
		return PinState{}, ErrPinnedOrderUnsupported
	}
	sess, err := store.Get(ctx, id)
	if err != nil {
		return PinState{}, err
	}
	if sess == nil {
		return PinState{}, ErrNotFound
	}
	if sess.Pinned == pinned {
		return PinState{Pinned: pinned, PinOrder: sess.PinOrder}, nil
	}
	sess.Pinned = pinned
	if err := store.Update(ctx, sess); err != nil {
		return PinState{}, err
	}
	return PinState{Pinned: pinned, Changed: true}, nil
}

func isSQLiteBacked(store Store) bool {
	if logging, ok := store.(*LoggingStore); ok {
		return isSQLiteBacked(logging.Store)
	}
	_, ok := store.(*SQLiteStore)
	return ok
}

func (s *SQLiteStore) pinOrderWritable() bool {
	return !s.cfg.ReadOnly && s.hasPinned && s.hasPinOrder
}

// pinOrderCol selects the persisted rank, or NULL on a database that predates it.
func (s *SQLiteStore) pinOrderCol(alias string) string {
	if !s.hasPinned || !s.hasPinOrder {
		return "NULL"
	}
	return qualifiedMessageColumn(alias, "pin_order")
}

// pinnedSelectCols selects the pinned flag followed by its persisted rank.
func (s *SQLiteStore) pinnedSelectCols(alias string) string {
	if !s.hasPinned {
		return "FALSE, NULL"
	}
	return "COALESCE(" + qualifiedMessageColumn(alias, "pinned") + ", FALSE), " + s.pinOrderCol(alias)
}

// pinnedOrderPrefix returns the leading ORDER BY terms that list pinned
// sessions first, in their persisted order, ahead of the caller's activity
// order. It matches the expressions indexed by pinOrderSchemaV60.
func (s *SQLiteStore) pinnedOrderPrefix(alias string) string {
	if !s.hasPinned {
		return ""
	}
	prefix := "COALESCE(" + qualifiedMessageColumn(alias, "pinned") + ", FALSE) DESC, "
	if s.hasPinOrder {
		prefix += pinnedRankSQL(alias) + " ASC, "
	}
	return prefix
}

// pinOrderValue exposes a rank only for pinned rows.
func pinOrderValue(pinned bool, raw sql.NullInt64) int64 {
	if !pinned {
		return 0
	}
	return rankValue(raw)
}

// cursorPinRank is a boundary row's pinnedRankSQL value.
func cursorPinRank(summary SessionSummary) int64 {
	switch {
	case !summary.Pinned:
		return 0
	case summary.PinOrder > 0:
		return summary.PinOrder
	default:
		return unrankedPinKey
	}
}

// pinnedRank is the cursor row's pinnedRankSQL value. Cursors minted before
// ranks existed omit it, which restarts the pinned block; that is safe because
// clients merge pages by session ID.
func (c ProjectSessionCursor) pinnedRank() int64 {
	if !c.Pinned {
		return 0
	}
	return c.PinOrder
}

// projectCursorClause continues a pinned-first activity listing strictly after
// the cursor row, comparing (pinned, rank, activity, number) lexicographically
// in the same order as listOrderBy.
func (s *SQLiteStore) projectCursorClause(cursor *ProjectSessionCursor, opts ListOptions) (string, []any, error) {
	if cursor == nil {
		return "", nil, nil
	}
	if !opts.SortByActivity || !s.hasPinned || !s.hasLastMessageAt || !s.hasLastUserMessageAt {
		return "", nil, fmt.Errorf("project cursor requires activity sorting on the current schema")
	}
	activityExpr := "COALESCE(s.last_message_at, s.last_user_message_at, s.created_at)"
	within := activityExpr + ` < ? OR (` + activityExpr + ` = ? AND s.number < ?)`
	args := []any{cursor.Pinned, cursor.Pinned}
	if s.hasPinOrder {
		rank := pinnedRankSQL("s")
		within = rank + ` > ? OR (` + rank + ` = ? AND (` + within + `))`
		args = append(args, cursor.pinnedRank(), cursor.pinnedRank())
	}
	args = append(args, cursor.ActivityAt, cursor.ActivityAt, cursor.Number)
	return ` AND (COALESCE(s.pinned, FALSE) < ? OR (COALESCE(s.pinned, FALSE) = ? AND (` + within + `)))`, args, nil
}

// listOrderBy returns List's ORDER BY clause.
func (s *SQLiteStore) listOrderBy(opts ListOptions) string {
	if opts.SortByNumberDesc {
		return " ORDER BY s.number DESC"
	}
	// Sort by last user message time (when the user last interacted), falling back
	// to created_at for sessions with no user messages yet. This prevents background
	// activity (autotitle, mining, status changes) from reordering the sidebar.
	// Web sidebar callers set SortByActivity to use last_message_at instead so
	// assistant-only turns also surface (keeps the top-N window aligned with the
	// client-side "any-message" ordering). Pinned sessions always lead in their
	// persisted order, which activity never changes.
	sortCol := "s.updated_at"
	if opts.SortByActivity && s.hasLastMessageAt {
		sortCol = "COALESCE(s.last_message_at, s.last_user_message_at, s.created_at)"
	} else if s.hasLastUserMessageAt {
		sortCol = "COALESCE(s.last_user_message_at, s.created_at)"
	}
	return " ORDER BY " + s.pinnedOrderPrefix("s") + sortCol + " DESC, s.number DESC"
}

// SetSessionPinned implements PinnedSessionStore.
func (s *SQLiteStore) SetSessionPinned(ctx context.Context, id string, pinned bool) (PinState, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return PinState{}, fmt.Errorf("session id is empty")
	}
	if !s.pinOrderWritable() {
		return PinState{}, ErrPinnedOrderUnsupported
	}
	var state PinState
	err := retryOnBusy(ctx, 5, func() error {
		var err error
		state, err = s.setSessionPinnedTx(ctx, id, pinned)
		return err
	})
	if err != nil {
		return PinState{}, fmt.Errorf("set session pinned: %w", err)
	}
	return state, nil
}

func (s *SQLiteStore) setSessionPinnedTx(ctx context.Context, id string, pinned bool) (PinState, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PinState{}, err
	}
	defer tx.Rollback()

	var current bool
	var rank sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(pinned, FALSE), pin_order FROM sessions WHERE id = ?`, id).Scan(&current, &rank)
	if errors.Is(err, sql.ErrNoRows) {
		return PinState{}, fmt.Errorf("session not found %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return PinState{}, err
	}
	state := PinState{Pinned: pinned, PinOrder: pinOrderValue(current, rank)}
	if current == pinned && (!pinned || state.PinOrder > 0) {
		// Idempotent: an existing pin keeps its rank.
		return state, nil
	}
	if pinned {
		// A new pin goes after every existing one. The same statement repairs a
		// pinned row that lacks a rank without touching its activity time.
		err = tx.QueryRowContext(ctx, `
			UPDATE sessions
			SET pin_order = (SELECT COALESCE(MAX(pin_order), 0) + 1 FROM sessions WHERE COALESCE(pinned, FALSE)),
			    updated_at = CASE WHEN COALESCE(pinned, FALSE) THEN updated_at ELSE ? END,
			    pinned = TRUE
			WHERE id = ?
			RETURNING pin_order`, time.Now(), id).Scan(&state.PinOrder)
	} else {
		state.PinOrder = 0
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET pinned = FALSE, pin_order = NULL, updated_at = ? WHERE id = ?`, time.Now(), id)
	}
	if err != nil {
		return PinState{}, err
	}
	if err := tx.Commit(); err != nil {
		return PinState{}, err
	}
	state.Changed = true
	return state, nil
}

// ReorderPinnedSessions implements PinnedSessionStore.
func (s *SQLiteStore) ReorderPinnedSessions(ctx context.Context, orderedIDs []string) (PinnedOrder, error) {
	ids, err := normalizeOrderIDs(orderedIDs, MaxPinnedOrderIDs, ErrPinnedOrderInvalid, "session")
	if err != nil {
		return PinnedOrder{}, err
	}
	if !s.pinOrderWritable() {
		return PinnedOrder{}, ErrPinnedOrderUnsupported
	}
	var order PinnedOrder
	err = retryOnBusy(ctx, 5, func() error {
		var err error
		order, err = s.reorderPinnedSessionsTx(ctx, ids)
		return err
	})
	if err != nil {
		return PinnedOrder{}, fmt.Errorf("reorder pinned sessions: %w", err)
	}
	return order, nil
}

func (s *SQLiteStore) reorderPinnedSessionsTx(ctx context.Context, ids []string) (PinnedOrder, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PinnedOrder{}, err
	}
	defer tx.Rollback()

	// Every pinned session in its displayed order.
	current, err := loadRankedRowsTx(ctx, tx, `
		SELECT id, pin_order FROM sessions
		WHERE COALESCE(pinned, FALSE)
		ORDER BY `+pinnedRankSQL("")+` ASC,
		         COALESCE(last_message_at, last_user_message_at, created_at) DESC,
		         number DESC, id`)
	if err != nil {
		return PinnedOrder{}, fmt.Errorf("load pinned sessions: %w", err)
	}
	if err := validatePinnedOrderTx(ctx, tx, current, ids); err != nil {
		return PinnedOrder{}, err
	}
	positions, changed, err := writeOrderTx(ctx, tx, `UPDATE sessions SET pin_order = ? WHERE id = ?`, current, permuteOrder(current, ids))
	if err != nil {
		return PinnedOrder{}, err
	}
	if err := tx.Commit(); err != nil {
		return PinnedOrder{}, err
	}
	order := PinnedOrder{Positions: make([]PinnedPosition, 0, len(positions)), Changed: changed}
	for _, row := range positions {
		order.Positions = append(order.Positions, PinnedPosition{ID: row.id, PinOrder: row.rank})
	}
	return order, nil
}

func validatePinnedOrderTx(ctx context.Context, tx *sql.Tx, current []rankedRow, ids []string) error {
	pinned := make(map[string]struct{}, len(current))
	for _, row := range current {
		pinned[row.id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := pinned[id]; ok {
			continue
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&exists); err != nil {
			return fmt.Errorf("inspect session %s: %w", id, err)
		}
		if exists == 0 {
			return fmt.Errorf("session not found %s: %w", id, ErrNotFound)
		}
		return fmt.Errorf("%w: session %s is not pinned", ErrPinnedOrderConflict, id)
	}
	return nil
}
