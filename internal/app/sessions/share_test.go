package sessions

import (
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestSessionShareExportOptionsRequireExplicitRawReasoning(t *testing.T) {
	cfg := &config.Config{Reasoning: config.ReasoningConfig{Raw: true, Source: config.ReasoningSourceAll, Export: config.ReasoningExportRaw}}
	sess := &session.Session{ID: "share-raw", Mode: session.ModeChat, Provider: "mock", Model: "mock", CreatedAt: time.Now()}
	messages := []session.Message{{
		SessionID: sess.ID, Role: core.RoleAssistant, TextContent: "Final answer.",
		Parts: []core.Part{{Type: core.PartText, Text: "Final answer.", ReasoningContent: "private raw chain", ReasoningKind: core.ReasoningKindRaw}},
	}}
	withoutOptions, err := ShareExportOptions(cfg, sess, false)
	if err != nil {
		t.Fatal(err)
	}
	without := session.ExportToMarkdown(sess, messages, withoutOptions)
	if strings.Contains(without, "private raw chain") {
		t.Fatalf("generic session share implicitly included raw reasoning: %q", without)
	}
	withOptions, err := ShareExportOptions(cfg, sess, true)
	if err != nil {
		t.Fatal(err)
	}
	with := session.ExportToMarkdown(sess, messages, withOptions)
	if !strings.Contains(with, "private raw chain") {
		t.Fatalf("explicit raw share omitted reasoning: %q", with)
	}
	if _, err := ShareExportOptions(&config.Config{}, sess, true); err == nil || !strings.Contains(err.Error(), "reasoning.raw") {
		t.Fatalf("disabled explicit raw error=%v", err)
	}
}
