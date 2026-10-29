package session

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/samsaffron/term-llm/internal/sqlitefts"
)

// Search finds sessions containing the query text using FTS5.
func (s *SQLiteStore) Search(ctx context.Context, opts SearchOptions) ([]SearchResult, error) {
	if opts.Limit == 0 {
		opts.Limit = 20
	}

	ftsQuery := sqlitefts.LiteralQuery(opts.Query)
	if ftsQuery == "" {
		return []SearchResult{}, nil
	}

	messageCountCol := s.conversationMessageCountSelectSQL("s")
	compactionTailClause := ""
	if s.hasMessageCompactionTail {
		compactionTailClause = " AND COALESCE(m.compaction_tail, FALSE) = FALSE"
	}

	originCol := "'tui'"
	if s.hasOrigin {
		originCol = "COALESCE(NULLIF(TRIM(s.origin), ''), 'tui')"
	}
	generatedShortCol, generatedLongCol, titleSourceCol := "''", "''", "''"
	if s.hasGeneratedTitles {
		generatedShortCol = "COALESCE(s.generated_short_title, '')"
		generatedLongCol = "COALESCE(s.generated_long_title, '')"
		titleSourceCol = "COALESCE(s.title_source, '')"
	}
	lastMessageAtCol := "NULL"
	if s.hasLastMessageAt {
		lastMessageAtCol = "s.last_message_at"
	}

	orderBy, err := searchOrderBy(opts.Sort, lastMessageAtCol)
	if err != nil {
		return nil, err
	}
	projectIDCol := "''"
	projectNameCol := "''"
	projectJoin := ""
	if s.hasProjectID {
		projectIDCol = "COALESCE(s.project_id, '')"
		if s.hasProjectsTable {
			projectNameCol = "COALESCE(p.name, '')"
			projectJoin = " LEFT JOIN projects p ON p.id = s.project_id"
		}
	}

	filterClause := ""
	args := []any{ftsQuery}
	categoryClause, categoryArgs := s.sessionCategoryFilter(opts.Categories)
	filterClause += categoryClause
	args = append(args, categoryArgs...)
	if !opts.Archived {
		filterClause += " AND s.archived = FALSE"
	}
	if opts.ExcludeSubagents {
		filterClause += " AND s.parent_id IS NULL"
	}
	if opts.ProjectID != "" {
		if !s.hasProjectID {
			return []SearchResult{}, nil
		}
		filterClause += " AND s.project_id = ?"
		args = append(args, opts.ProjectID)
	}
	args = append(args, opts.Limit)

	rows, err := s.queryDB().QueryContext(ctx, `
		WITH raw_matches AS (
			SELECT rowid AS message_id, rank AS match_rank
			FROM messages_fts
			WHERE messages_fts MATCH ?
		), ranked_matches AS (
			SELECT m.id AS message_id, m.session_id, raw_matches.match_rank,
			       ROW_NUMBER() OVER (PARTITION BY m.session_id ORDER BY raw_matches.match_rank, m.id) AS session_row
			FROM raw_matches
			JOIN messages m ON m.id = raw_matches.message_id
			JOIN sessions s ON s.id = m.session_id
			WHERE 1=1`+compactionTailClause+filterClause+`
		), session_matches AS (
			SELECT sm.message_id, sm.session_id, sm.match_rank
			FROM ranked_matches sm
			JOIN sessions s ON s.id = sm.session_id
			WHERE session_row = 1
			ORDER BY `+orderBy+`
			LIMIT ?
		)
		SELECT m.session_id, s.number, m.id, s.name, s.summary, `+generatedShortCol+` AS generated_short_title,
		       `+generatedLongCol+` AS generated_long_title, `+titleSourceCol+` AS title_source,
		       snippet(messages_fts, 0, '**', '**', '...', 32) AS snippet, s.provider, COALESCE(s.provider_key, '') AS provider_key,
		       s.model, COALESCE(s.mode, ''), `+originCol+` AS origin, s.archived, `+s.pinnedSelectCols("s")+`, COALESCE(s.status, ''),
		       `+messageCountCol+` AS message_count, `+projectIDCol+` AS project_id, `+projectNameCol+` AS project_name,
		       s.created_at, s.updated_at, `+lastMessageAtCol+` AS last_message_at,
		       m.created_at AS message_created_at
		FROM session_matches sm
		JOIN messages m ON m.id = sm.message_id
		JOIN sessions s ON s.id = sm.session_id`+projectJoin+`
		JOIN messages_fts ON messages_fts.rowid = sm.message_id
		ORDER BY `+orderBy, args...)
	if err != nil {
		return nil, fmt.Errorf("search messages: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var number, pinOrder sql.NullInt64
		var lastMessageAt sql.NullTime
		err := rows.Scan(&r.SessionID, &number, &r.MessageID, &r.SessionName, &r.Summary,
			&r.GeneratedShortTitle, &r.GeneratedLongTitle, &r.TitleSource, &r.Snippet, &r.Provider, &r.ProviderKey,
			&r.Model, &r.Mode, &r.Origin, &r.Archived, &r.Pinned, &pinOrder, &r.Status, &r.MessageCount, &r.ProjectID, &r.ProjectName,
			&r.SessionCreatedAt, &r.UpdatedAt, &lastMessageAt, &r.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		r.PinOrder = pinOrderValue(r.Pinned, pinOrder)
		r.SessionNumber = number.Int64
		r.LastMessageAt = lastMessageAt.Time
		results = append(results, r)
	}
	return results, rows.Err()
}

// searchOrderBy orders distinct sessions without changing their best match/snippet.
func searchOrderBy(order SearchSort, lastMessageAtCol string) (string, error) {
	switch order {
	case "", SearchSortRelevance:
		return "sm.match_rank, sm.message_id", nil
	case SearchSortNewest, SearchSortOldest:
		direction := " DESC"
		if order == SearchSortOldest {
			direction = " ASC"
		}
		return "COALESCE(" + lastMessageAtCol + ", s.created_at)" + direction + ", s.id ASC", nil
	default:
		return "", fmt.Errorf("invalid search sort: %q", order)
	}
}
