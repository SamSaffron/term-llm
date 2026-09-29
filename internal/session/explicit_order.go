package session

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// The helpers below implement the explicit, user-arranged orders shared by
// pinned conversations and projects: each entry stores a dense 1-based rank,
// and a reorder moves the listed entries, in order, into the positions they
// already occupy so entries a caller did not list keep theirs. An entry that
// lacks a rank sorts after every ranked one (unrankedPinKey/unrankedPinKeySQL).

// rankedRow is one entry of an explicit order with its persisted rank, or 0
// when it has none.
type rankedRow struct {
	id   string
	rank int64
}

// rankValue exposes a persisted rank, treating NULL or non-positive values as
// no rank.
func rankValue(raw sql.NullInt64) int64 {
	if !raw.Valid || raw.Int64 <= 0 {
		return 0
	}
	return raw.Int64
}

// normalizeOrderIDs trims a reorder request's IDs. A request that is empty,
// names more than limit entries, or names an entry twice is rejected with
// invalid; noun names the entries in the error.
func normalizeOrderIDs(orderedIDs []string, limit int, invalid error, noun string) ([]string, error) {
	if len(orderedIDs) == 0 {
		return nil, fmt.Errorf("%w: no %ss listed", invalid, noun)
	}
	if len(orderedIDs) > limit {
		return nil, fmt.Errorf("%w: at most %d %ss may be ordered at once", invalid, limit, noun)
	}
	ids := make([]string, 0, len(orderedIDs))
	seen := make(map[string]struct{}, len(orderedIDs))
	for _, raw := range orderedIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("%w: %s id is empty", invalid, noun)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: %s %s is listed more than once", invalid, noun, id)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// loadRankedRowsTx reads an explicit order: query selects each entry's ID and
// persisted rank, already sorted in display order.
func loadRankedRowsTx(ctx context.Context, tx *sql.Tx, query string) ([]rankedRow, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ranked []rankedRow
	for rows.Next() {
		var row rankedRow
		var rank sql.NullInt64
		if err := rows.Scan(&row.id, &rank); err != nil {
			return nil, err
		}
		row.rank = rankValue(rank)
		ranked = append(ranked, row)
	}
	return ranked, rows.Err()
}

// permuteOrder places ids, in order, into the positions they currently occupy
// within current. Every other entry keeps its position.
func permuteOrder(current []rankedRow, ids []string) []string {
	position := make(map[string]int, len(current))
	next := make([]string, len(current))
	for i, row := range current {
		position[row.id] = i
		next[i] = row.id
	}
	slots := make([]int, 0, len(ids))
	for _, id := range ids {
		slots = append(slots, position[id])
	}
	sort.Ints(slots)
	for i, id := range ids {
		next[slots[i]] = id
	}
	return next
}

// writeOrderTx stores dense 1-based ranks for next through update, a statement
// that binds the rank and then the ID. It writes only rows whose rank changes,
// so an unchanged order writes nothing, and returns every entry's committed
// rank in order along with the IDs whose rank changed.
func writeOrderTx(ctx context.Context, tx *sql.Tx, update string, current []rankedRow, next []string) ([]rankedRow, []string, error) {
	existing := make(map[string]int64, len(current))
	for _, row := range current {
		existing[row.id] = row.rank
	}
	stmt, err := tx.PrepareContext(ctx, update)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare rank update: %w", err)
	}
	defer stmt.Close()
	positions := make([]rankedRow, 0, len(next))
	var changed []string
	for i, id := range next {
		rank := int64(i + 1)
		positions = append(positions, rankedRow{id: id, rank: rank})
		if existing[id] == rank {
			continue
		}
		if _, err := stmt.ExecContext(ctx, rank, id); err != nil {
			return nil, nil, fmt.Errorf("store rank for %s: %w", id, err)
		}
		changed = append(changed, id)
	}
	return positions, changed, nil
}
