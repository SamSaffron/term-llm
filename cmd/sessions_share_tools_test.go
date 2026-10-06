package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	sharepkg "github.com/samsaffron/term-llm/internal/share"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestSessionsShareNoToolsIsStickyAcrossUpdates(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	viper.Reset()
	t.Cleanup(viper.Reset)
	oldDBPath, oldNoSession := sessionDBPath, noSession
	oldVisibility, oldNew, oldJSON, oldRaw := sessionsShareVisibility, sessionsShareNew, sessionsShareJSON, sessionsShareIncludeRawReasoning
	oldNoTools, oldNoImages, oldFactory := sessionsShareNoTools, sessionsShareNoImages, newSessionSharePublisher
	t.Cleanup(func() {
		sessionDBPath, noSession = oldDBPath, oldNoSession
		sessionsShareVisibility, sessionsShareNew, sessionsShareJSON, sessionsShareIncludeRawReasoning = oldVisibility, oldNew, oldJSON, oldRaw
		sessionsShareNoTools, sessionsShareNoImages, newSessionSharePublisher = oldNoTools, oldNoImages, oldFactory
	})
	sessionDBPath, noSession = filepath.Join(t.TempDir(), "sessions.db"), false
	sessionsShareVisibility, sessionsShareJSON, sessionsShareIncludeRawReasoning = "private", true, false

	store, err := session.NewStore(session.Config{Enabled: true, Path: sessionDBPath})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	sess := &session.Session{ID: "session-share-tools", Name: "Tools", Provider: "mock", Model: "mock", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	call := llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{
		{Type: llm.PartText, Text: "Let me check."},
		{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{ID: "c1", Name: "shell", Arguments: []byte(`{"command":"env"}`)}},
	}}
	result := llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "c1", Name: "shell", Content: "API_KEY=sk-live-SECRET"}}}}
	for _, message := range []llm.Message{llm.UserText("what is set?"), call, result, llm.AssistantText("Everything looks fine.")} {
		if err := store.AddMessage(context.Background(), sess.ID, session.NewMessage(sess.ID, message, -1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	publisher := &sessionsSharePublisherMock{}
	newSessionSharePublisher = func(config.ShareConfig) (sharepkg.Publisher, error) { return publisher, nil }
	shareHTML := func() string {
		t.Helper()
		command := &cobra.Command{}
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		if err := runSessionsShare(command, []string{sess.ID}); err != nil {
			t.Fatal(err)
		}
		for _, file := range publisher.lastRequest.Files {
			if file.Name == "index.html" {
				return string(file.Content)
			}
		}
		t.Fatal("no index.html")
		return ""
	}
	persisted := func() *session.ShareState {
		t.Helper()
		check, err := session.NewStore(session.Config{Enabled: true, Path: sessionDBPath})
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		got, err := check.Get(context.Background(), sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Share
	}

	sessionsShareNew, sessionsShareNoTools = false, true
	html := shareHTML()
	if strings.Contains(html, "sk-live-SECRET") || strings.Contains(html, `"command"`) || !strings.Contains(html, "Let me check.") || !strings.Contains(html, "Everything looks fine.") {
		t.Fatalf("--no-tools share content wrong: %s", html)
	}
	if state := persisted(); state == nil || !state.ExcludeTools {
		t.Fatalf("exclusion not persisted: %+v", state)
	}

	// A plain update must not add tool output to the existing URL.
	sessionsShareNoTools = false
	if html := shareHTML(); publisher.updateCalls != 1 || strings.Contains(html, "sk-live-SECRET") {
		t.Fatalf("update widened the share (updates=%d)", publisher.updateCalls)
	}

	// Only an explicit new share may include tools again.
	sessionsShareNew = true
	if html := shareHTML(); publisher.createCalls != 2 || !strings.Contains(html, "sk-live-SECRET") {
		t.Fatalf("new share did not include tools (creates=%d)", publisher.createCalls)
	}
	if state := persisted(); state.ExcludeTools {
		t.Fatal("new share kept the old exclusion")
	}
}
