package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// newModelUsageStore opens a real SQLite store on a temp database: additive
// upserts, ordering and the ON DELETE CASCADE are all SQLite behaviour.
func newModelUsageStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createModelUsageSession(t *testing.T, store *SQLiteStore, id string) {
	t.Helper()
	sess := &Session{ID: id, Provider: "test", Model: "opus", Mode: ModeChat}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
}

func modelUsageRow(t *testing.T, rows []ModelUsage, model string, kind ModelUsageKind) ModelUsage {
	t.Helper()
	for _, row := range rows {
		if row.Model == model && row.Kind == kind {
			return row
		}
	}
	t.Fatalf("no %q/%q row in %+v", model, kind, rows)
	return ModelUsage{}
}

// countModelUsageRows distinguishes "no row written" from "row written with
// zero counters" for the no-op cases, where a query by session id cannot.
func countModelUsageRows(t *testing.T, store *SQLiteStore) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM session_model_usage`).Scan(&count); err != nil {
		t.Fatalf("count session_model_usage: %v", err)
	}
	return count
}

func TestRecordModelUsageAccumulatesInsteadOfReplacing(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	createModelUsageSession(t, store, "accumulate")

	entry := ModelUsage{
		Model: "claude-bin:opus-max", Kind: ModelUsageMain,
		InputTokens: 10, OutputTokens: 4, CachedInputTokens: 30, CacheWriteTokens: 7,
		LLMTurns: 2, ToolCalls: 3,
	}
	for range 2 {
		if err := store.RecordModelUsage(ctx, "accumulate", entry); err != nil {
			t.Fatalf("RecordModelUsage: %v", err)
		}
	}

	rows, err := store.ListModelUsage(ctx, "accumulate")
	if err != nil {
		t.Fatalf("ListModelUsage: %v", err)
	}
	want := ModelUsage{
		Model: "claude-bin:opus-max", Kind: ModelUsageMain,
		InputTokens: 20, OutputTokens: 8, CachedInputTokens: 60, CacheWriteTokens: 14,
		LLMTurns: 4, ToolCalls: 6,
	}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("rows = %+v, want one accumulated row %+v", rows, want)
	}
}

// Runtime timing dies with the runtime, so the durable rows are the only place
// a resumed session can learn how long its own work took.
func TestRecordModelUsageAccumulatesTiming(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	createModelUsageSession(t, store, "timing")

	for _, entry := range []ModelUsage{
		{Model: "opus", Kind: ModelUsageMain, InputTokens: 10, OutputTokens: 2, LLMMS: 1_500, ToolMS: 400},
		{Model: "opus", Kind: ModelUsageMain, InputTokens: 5, OutputTokens: 1, LLMMS: 2_500, ToolMS: 600},
		// Negative clocks are clamped rather than subtracted from the session.
		{Model: "opus", Kind: ModelUsageMain, InputTokens: 1, OutputTokens: 1, LLMMS: -9_000, ToolMS: -1},
	} {
		if err := store.RecordModelUsage(ctx, "timing", entry); err != nil {
			t.Fatalf("RecordModelUsage: %v", err)
		}
	}

	rows, err := store.ListModelUsage(ctx, "timing")
	if err != nil {
		t.Fatalf("ListModelUsage: %v", err)
	}
	row := modelUsageRow(t, rows, "opus", ModelUsageMain)
	if row.LLMMS != 4_000 || row.ToolMS != 1_000 {
		t.Fatalf("timing = %dms model / %dms tools, want 4000/1000", row.LLMMS, row.ToolMS)
	}
}

func TestRecordModelUsageKeepsOneRowPerKindForOneModel(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	createModelUsageSession(t, store, "kinds")

	for _, entry := range []ModelUsage{
		{Model: "opus", Kind: ModelUsageMain, InputTokens: 100, OutputTokens: 10, LLMTurns: 1, ToolCalls: 2},
		{Model: "opus", Kind: ModelUsageGuardian, InputTokens: 7, OutputTokens: 3, LLMTurns: 1},
	} {
		if err := store.RecordModelUsage(ctx, "kinds", entry); err != nil {
			t.Fatalf("RecordModelUsage(%s): %v", entry.Kind, err)
		}
	}

	rows, err := store.ListModelUsage(ctx, "kinds")
	if err != nil {
		t.Fatalf("ListModelUsage: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two kinds kept apart", rows)
	}
	main := modelUsageRow(t, rows, "opus", ModelUsageMain)
	if main.InputTokens != 100 || main.OutputTokens != 10 || main.LLMTurns != 1 || main.ToolCalls != 2 {
		t.Fatalf("main row = %+v", main)
	}
	guardian := modelUsageRow(t, rows, "opus", ModelUsageGuardian)
	if guardian.InputTokens != 7 || guardian.OutputTokens != 3 || guardian.CachedInputTokens != 0 || guardian.CacheWriteTokens != 0 {
		t.Fatalf("guardian row = %+v", guardian)
	}
}

func TestListModelUsageOrdersByTotalTokensDescending(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	createModelUsageSession(t, store, "ordering")

	// Two deliberate ties: alpha/zeta share a total and are ordered by model
	// name; scope's two kinds share a total and are ordered by kind name.
	for _, entry := range []ModelUsage{
		{Model: "solo", Kind: ModelUsageGuardian, InputTokens: 25},
		{Model: "zeta", Kind: ModelUsageMain, InputTokens: 40},
		{Model: "opus", Kind: ModelUsageMain, CachedInputTokens: 900},
		{Model: "alpha", Kind: ModelUsageMain, OutputTokens: 40},
		{Model: "solo", Kind: ModelUsageMain, OutputTokens: 25},
	} {
		if err := store.RecordModelUsage(ctx, "ordering", entry); err != nil {
			t.Fatalf("RecordModelUsage(%s/%s): %v", entry.Model, entry.Kind, err)
		}
	}

	rows, err := store.ListModelUsage(ctx, "ordering")
	if err != nil {
		t.Fatalf("ListModelUsage: %v", err)
	}
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Model+"/"+string(row.Kind))
	}
	want := []string{"opus/main", "alpha/main", "zeta/main", "solo/guardian", "solo/main"}
	if len(got) != len(want) {
		t.Fatalf("ordering = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ordering = %v, want %v", got, want)
		}
	}
}

func TestRecordModelUsageNormalizesEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   ModelUsage
		want ModelUsage
	}{
		{
			name: "blank model and kind fall back",
			in:   ModelUsage{InputTokens: 5},
			want: ModelUsage{Model: UnknownUsageModel, Kind: ModelUsageMain, InputTokens: 5},
		},
		{
			name: "whitespace model and kind fall back",
			in:   ModelUsage{Model: "   ", Kind: "  ", OutputTokens: 5},
			want: ModelUsage{Model: UnknownUsageModel, Kind: ModelUsageMain, OutputTokens: 5},
		},
		{
			name: "model is trimmed",
			in:   ModelUsage{Model: "  opus  ", InputTokens: 3},
			want: ModelUsage{Model: "opus", Kind: ModelUsageMain, InputTokens: 3},
		},
		{
			// Clamping happens before the write is judged, so an entry whose
			// only counters are negative records no work at all.
			name: "negative counters clamp to zero and write nothing",
			in: ModelUsage{
				Model: "opus", Kind: ModelUsageGuardian,
				InputTokens: -5, OutputTokens: -1, CachedInputTokens: -3, CacheWriteTokens: -2,
				LLMTurns: -4, ToolCalls: -9,
			},
		},
		{
			name: "partly negative usage keeps the billable counters",
			in:   ModelUsage{Model: "opus", Kind: ModelUsageCompaction, InputTokens: -3, OutputTokens: 8},
			want: ModelUsage{Model: "opus", Kind: ModelUsageCompaction, OutputTokens: 8},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newModelUsageStore(t)
			ctx := context.Background()
			createModelUsageSession(t, store, "normalize")

			if err := store.RecordModelUsage(ctx, "normalize", tc.in); err != nil {
				t.Fatalf("RecordModelUsage: %v", err)
			}
			rows, err := store.ListModelUsage(ctx, "normalize")
			if err != nil {
				t.Fatalf("ListModelUsage: %v", err)
			}
			if tc.want == (ModelUsage{}) {
				if len(rows) != 0 {
					t.Fatalf("rows = %+v, want none", rows)
				}
				return
			}
			if len(rows) != 1 || rows[0] != tc.want {
				t.Fatalf("rows = %+v, want one row %+v", rows, tc.want)
			}
		})
	}
}

func TestRecordModelUsageWritesNothingForEmptySessionOrZeroCounters(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	createModelUsageSession(t, store, "noop")

	for _, tc := range []struct {
		name      string
		sessionID string
		usage     ModelUsage
	}{
		{
			name:      "empty session id",
			sessionID: "",
			usage:     ModelUsage{Model: "opus", Kind: ModelUsageMain, InputTokens: 11},
		},
		{
			name:      "whitespace session id",
			sessionID: "   ",
			usage:     ModelUsage{Model: "opus", Kind: ModelUsageMain, InputTokens: 11},
		},
		{
			name:      "zero token counters with turns and tools",
			sessionID: "noop",
			usage:     ModelUsage{Model: "opus", Kind: ModelUsageMain, LLMTurns: 5, ToolCalls: 9},
		},
		{
			name:      "no counters and no clock",
			sessionID: "noop",
			usage:     ModelUsage{Model: "opus", Kind: ModelUsageMain},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.RecordModelUsage(ctx, tc.sessionID, tc.usage); err != nil {
				t.Fatalf("RecordModelUsage: %v", err)
			}
			if rows, err := store.ListModelUsage(ctx, "noop"); err != nil || len(rows) != 0 {
				t.Fatalf("rows = %+v err = %v, want none", rows, err)
			}
			if count := countModelUsageRows(t, store); count != 0 {
				t.Fatalf("session_model_usage rows = %d, want 0", count)
			}
		})
	}

	// Elapsed time is evidence of work in its own right: a tool-only turn that
	// reported no tokens still belongs to the model that ran it.
	if err := store.RecordModelUsage(ctx, "noop", ModelUsage{Model: "haiku", Kind: ModelUsageMain, LLMTurns: 1, ToolCalls: 3, ToolMS: 7_000}); err != nil {
		t.Fatalf("RecordModelUsage: %v", err)
	}
	timed, err := store.ListModelUsage(ctx, "noop")
	if err != nil || len(timed) != 1 {
		t.Fatalf("rows = %+v err = %v, want the timed row recorded", timed, err)
	}
	if timed[0].ToolMS != 7_000 || timed[0].ToolCalls != 3 {
		t.Fatalf("timed row = %+v", timed[0])
	}

	// The same entries become attributable once they carry billable counters.
	if err := store.RecordModelUsage(ctx, "noop", ModelUsage{Model: "opus", Kind: ModelUsageMain, InputTokens: 11, LLMTurns: 5, ToolCalls: 9}); err != nil {
		t.Fatalf("RecordModelUsage: %v", err)
	}
	rows, err := store.ListModelUsage(ctx, "noop")
	if err != nil {
		t.Fatalf("ListModelUsage: %v", err)
	}
	billable := modelUsageRow(t, rows, "opus", ModelUsageMain)
	if billable.InputTokens != 11 || billable.LLMTurns != 5 || billable.ToolCalls != 9 {
		t.Fatalf("rows = %+v, want the billable entry recorded once", rows)
	}
}

// TestRecordModelUsageDecomposesSessionTotals is the invariant the feature
// rests on: the session row keeps one undifferentiated token bucket, and the
// per-model rows must account for exactly that bucket.
func TestRecordModelUsageDecomposesSessionTotals(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	createModelUsageSession(t, store, "decompose")

	// The session's own dominating turn plus one delegated run.
	own := ModelUsage{
		Model: "opus", Kind: ModelUsageMain,
		InputTokens: 5720, OutputTokens: 149063, CachedInputTokens: 35795091, CacheWriteTokens: 465243,
		LLMTurns: 1, ToolCalls: 12,
	}
	delegated := ModelUsage{
		Model: "gpt-5.6-sol-high", Kind: ModelUsageSubagent,
		InputTokens: 50868, OutputTokens: 12791, CachedInputTokens: 679040,
	}
	for _, entry := range []ModelUsage{own, delegated} {
		if err := store.UpdateMetrics(ctx, "decompose", entry.LLMTurns, entry.ToolCalls, entry.InputTokens, entry.OutputTokens, entry.CachedInputTokens, entry.CacheWriteTokens); err != nil {
			t.Fatalf("UpdateMetrics(%s): %v", entry.Model, err)
		}
		if err := store.RecordModelUsage(ctx, "decompose", entry); err != nil {
			t.Fatalf("RecordModelUsage(%s): %v", entry.Model, err)
		}
	}

	sess, err := store.Get(ctx, "decompose")
	if err != nil || sess == nil {
		t.Fatalf("Get: %+v err = %v", sess, err)
	}
	if sess.InputTokens != 56588 || sess.OutputTokens != 161854 || sess.CachedInputTokens != 36474131 || sess.CacheWriteTokens != 465243 {
		t.Fatalf("session bucket = %+v, want the two records summed", sess)
	}

	rows, err := store.ListModelUsage(ctx, "decompose")
	if err != nil {
		t.Fatalf("ListModelUsage: %v", err)
	}
	var summed ModelUsage
	for _, row := range rows {
		summed.InputTokens += row.InputTokens
		summed.OutputTokens += row.OutputTokens
		summed.CachedInputTokens += row.CachedInputTokens
		summed.CacheWriteTokens += row.CacheWriteTokens
		summed.LLMTurns += row.LLMTurns
		summed.ToolCalls += row.ToolCalls
	}
	if summed.InputTokens != sess.InputTokens || summed.OutputTokens != sess.OutputTokens ||
		summed.CachedInputTokens != sess.CachedInputTokens || summed.CacheWriteTokens != sess.CacheWriteTokens {
		t.Fatalf("per-model tokens %+v do not decompose the session bucket %+v (rows %+v)", summed, sess, rows)
	}
	if summed.LLMTurns != sess.LLMTurns || summed.ToolCalls != sess.ToolCalls {
		t.Fatalf("per-model turns %+v do not decompose the session totals %+v", summed, sess)
	}

	// The regression this feature fixes: the session's own model was absent
	// from a breakdown that only knew about child sessions.
	if got := modelUsageRow(t, rows, "opus", ModelUsageMain); got != own {
		t.Fatalf("own-model row = %+v, want %+v", got, own)
	}
	if len(rows) != 2 || rows[0].Model != "opus" {
		t.Fatalf("rows = %+v, want the dominant model first", rows)
	}
}

func TestRecordModelUsageDecomposesTotalsAcrossHelperKinds(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	createModelUsageSession(t, store, "helpers")

	entries := []ModelUsage{
		{Model: "opus", Kind: ModelUsageMain, InputTokens: 900, OutputTokens: 300, CachedInputTokens: 40000, CacheWriteTokens: 1200, LLMTurns: 3, ToolCalls: 4},
		{Model: "opus", Kind: ModelUsageGuardian, InputTokens: 40, OutputTokens: 12},
		{Model: "haiku", Kind: ModelUsageCompaction, InputTokens: 8000, OutputTokens: 600},
		{Model: "haiku", Kind: ModelUsageSideQuestion, InputTokens: 300, OutputTokens: 90},
		{Model: "gpt-5.6-sol-high", Kind: ModelUsageHandover, InputTokens: 2500, OutputTokens: 400},
		{Model: "gpt-5.6-sol-high", Kind: ModelUsageSubagent, InputTokens: 12000, OutputTokens: 900, CachedInputTokens: 5000},
	}
	for _, entry := range entries {
		if err := store.UpdateMetrics(ctx, "helpers", entry.LLMTurns, entry.ToolCalls, entry.InputTokens, entry.OutputTokens, entry.CachedInputTokens, entry.CacheWriteTokens); err != nil {
			t.Fatalf("UpdateMetrics(%s/%s): %v", entry.Model, entry.Kind, err)
		}
		if err := store.RecordModelUsage(ctx, "helpers", entry); err != nil {
			t.Fatalf("RecordModelUsage(%s/%s): %v", entry.Model, entry.Kind, err)
		}
	}

	sess, err := store.Get(ctx, "helpers")
	if err != nil || sess == nil {
		t.Fatalf("Get: %+v err = %v", sess, err)
	}
	rows, err := store.ListModelUsage(ctx, "helpers")
	if err != nil {
		t.Fatalf("ListModelUsage: %v", err)
	}
	// Every kind stays separable, so the aggregate is fully attributable.
	if len(rows) != len(entries) {
		t.Fatalf("rows = %+v, want one row per model and kind", rows)
	}
	var summed ModelUsage
	for _, row := range rows {
		summed.InputTokens += row.InputTokens
		summed.OutputTokens += row.OutputTokens
		summed.CachedInputTokens += row.CachedInputTokens
		summed.CacheWriteTokens += row.CacheWriteTokens
		summed.LLMTurns += row.LLMTurns
		summed.ToolCalls += row.ToolCalls
	}
	if summed.InputTokens != sess.InputTokens || summed.OutputTokens != sess.OutputTokens ||
		summed.CachedInputTokens != sess.CachedInputTokens || summed.CacheWriteTokens != sess.CacheWriteTokens ||
		summed.LLMTurns != sess.LLMTurns || summed.ToolCalls != sess.ToolCalls {
		t.Fatalf("per-model totals %+v do not decompose the session bucket %+v (rows %+v)", summed, sess, rows)
	}
}

func TestDeleteSessionCascadesModelUsage(t *testing.T) {
	store := newModelUsageStore(t)
	ctx := context.Background()
	for _, id := range []string{"keep", "drop"} {
		createModelUsageSession(t, store, id)
		if err := store.RecordModelUsage(ctx, id, ModelUsage{Model: "opus", Kind: ModelUsageMain, InputTokens: 5, OutputTokens: 2}); err != nil {
			t.Fatalf("RecordModelUsage(%s): %v", id, err)
		}
	}

	if err := store.Delete(ctx, "drop"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if rows, err := store.ListModelUsage(ctx, "drop"); err != nil || len(rows) != 0 {
		t.Fatalf("rows for deleted session = %+v err = %v, want none", rows, err)
	}
	// The store opens SQLite with foreign_keys(1), so the declared
	// ON DELETE CASCADE is enforced rather than merely recorded in the schema.
	var orphans int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM session_model_usage WHERE session_id = ?`, "drop").Scan(&orphans); err != nil {
		t.Fatalf("count orphaned rows: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("orphaned rows for deleted session = %d, want 0", orphans)
	}
	if rows, err := store.ListModelUsage(ctx, "keep"); err != nil || len(rows) != 1 {
		t.Fatalf("rows for surviving session = %+v err = %v, want one", rows, err)
	}
}

func TestMigration58AddsModelUsageAttribution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	ctx := context.Background()
	createModelUsageSession(t, store, "v57")
	if _, err := store.db.Exec(`DROP TABLE session_model_usage; UPDATE schema_version SET version = 57`); err != nil {
		store.Close()
		t.Fatalf("stage version 57 schema: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatalf("migrate version 57 store: %v", err)
	}
	defer store.Close()

	var version int
	if err := store.db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("schema version = %d err = %v, want %d", version, err, schemaVersion)
	}
	var table int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_model_usage'`).Scan(&table); err != nil || table != 1 {
		t.Fatalf("session_model_usage table count = %d err = %v, want 1", table, err)
	}
	var timingColumns int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('session_model_usage') WHERE name IN ('llm_ms','tool_ms')`).Scan(&timingColumns); err != nil || timingColumns != 2 {
		t.Fatalf("timing columns = %d err = %v, want 2", timingColumns, err)
	}
	// The session lookup rides the primary key's leading column; no separate
	// index should be carried.
	var plan string
	if err := store.db.QueryRow(`EXPLAIN QUERY PLAN SELECT model FROM session_model_usage WHERE session_id = 'v57'`).Scan(new(int), new(int), new(int), &plan); err != nil {
		t.Fatalf("explain session_model_usage lookup: %v", err)
	}
	if !strings.Contains(plan, "USING PRIMARY KEY") && !strings.Contains(plan, "sqlite_autoindex") {
		t.Fatalf("session lookup does not use the primary key: %s", plan)
	}

	// The migrated table must accept and return attribution for the session
	// that predates the migration.
	entry := ModelUsage{Model: "opus", Kind: ModelUsageMain, InputTokens: 7, OutputTokens: 3, LLMTurns: 1, ToolCalls: 2}
	if err := store.RecordModelUsage(ctx, "v57", entry); err != nil {
		t.Fatalf("RecordModelUsage after migration: %v", err)
	}
	rows, err := store.ListModelUsage(ctx, "v57")
	if err != nil || len(rows) != 1 || rows[0] != entry {
		t.Fatalf("rows = %+v err = %v, want %+v", rows, err, entry)
	}
}

// An intermediate build stamped version 58 without ever creating the table.
// Opening such a database must create it rather than fail every session.
func TestModelUsageTimingMigrationCreatesMissingTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(58)`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('session_model_usage') WHERE name IN ('llm_ms','tool_ms')`).Scan(&count); err != nil {
		t.Fatalf("inspect table: %v", err)
	}
	if count != 2 {
		t.Fatalf("timing columns = %d, want 2", count)
	}
}

// A database that created session_model_usage before timing existed must gain
// the columns without losing the rows it already attributed.
func TestModelUsageTimingMigrationUpgradesExistingTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	// The v58 table as it shipped, without the timing columns.
	if _, err := db.Exec(`
		CREATE TABLE session_model_usage (
		 session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
		 model TEXT NOT NULL,
		 kind TEXT NOT NULL,
		 input_tokens INTEGER NOT NULL DEFAULT 0,
		 output_tokens INTEGER NOT NULL DEFAULT 0,
		 cached_input_tokens INTEGER NOT NULL DEFAULT 0,
		 cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		 llm_turns INTEGER NOT NULL DEFAULT 0,
		 tool_calls INTEGER NOT NULL DEFAULT 0,
		 updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		 PRIMARY KEY(session_id, model, kind));
		INSERT INTO session_model_usage(session_id, model, kind, input_tokens, output_tokens)
		VALUES ('legacy', 'opus', 'main', 11, 22);
		CREATE TABLE schema_version(version INTEGER NOT NULL);
		INSERT INTO schema_version VALUES(58)`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	var input, output, llmMS, toolMS int
	if err := db.QueryRow(`SELECT input_tokens, output_tokens, llm_ms, tool_ms FROM session_model_usage`).
		Scan(&input, &output, &llmMS, &toolMS); err != nil {
		t.Fatalf("read upgraded row: %v", err)
	}
	if input != 11 || output != 22 || llmMS != 0 || toolMS != 0 {
		t.Fatalf("upgraded row = %d/%d tokens, %d/%d ms", input, output, llmMS, toolMS)
	}
}
