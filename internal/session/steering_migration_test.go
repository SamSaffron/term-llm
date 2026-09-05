package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSteeringMigrationPreservesLegacyPayloadOrderAndCascade(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA foreign_keys=ON;" + schema + projectsSchemaV47 + changeLogSchemaV52 + attentionSchemaV54); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE session_pending_steering; DROP TABLE session_steering_sequence;
 CREATE TABLE schema_version(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL);
 INSERT INTO schema_version VALUES(1,55);
 INSERT INTO sessions(id,provider,model) VALUES('legacy','test','model');`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version == 50 {
			if err = migration.up(db); err != nil {
				t.Fatal(err)
			}
		}
	}
	message := llm.UserText("keep full context")
	message.Parts = append(message.Parts, llm.Part{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "original-bytes"}})
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"b", "a"} {
		if _, err = db.Exec(`INSERT INTO session_pending_interjections(session_id,id,message,display_text,attachment_summary,created_at) VALUES('legacy',?,?,?,'image','2026-01-01 00:00:00')`, id, string(payload), "visible guidance"); err != nil {
			t.Fatal(err)
		}
	}
	if err = initSchema(db); err != nil {
		t.Fatal(err)
	}
	store := &SQLiteStore{db: db}
	pending, err := store.ListPendingSteering(context.Background(), "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].ID != "a" || pending[1].ID != "b" || pending[0].AcceptanceSequence != 1 || pending[1].AcceptanceSequence != 2 {
		t.Fatalf("historical order: %+v", pending)
	}
	for _, entry := range pending {
		encoded, _ := json.Marshal(entry.Message)
		if string(encoded) != string(payload) || entry.Origin != llm.SteeringOriginLegacy || entry.DisplayText != "visible guidance" {
			t.Fatalf("payload changed: %+v", entry)
		}
	}
	if _, err = db.Exec(`DELETE FROM sessions WHERE id='legacy'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM session_pending_steering`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade count=%d err=%v", count, err)
	}
}
