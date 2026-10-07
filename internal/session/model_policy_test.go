package session

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
)

func TestSessionModelPolicyPersistsAndBranches(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	source := &Session{ID: NewID(), Provider: "debug", Model: "allowed", ModelPolicy: modelpolicy.Policy{}.With("boss", []string{"debug:allowed"})}
	if err := store.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, source.ID)
	if err != nil || !got.ModelPolicy.Restricted() {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	anchor := addBranchTestMessage(t, store, source.ID, llm.UserText("branch me"))
	branch, branchErr := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: anchor.ID})
	if branchErr != nil {
		t.Fatal(branchErr)
	}
	if branch.Session == nil || len(branch.Session.ModelPolicy.Rules) != 1 {
		t.Fatalf("branch policy = %+v", branch.Session)
	}
	got.ModelPolicy = got.ModelPolicy.With("supervisor", []string{"debug:*"})
	if err := store.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, source.ID)
	if err != nil || len(got.ModelPolicy.Rules) != 2 {
		t.Fatalf("Update/Get = %+v, %v", got, err)
	}
	if _, err := store.db.Exec(`UPDATE sessions SET model_policy = NULL WHERE id = ?`, source.ID); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, source.ID)
	if err != nil || got.ModelPolicy.Restricted() {
		t.Fatalf("legacy unrestricted = %+v, %v", got, err)
	}
}

func TestSessionModelPolicyMigrationV64(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	legacy := schema + projectsSchemaV47 + changeLogSchemaV52 + attentionSchemaV54 + rushSchemaV57 + modelUsageSchemaV58 + agentRunSchemaV62
	// Synthetic v63 schema fixture: omit the newly introduced column.
	legacy = strings.Replace(legacy, "	model_policy TEXT,\n", "", 1)
	if _, err := db.Exec(legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (id INTEGER PRIMARY KEY CHECK (id=1),version INTEGER NOT NULL); INSERT INTO schema_version(id,version) VALUES(1,63); INSERT INTO sessions(id,provider,model) VALUES('old','debug','allowed')`); err != nil {
		t.Fatal(err)
	}
	if err := initSchema(db); err != nil {
		t.Fatal(err)
	}
	var version int
	var stored sql.NullString
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT model_policy FROM sessions WHERE id='old'`).Scan(&stored); err != nil || stored.Valid || version != schemaVersion {
		t.Fatalf("version=%d stored=%v err=%v", version, stored, err)
	}
}

// A failed policy copy must roll back the INSERT, not leave an unrestricted child.
func TestBranchModelPolicyCopyFailureRollsBackChild(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	source := &Session{ID: NewID(), Provider: "debug", Model: "main", ModelPolicy: modelpolicy.Policy{}.With("boss", []string{"debug:main"})}
	if err := store.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	anchor := addBranchTestMessage(t, store, source.ID, llm.UserText("anchor"))
	if _, err := store.db.Exec(`CREATE TRIGGER reject_branch_policy BEFORE UPDATE OF model_policy ON sessions
 WHEN NEW.id <> '` + source.ID + `' BEGIN SELECT RAISE(ABORT, 'policy copy failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: anchor.ID}); err == nil {
		t.Fatal("branch succeeded without copying policy")
	}
	var children int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id <> ?`, source.ID).Scan(&children); err != nil {
		t.Fatal(err)
	}
	if children != 0 {
		t.Fatalf("failed policy copy left %d unrestricted branches", children)
	}
}
