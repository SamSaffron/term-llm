package sqliteutil

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	schemaWhitespace       = regexp.MustCompile(`\s+`)
	schemaQuotedIdentifier = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"`)
	schemaIfNotExists      = regexp.MustCompile(`(?i)\s+IF\s+NOT\s+EXISTS`)
	schemaPunctuationSpace = regexp.MustCompile(`\s*([(),])\s*`)
)

func normalizeSchemaSQL(statement string) string {
	statement = schemaQuotedIdentifier.ReplaceAllString(statement, `$1`)
	statement = schemaIfNotExists.ReplaceAllString(statement, "")
	statement = schemaWhitespace.ReplaceAllString(statement, " ")
	statement = schemaPunctuationSpace.ReplaceAllString(statement, `$1`)
	statement = strings.TrimSpace(statement)
	return canonicalizeCreateTableColumns(statement)
}

func canonicalizeCreateTableColumns(statement string) string {
	// Column order is deliberately normalized: migration equivalence is about
	// named structure and constraints, not physical ALTER-append order. Callers
	// that depend on SELECT * ordering must test that behavior separately.
	upper := strings.ToUpper(statement)
	if !strings.HasPrefix(upper, "CREATE TABLE ") || strings.HasPrefix(upper, "CREATE TABLE SQLITE_") {
		return statement
	}
	open := strings.IndexByte(statement, '(')
	if open < 0 {
		return statement
	}
	depth := 0
	close := -1
	inString := false
	for i := open; i < len(statement); i++ {
		if statement[i] == '\'' {
			if inString && i+1 < len(statement) && statement[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch statement[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				close = i
				i = len(statement)
			}
		}
	}
	if close < 0 {
		return statement
	}
	body := statement[open+1 : close]
	depth = 0
	inString = false
	start := 0
	var definitions []string
	for i := 0; i < len(body); i++ {
		if body[i] == '\'' {
			if inString && i+1 < len(body) && body[i+1] == '\'' {
				i++
				continue
			}
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				definitions = append(definitions, body[start:i])
				start = i + 1
			}
		}
	}
	definitions = append(definitions, body[start:])
	sort.Strings(definitions)
	return statement[:open+1] + strings.Join(definitions, ",") + statement[close:]
}

// SchemaSignature captures a normalized structural signature for tests and
// diagnostics. It includes sqlite_master SQL, complete table_info metadata, and
// foreign-key metadata while excluding SQLite's auto-generated object names.
// It is intentionally not used by normal migration startup paths.
func SchemaSignature(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT type, name, tbl_name, sql
		FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_autoindex_%'
		ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("read sqlite_master: %w", err)
	}
	defer rows.Close()

	var signature []string
	var tables []string
	for rows.Next() {
		var objectType, name, tableName, statement string
		if err := rows.Scan(&objectType, &name, &tableName, &statement); err != nil {
			return nil, fmt.Errorf("scan sqlite_master: %w", err)
		}
		normalized := normalizeSchemaSQL(statement)
		signature = append(signature, fmt.Sprintf("master|%s|%s|%s|%s", objectType, name, tableName, normalized))
		if objectType == "table" && !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sqlite_master: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close sqlite_master rows: %w", err)
	}

	for _, table := range tables {
		quoted, err := quoteIdentifier(table)
		if err != nil {
			return nil, err
		}
		columnRows, err := db.QueryContext(ctx, `PRAGMA table_info(`+quoted+`)`)
		if err != nil {
			return nil, fmt.Errorf("read table_info for %s: %w", table, err)
		}
		for columnRows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue sql.NullString
			if err := columnRows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				columnRows.Close()
				return nil, fmt.Errorf("scan table_info for %s: %w", table, err)
			}
			signature = append(signature, fmt.Sprintf("column|%s|%s|%s|%d|%t:%s|%d", table, name, columnType, notNull, defaultValue.Valid, defaultValue.String, primaryKey))
		}
		if err := columnRows.Close(); err != nil {
			return nil, fmt.Errorf("close table_info for %s: %w", table, err)
		}

		foreignKeyRows, err := db.QueryContext(ctx, `PRAGMA foreign_key_list(`+quoted+`)`)
		if err != nil {
			return nil, fmt.Errorf("read foreign_key_list for %s: %w", table, err)
		}
		for foreignKeyRows.Next() {
			var id, sequence int
			var targetTable, from, to, onUpdate, onDelete, match string
			if err := foreignKeyRows.Scan(&id, &sequence, &targetTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				foreignKeyRows.Close()
				return nil, fmt.Errorf("scan foreign_key_list for %s: %w", table, err)
			}
			signature = append(signature, fmt.Sprintf("foreign-key|%s|%d|%d|%s|%s|%s|%s|%s|%s", table, id, sequence, targetTable, from, to, onUpdate, onDelete, match))
		}
		if err := foreignKeyRows.Close(); err != nil {
			return nil, fmt.Errorf("close foreign_key_list for %s: %w", table, err)
		}
	}
	sort.Strings(signature)
	return signature, nil
}
