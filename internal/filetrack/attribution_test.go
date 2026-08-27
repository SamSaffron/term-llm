package filetrack

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestExplicitAttributionAndObservationSeparation(t *testing.T) {
	store := openTestStore(t, Options{MaxObservationRows: 2, MaxObservationSessionRows: 2})
	ctx := context.Background()
	if err := store.RecordRunStart(ctx, RunRecord{SessionID: "s", RunID: "r1", StartedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAttributedChange(ctx, ChangeRecord{SessionID: "s", RunID: "r1", ToolName: "shell", Path: "/work/a.txt", Before: []byte("a\n"), After: []byte("b\n"), Provenance: ProvenanceDeclaredTransform, ClaimKind: ClaimTransform, ClaimPattern: "/work/*.txt", ClaimCoverage: CoverageComplete, BaselineState: BaselinePreexistingDirty}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordFilesystemObservation(ctx, FilesystemObservation{SessionID: "s", RunID: "r1", Classification: ObservationUnclaimed, Root: "/work/noise", CreatedCount: 100, SampledPaths: []string{"/work/noise/one"}, CoverageStatus: CoverageTruncated}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRunComplete(ctx, RunRecord{SessionID: "s", RunID: "r1", CompletedAt: time.Unix(2, 0)}); err != nil {
		t.Fatal(err)
	}
	changes, err := store.ListRecentRunChanges(ctx, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Provenance != ProvenanceDeclaredTransform || changes[0].BaselineState != BaselinePreexistingDirty {
		t.Fatalf("changes = %+v", changes)
	}
	ids, err := store.RecentRunIDs(ctx, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	observations, err := store.ListRunObservations(ctx, "s", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].CreatedCount != 100 {
		t.Fatalf("observations = %+v", observations)
	}
	var observationBlobRefs int
	if err := store.observationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('filesystem_observations') WHERE name IN ('before_hash','after_hash')`).Scan(&observationBlobRefs); err != nil {
		t.Fatal(err)
	}
	if observationBlobRefs != 0 {
		t.Fatal("observation sidecar can reference attributed blobs")
	}
}

func TestLatestCompletedEmptyRunReturnsEmpty(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	if err := store.RecordRunStart(ctx, RunRecord{SessionID: "s", RunID: "r1", StartedAt: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAttributedChange(ctx, ChangeRecord{SessionID: "s", RunID: "r1", ToolName: "write_file", Path: "/work/a", BeforeMissing: true, After: []byte("x\n"), Provenance: ProvenanceDirect, ClaimCoverage: CoverageComplete, BaselineState: BaselineNormal}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRunStart(ctx, RunRecord{SessionID: "s", RunID: "r2", StartedAt: time.Unix(2, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRunComplete(ctx, RunRecord{SessionID: "s", RunID: "r2", CompletedAt: time.Unix(3, 0)}); err != nil {
		t.Fatal(err)
	}
	changes, err := store.ListRecentRunChanges(ctx, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("latest empty run fell back to old changes: %+v", changes)
	}
}

func TestObservationPressureCannotEvictAttributedBlobs(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "file_history.db"), Options{MaxObservationRows: 1, MaxObservationSessionRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	change, err := store.RecordAttributedChange(ctx, ChangeRecord{SessionID: "s", RunID: "r", ToolName: "write_file", Path: "/work/a", BeforeMissing: true, After: []byte("retained\n"), Provenance: ProvenanceDirect, ClaimCoverage: CoverageComplete, BaselineState: BaselineNormal})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if _, err := store.RecordFilesystemObservation(ctx, FilesystemObservation{SessionID: "s", RunID: "r", Classification: ObservationMaterialized, Root: "/tmp/materialized", CreatedCount: 1000, CoverageStatus: CoverageComplete}); err != nil {
			t.Fatal(err)
		}
	}
	if !store.blobExists(ctx, change.AfterHash) {
		t.Fatal("observation retention evicted attributed blob")
	}
	var count int
	if err := store.observationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM filesystem_observations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("observation count = %d, want independent cap 1", count)
	}
}

func TestRecordAttributedChangeRejectsUnclassifiedAndMaterialize(t *testing.T) {
	store := openTestStore(t, Options{})
	base := ChangeRecord{SessionID: "s", Path: "/work/a", Before: []byte("a"), After: []byte("b"), ClaimCoverage: CoverageComplete, BaselineState: BaselineUnknown}
	if _, err := store.RecordAttributedChange(context.Background(), base); err == nil {
		t.Fatal("missing provenance accepted")
	}
	base.Provenance = ProvenanceDeclaredGenerate
	base.ClaimKind = ClaimMaterialize
	base.ClaimPattern = "/work/a"
	if _, err := store.RecordAttributedChange(context.Background(), base); err == nil {
		t.Fatal("materialize claim entered attributed API")
	}
}
