package session

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestShareBundleExcludesDeveloperMessages(t *testing.T) {
	const secret = "private platform instructions"
	messages := []Message{
		shareTestMessage(1, 1, core.RoleDeveloper, secret, ""),
		shareTestMessage(2, 2, core.RoleUser, "public question", ""),
		shareTestMessage(3, 3, core.RoleAssistant, "public answer", "response-a"),
	}
	messages[0].Parts = append(messages[0].Parts, core.Part{
		Type: core.PartImage,
		ImageData: &core.ToolImageData{
			MediaType: "image/png",
			Base64:    base64.StdEncoding.EncodeToString(testPNG(t, 4, 4, true)),
		},
	})
	for _, includeSystem := range []bool{false, true} {
		for _, mode := range []ExportImageMode{ExportImagesInline, ExportImagesAssets} {
			t.Run(fmt.Sprintf("system=%t/images=%s", includeSystem, mode), func(t *testing.T) {
				files, err := ShareBundle(&Session{ID: "share-session"}, messages, ExportOptions{
					IncludeSystem: includeSystem,
					Images:        mode,
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(files) != 2 {
					t.Fatalf("bundle has %d files, want only HTML and Markdown without developer assets", len(files))
				}
				for _, file := range files {
					content := string(file.Content)
					for _, hidden := range []string{secret, "data:image/", "assets/"} {
						if strings.Contains(content, hidden) {
							t.Errorf("%s leaked developer content %q", file.Name, hidden)
						}
					}
					for _, visible := range []string{"public question", "public answer"} {
						if !strings.Contains(content, visible) {
							t.Errorf("%s omitted %q", file.Name, visible)
						}
					}
				}
			})
		}
	}
}
