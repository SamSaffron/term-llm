package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/filetrack"
	"github.com/samsaffron/term-llm/internal/llm"
)

func executeClaimedShell(t *testing.T, tool *ShellTool, sessionID, runID string, args ShellArgs) llm.ToolOutput {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	ctx := llm.ContextWithSessionID(context.Background(), sessionID)
	ctx = llm.ContextWithToolRunID(ctx, runID)
	ctx = llm.ContextWithCallID(ctx, "call-1")
	out, err := tool.Execute(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type legacyOnlyFileRecorder struct{ records []filetrack.ChangeRecord }

func (r *legacyOnlyFileRecorder) RecordChange(_ context.Context, rec filetrack.ChangeRecord) *llm.FileChange {
	r.records = append(r.records, rec)
	return &llm.FileChange{Path: rec.Path, Kind: filetrack.KindModify}
}
func (r *legacyOnlyFileRecorder) SessionPaths(context.Context, string) []string { return nil }
func (r *legacyOnlyFileRecorder) MaxFileBytes() int                             { return filetrack.DefaultMaxFileBytes }

func TestLegacyRecorderCannotTurnShellDetectionIntoAttribution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	recorder := &legacyOnlyFileRecorder{}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = recorder
	out := executeClaimedShell(t, tool, "session", "run", ShellArgs{Command: "printf 'after\\n' > value.txt", WorkingDir: dir, AffectedPaths: []string{"value.txt"}})
	if len(out.FileChanges) != 0 || len(recorder.records) != 0 {
		t.Fatalf("legacy recorder attributed shell detection: output=%+v records=%+v", out, recorder.records)
	}
	if len(out.OutputClaimDiagnostics) == 0 || out.OutputClaimDiagnostics[0].Reason != "claim_unconfirmed_tracker_error" {
		t.Fatalf("diagnostics = %+v", out.OutputClaimDiagnostics)
	}
}

func TestShellAttributionRequiresCompatibleClaim(t *testing.T) {
	dir := t.TempDir()
	store, err := filetrack.Open(filepath.Join(t.TempDir(), "file_history.db"), filetrack.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = filetrack.NewRecorder(store)

	path := filepath.Join(dir, "value.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out := executeClaimedShell(t, tool, "session", "run-unclaimed", ShellArgs{
		Command: "printf 'unclaimed\\n' > value.txt", WorkingDir: dir, AffectedPaths: []string{"value.txt"},
	})
	if len(out.FileChanges) != 0 || len(out.FilesystemObservations) != 1 || out.FilesystemObservations[0].Classification != filetrack.ObservationUnclaimed {
		t.Fatalf("unclaimed output = %+v", out)
	}

	out = executeClaimedShell(t, tool, "session", "run-transform", ShellArgs{
		Command: "printf 'claimed\\n' > value.txt", WorkingDir: dir,
		OutputClaims: []OutputClaim{{Path: "value.txt", Kind: filetrack.ClaimTransform}},
	})
	if len(out.FileChanges) != 1 || out.FileChanges[0].Provenance != filetrack.ProvenanceDeclaredTransform || !out.FileChanges[0].TrustedPersisted {
		t.Fatalf("transform output = %+v", out)
	}
	if out.FileChanges[0].Path != path {
		t.Fatalf("path = %q", out.FileChanges[0].Path)
	}

	out = executeClaimedShell(t, tool, "session", "run-mismatch", ShellArgs{
		Command: "printf 'new\\n' > created.txt", WorkingDir: dir,
		OutputClaims: []OutputClaim{{Path: "created.txt", Kind: filetrack.ClaimTransform}},
	})
	if len(out.FileChanges) != 0 || len(out.FilesystemObservations) != 1 || out.FilesystemObservations[0].Classification != filetrack.ObservationClaimMismatch {
		t.Fatalf("mismatch output = %+v", out)
	}
}

func TestShellMaterializationNeverProducesAttributedRows(t *testing.T) {
	dir := t.TempDir()
	store, err := filetrack.Open(filepath.Join(t.TempDir(), "file_history.db"), filetrack.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.recorder = filetrack.NewRecorder(store)
	out := executeClaimedShell(t, tool, "session", "run-materialize", ShellArgs{
		Command: "mkdir -p imported && printf 'external\\n' > imported/file.txt", WorkingDir: dir,
		OutputClaims: []OutputClaim{{Path: "imported/**", Kind: filetrack.ClaimMaterialize}},
	})
	if len(out.FileChanges) != 0 || len(out.FilesystemObservations) != 1 || out.FilesystemObservations[0].Classification != filetrack.ObservationMaterialized {
		t.Fatalf("materialize output = %+v", out)
	}
	changes, err := store.ListRecentRunChanges(context.Background(), "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("materialization entered attributed history: %+v", changes)
	}
}

func TestOutputClaimValidationRejectsConflictsAndGitAdmin(t *testing.T) {
	dir := t.TempDir()
	if _, err := normalizeOutputClaims(dir, []OutputClaim{{Path: "a.txt", Kind: "transform"}, {Path: "./a.txt", Kind: "generate"}}); err == nil {
		t.Fatal("identical normalized conflict accepted")
	}
	if _, err := normalizeOutputClaims(dir, []OutputClaim{{Path: ".git/config", Kind: "transform"}}); err == nil {
		t.Fatal("Git administration claim accepted")
	}
	claims, err := normalizeOutputClaims(dir, []OutputClaim{{Path: "*.txt", Kind: "generate"}, {Path: "a.txt", Kind: "transform"}})
	if err != nil {
		t.Fatal(err)
	}
	matches := matchingOutputClaims(claims, filepath.Join(dir, "a.txt"))
	if len(matches) != 1 || !claims[matches[0]].literal || claims[matches[0]].kind != "transform" {
		t.Fatalf("literal precedence = %#v over %#v", matches, claims)
	}
	if covered := allMatchingOutputClaims(claims, filepath.Join(dir, "a.txt")); len(covered) != 2 {
		t.Fatalf("overlapping claims were not both marked covered: %#v", covered)
	}
}
