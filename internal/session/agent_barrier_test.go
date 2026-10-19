package session

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

func openAgentBarrierV65DB(t *testing.T) *sql.DB {
	t.Helper()
	// Faithful v65 fixture: v64 schema plus the released v65 migration.
	db := openAttentionV64DB(t)
	for _, m := range migrations {
		if m.version == 65 {
			if err := runSessionMigration(db, m); err != nil {
				t.Fatal(err)
			}
			return db
		}
	}
	t.Fatal("missing migration 65")
	return nil
}

func TestAgentBarrierMigrationV66BackfillAndSchemaEquivalence(t *testing.T) {
	db := openAgentBarrierV65DB(t)
	for _, r := range []struct {
		id, status, reason string
		notify             bool
		acknowledged       bool
	}{
		{"historical", "completed", "completed", false, true},
		{"interrupted", "interrupted", "interrupted", false, true},
		{"restart", "interrupted", "host_restarted", false, false},
		{"eager", "completed", "completed", true, false},
		{"running", "running", "", false, false},
		{"queued", "queued", "", false, false},
		{"approval", "awaiting_approval", "", false, false},
	} {
		if _, err := db.Exec(`INSERT INTO session_agent_runs(child_session_id,parent_session_id,agent_name,prompt,run_status,stop_reason,notify_when_done,owner_instance_id,updated_at) VALUES(?,'old','developer','work',?,?,?,'old-owner',CURRENT_TIMESTAMP)`, r.id, r.status, r.reason, r.notify); err != nil {
			t.Fatal(err)
		}
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"historical", "interrupted", "restart", "eager", "running", "queued", "approval"} {
		r, err := scanAgentRun(db.QueryRow(`SELECT `+agentRunColumns+` FROM session_agent_runs WHERE child_session_id=?`, id))
		wantAck := id == "historical" || id == "interrupted"
		if err != nil || r.NotifiedAt.IsZero() == wantAck {
			t.Fatalf("%s backfill=%+v err=%v", id, r, err)
		}
		if id == "running" && r.Status != "running" {
			t.Fatalf("nonterminal rewritten: %+v", r)
		}
		if id == "restart" && r.StopReason != "host_restarted" {
			t.Fatalf("restart provenance lost: %+v", r)
		}
	}
	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	want, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	got, err := sqliteutil.SchemaSignature(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh/upgraded schemas differ\nfresh=%v\nupgraded=%v", want, got)
	}
}

func TestAgentBarrierMigrationV66RollbackAndRetry(t *testing.T) {
	db := openAgentBarrierV65DB(t)
	if _, err := db.Exec(`INSERT INTO session_agent_runs(child_session_id,parent_session_id,agent_name,prompt,run_status,owner_instance_id,updated_at) VALUES('historical','old','developer','work','completed','old-owner',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	var m migration
	for _, candidate := range migrations {
		if candidate.version == 66 {
			m = candidate
		}
	}
	failing := m
	failing.up = func(db schemaExecutor) error {
		if err := m.up(db); err != nil {
			return err
		}
		return errors.New("injected barrier backfill failure")
	}
	if err := runSessionMigration(db, failing); err == nil {
		t.Fatal("injected failure succeeded")
	}
	var marker int
	var notified sql.NullTime
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&marker); err != nil || marker != 65 {
		t.Fatalf("marker=%d %v", marker, err)
	}
	if err := db.QueryRow(`SELECT notified_at FROM session_agent_runs WHERE child_session_id='historical'`).Scan(&notified); err != nil || notified.Valid {
		t.Fatalf("backfill leaked after rollback: %+v %v", notified, err)
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT notified_at FROM session_agent_runs WHERE child_session_id='historical'`).Scan(&notified); err != nil || !notified.Valid {
		t.Fatalf("retry lost backfill: %+v %v", notified, err)
	}
}

func TestAgentBarrierAdmissionFencesOutstandingAcrossOrigins(t *testing.T) {
	for _, origin := range []SessionOrigin{OriginWeb, OriginTUI} {
		for _, status := range []string{"completed", "queued", "running", "awaiting_approval"} {
			t.Run(string(origin)+"/"+status, func(t *testing.T) {
				store, err := NewSQLiteStore(Config{Enabled: true, Path: t.TempDir() + "/s.db"})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				ctx := context.Background()
				if err := store.Create(ctx, &Session{ID: "parent", Origin: origin, Status: StatusActive}); err != nil {
					t.Fatal(err)
				}
				event := AgentRun{ID: "event", ParentSessionID: "parent", Status: "completed", RunGeneration: 1, UpdatedAt: time.Now()}
				if err := store.PutAgentRun(ctx, event); err != nil {
					t.Fatal(err)
				}
				sibling := event
				sibling.ID, sibling.Status, sibling.CollectedAt, sibling.WakeSuppressed = "sibling", status, time.Now(), true
				if err := store.PutAgentRun(ctx, sibling); err != nil {
					t.Fatal(err)
				}
				_, err = store.AdmitResponseRun(ctx, ResponseRunAdmission{ResponseID: "wake", SessionID: "parent", OwnerInstanceID: "host", RunEpoch: 1, AgentEvents: []AgentRun{event}, AgentBarrier: true})
				if status == "completed" {
					if err != nil {
						t.Fatalf("origin-independent barrier refused: %v", err)
					}
				} else if !errors.Is(err, ErrSessionTurnOwned) {
					t.Fatalf("outstanding sibling admitted: %v", err)
				}
			})
		}
	}
}
