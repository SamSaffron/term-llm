package cmd

import (
	"context"

	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"

	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestDeriveAskResumeAutoToolsFromMissingStructuredAttachment(t *testing.T) {
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock", Mode: session.ModeAsk}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	message := core.Message{Role: core.RoleUser, Parts: []core.Part{{
		Type: core.PartImage, ImagePath: filepath.Join(t.TempDir(), "missing.png"), ImageData: &core.ToolImageData{MediaType: "image/png"},
	}}}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
		t.Fatal(err)
	}
	settings := SessionSettings{Tools: tools.ShellToolName}
	deriveAskResumeAutoTools(ctx, store, sess, &settings)
	if settings.Tools != tools.ShellToolName {
		t.Fatalf("configured Tools changed to %q", settings.Tools)
	}
	if strings.Join(settings.AutoTools, ",") != tools.ReadFileToolName+","+tools.ViewImageToolName {
		t.Fatalf("AutoTools = %v", settings.AutoTools)
	}
}
