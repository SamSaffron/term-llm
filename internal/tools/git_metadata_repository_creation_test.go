package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitMetadataSecurity_CannotAssembleImplicitRepository(t *testing.T) {
	files := map[string]string{
		"config":            "[core]\n\tbare = true\n",
		"HEAD":              "ref: refs/heads/main\n",
		"objects/info/keep": "fixture\n",
		"refs/heads/.keep":  "fixture\n",
	}
	var permute func(order, remaining []string)
	permute = func(order, remaining []string) {
		if len(remaining) != 0 {
			for i, name := range remaining {
				next := append([]string(nil), remaining[:i]...)
				next = append(next, remaining[i+1:]...)
				permute(append(append([]string(nil), order...), name), next)
			}
			return
		}
		t.Run(strings.Join(order, "_"), func(t *testing.T) {
			root := gitMetadataSecurityFixture(t)
			manager := NewApprovalManager(NewToolPermissions())
			manager.IgnoreProjectApprovals = true
			if err := manager.permissions.AddWriteDir(root); err != nil {
				t.Fatal(err)
			}
			src := filepath.Join(root, "src")
			for _, name := range order {
				args, err := json.Marshal(WriteFileArgs{Path: filepath.Join(src, filepath.FromSlash(name)), Content: files[name]})
				if err != nil {
					t.Fatal(err)
				}
				out, err := NewWriteFileTool(manager).Execute(t.Context(), args)
				if err != nil {
					t.Fatal(err)
				}
				if out.IsError != (name == "HEAD") {
					t.Errorf("write %s error=%v output=%q; only the reserved HEAD marker must be denied", name, out.IsError, out.Content)
				}
			}
			if _, err := os.Stat(filepath.Join(src, "HEAD")); !os.IsNotExist(err) {
				t.Errorf("implicit repository HEAD was created: %v", err)
			}
			if repo := DetectGitRepo(src); !repo.IsRepo || repo.Root != root {
				t.Errorf("source directory changed Git identity: %+v, want enclosing checkout %q", repo, root)
			}
		})
	}
	permute(nil, []string{"config", "HEAD", "objects/info/keep", "refs/heads/.keep"})
}

func TestGitMetadataSecurity_CannotCompletePartialRepository(t *testing.T) {
	for _, missing := range []string{"objects", "refs", "commondir"} {
		t.Run(missing, func(t *testing.T) {
			root := gitMetadataSecurityFixture(t)
			src := filepath.Join(root, "testdata", "fixture.git")
			writeGitMetadataSecurityFile(t, filepath.Join(src, "HEAD"), "ref: refs/heads/main\n")
			present := "objects"
			if missing == present {
				present = "refs"
			}
			if err := os.MkdirAll(filepath.Join(src, present), 0755); err != nil {
				t.Fatal(err)
			}
			manager := NewApprovalManager(NewToolPermissions())
			if err := manager.permissions.AddWriteDir(root); err != nil {
				t.Fatal(err)
			}
			// Ordinary siblings remain writable while the layout is incomplete.
			if out := executeGitMetadataSecurityTool(t, manager, WriteFileToolName, filepath.Join(src, "notes.txt")); out.IsError {
				t.Fatalf("ordinary sibling write failed: %+v", out)
			}
			marker := filepath.Join(src, missing, "info", "keep")
			if missing == "commondir" {
				marker = filepath.Join(src, missing)
			}
			if out := executeGitMetadataSecurityTool(t, manager, WriteFileToolName, marker); !out.IsError {
				t.Fatalf("missing Git structure was created: %+v", out)
			}
			if _, err := os.Stat(filepath.Join(src, missing)); !os.IsNotExist(err) {
				t.Errorf("missing structure %s was created: %v", missing, err)
			}
			if repo := DetectGitRepo(src); !repo.IsRepo || repo.Root != root {
				t.Errorf("partial fixture became an implicit repository: %+v", repo)
			}
		})
	}
}

func TestGitMetadataSecurity_HeadMarkerReadsAndYolo(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "HEAD")
	writeGitMetadataSecurityFile(t, path, "ref: refs/heads/main\n")
	manager := NewApprovalManager(NewToolPermissions())
	if err := manager.permissions.AddReadDir(root); err != nil {
		t.Fatal(err)
	}
	outcome, err := manager.CheckPathApproval(ReadFileToolName, path, path, false)
	if err != nil || outcome == Cancel {
		t.Fatalf("approved HEAD read failed: outcome=%v error=%v", outcome, err)
	}
	outcome, err = manager.CheckPathApproval(WriteFileToolName, path, path, true)
	requireGitAdministrativeWriteDenied(t, outcome, err)
	manager.SetApprovalMode(ModeYolo)
	outcome, err = manager.CheckPathApprovalWithContext(context.Background(), WriteFileToolName, path, path, true)
	if err != nil || outcome == Cancel {
		t.Fatalf("explicit yolo HEAD write failed: outcome=%v error=%v", outcome, err)
	}
}
