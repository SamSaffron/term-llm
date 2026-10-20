package serve

import (
	"context"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
	"os"
	"path/filepath"
	"testing"
)

func addShareAudioFixture(t *testing.T, store session.Store, id string) int64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private-recording.mp3")
	if err := os.WriteFile(path, mediafixture.MP3(), 0o600); err != nil {
		t.Fatal(err)
	}
	item := core.MediaArtifact{Reference: "0123456789abcdef0123456789abcdef", SourcePath: path, Name: "narration.mp3", MediaType: "audio/mpeg"}
	messages := []core.Message{
		{Role: core.RoleTool, Parts: []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{Name: "show_media", Media: []core.MediaArtifact{item}}}}},
		core.AssistantText("![Listen](term-llm-media://" + item.Reference + ")"),
	}
	for _, message := range messages {
		if err := store.AddMessage(context.Background(), id, session.NewMessage(id, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := store.GetMessages(context.Background(), id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows[len(rows)-1].ID
}
