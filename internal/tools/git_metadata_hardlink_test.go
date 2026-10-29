package tools

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/image"
)

func TestGitMetadataSecurity_HardlinkAliasesDoNotMutateMetadata(t *testing.T) {
	for _, toolName := range []string{WriteFileToolName, EditFileToolName, UnifiedDiffToolName, ImageGenerateToolName} {
		t.Run(toolName, func(t *testing.T) {
			root := gitMetadataSecurityFixture(t)
			metadata := filepath.Join(root, ".git", "config")
			alias := filepath.Join(root, "ordinary-alias")
			if err := os.Link(metadata, alias); err != nil {
				t.Skipf("hardlinks unavailable: %v", err)
			}
			manager := NewApprovalManager(NewToolPermissions())
			if err := manager.permissions.AddWriteDir(root); err != nil {
				t.Fatal(err)
			}
			if toolName == ImageGenerateToolName {
				tool := NewImageGenerateTool(manager, nil, "", nil, "", "")
				args := ImageGenerateArgs{OutputPath: alias, Prompt: "hardlink isolation fixture"}
				if _, denied := tool.approveOutputPath(t.Context(), args); denied != nil {
					t.Fatalf("ordinary output path denied: %+v", denied)
				}
				// Exercise persistence without a provider or any external call.
				_, _, errOut := tool.saveImageResult(args, &image.ImageResult{Data: []byte("# after\n")}, t.TempDir())
				if errOut != nil {
					t.Fatalf("save image: %+v", errOut)
				}
			} else if out := executeGitMetadataSecurityTool(t, manager, toolName, alias); out.IsError {
				t.Fatalf("ordinary alias write failed: %+v", out)
			}
			for path, want := range map[string]string{metadata: "# before\n", alias: "# after\n"} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Errorf("%s content=%q error=%v, want %q; alias replacement must not mutate Git metadata inode", path, data, err, want)
				}
			}
		})
	}
}
