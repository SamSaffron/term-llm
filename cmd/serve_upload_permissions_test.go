package cmd

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestGrantUploadedFileReads(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	file, err := parseUploadedFilePart("archive.zip", "application/zip", base64.StdEncoding.EncodeToString([]byte("PK\x00\xff")))
	if err != nil {
		t.Fatal(err)
	}
	imagePath, err := saveUploadedBytes("image.png", []byte("image"))
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := saveUploadedBytes("other.bin", []byte("other session"))
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	rt := &serveRuntime{toolMgr: &tools.ToolManager{ApprovalMgr: approval}}
	rt.grantUploadedFileReads([]llm.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{file, {Type: llm.PartImage, ImagePath: imagePath}, {Type: llm.PartFile, FilePath: outside}}},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartFile, FilePath: sibling}}},
	})
	for _, path := range []string{file.FilePath, imagePath} {
		for _, tool := range []string{"read_file", "grep", "glob", "view_image"} {
			outcome, err := approval.CheckPathApproval(tool, path, "", false)
			if err != nil || outcome == tools.Cancel {
				t.Fatalf("%s read %s: %v, %v", tool, path, outcome, err)
			}
		}
	}
	for _, tc := range []struct {
		path  string
		write bool
	}{
		{file.FilePath, true}, {sibling, false}, {outside, false}, {serveUploadsDir(), false},
	} {
		outcome, _ := approval.CheckPathApproval("read_file", tc.path, "", tc.write)
		if outcome != tools.Cancel {
			t.Fatalf("unexpected permission: %+v, %v", tc, outcome)
		}
	}
}
