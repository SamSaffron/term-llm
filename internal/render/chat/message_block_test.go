package chat

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestMessageBlockRenderer_UserMessageBackground_UsesTruecolorTheme(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: "contrast check",
	}

	rendered := renderer.renderUserMessage(msg)
	if !strings.Contains(rendered, "\x1b[48;2;60;56;54m") {
		t.Fatalf("expected truecolor user message background #3c3836, got %q", rendered)
	}
}

func TestMessageBlockRenderer_UserMessageRowsFillRendererWidth(t *testing.T) {
	const width = 24
	renderer := NewMessageBlockRenderer(width, nil, false)
	msg := &session.Message{
		ID:   1,
		Role: llm.RoleUser,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: "alpha beta gamma delta epsilon\n\nwide 😀 text"},
			{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
		},
	}

	rendered := renderer.renderUserMessage(msg)
	lines := strings.Split(rendered, "\n")
	if len(lines) < 5 {
		t.Fatalf("expected wrapped content, blank content line, attachment metadata, and trailing blank line; got %d lines in %q", len(lines), rendered)
	}

	messageRows := lines[:len(lines)-2] // Keep the final inter-message blank line unstyled/unpadded.
	if len(messageRows) < 4 {
		t.Fatalf("expected at least 4 user-message rows, got %d in %q", len(messageRows), rendered)
	}

	foundBlankContentRow := false
	foundAttachmentMetaRow := false
	for i, line := range messageRows {
		plain := ui.StripANSI(line)
		if got := xansi.StringWidth(plain); got != width {
			t.Fatalf("message row %d width = %d, want %d; plain=%q rendered=%q", i, got, width, plain, line)
		}
		if strings.TrimSpace(plain) == "" {
			foundBlankContentRow = true
		}
		if strings.Contains(plain, "[with: image 1]") {
			foundAttachmentMetaRow = true
		}
	}
	if !foundBlankContentRow {
		t.Fatalf("expected intentional blank content row to be padded to width in %q", rendered)
	}
	if !foundAttachmentMetaRow {
		t.Fatalf("expected attachment metadata row in %q", rendered)
	}

	if lines[len(lines)-2] != "" || lines[len(lines)-1] != "" {
		t.Fatalf("expected final inter-message blank line to remain unstyled, got trailing split lines %q", lines[len(lines)-2:])
	}
}

func TestMessageBlockRenderer_UserMessageWithImageParts_ShowsAttachmentMeta(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: "describe this",
		Parts: []llm.Part{
			{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: llm.PartText, Text: "describe this"},
		},
	}

	rendered := renderer.renderUserMessage(msg)
	if !strings.Contains(rendered, "describe this") {
		t.Fatalf("expected user text in rendered message, got %q", rendered)
	}
	if !strings.Contains(rendered, "[with: image 1]") {
		t.Fatalf("expected image attachment meta, got %q", rendered)
	}
}

func TestMessageBlockRenderer_ImageOnlyUserMessage_ShowsPlaceholderAndAttachmentMeta(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: "[image 1]",
		Parts: []llm.Part{
			{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
		},
	}

	rendered := renderer.renderUserMessage(msg)
	if !strings.Contains(rendered, "[image]") {
		t.Fatalf("expected image-only placeholder, got %q", rendered)
	}
	if !strings.Contains(rendered, "[with: image 1]") {
		t.Fatalf("expected image attachment meta, got %q", rendered)
	}
}

func TestMessageBlockRenderer_MultipleImages_ShowsCountedAttachmentMeta(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: "compare these",
		Parts: []llm.Part{
			{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/png", Base64: "aGVsbG8="}},
			{Type: llm.PartImage, ImageData: &llm.ToolImageData{MediaType: "image/jpeg", Base64: "d29ybGQ="}},
			{Type: llm.PartText, Text: "compare these"},
		},
	}

	rendered := renderer.renderUserMessage(msg)
	if !strings.Contains(rendered, "[with: 2 images]") {
		t.Fatalf("expected counted image attachment meta, got %q", rendered)
	}
}

func TestMessageBlockRenderer_UserMessageWithEmbeddedFile_ShowsAttachmentMetaWithoutBody(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	text := "what number was in the file\n\n" + llm.EmbeddedFileIntro + "\n\n" + llm.FormatEmbeddedFileText("test.txt", "text/plain", "100\n")
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: text,
		Parts: []llm.Part{
			{Type: llm.PartText, Text: text},
		},
	}

	rendered := renderer.renderUserMessage(msg)
	plain := ui.StripANSI(rendered)
	if !strings.Contains(plain, "what number was in the file") {
		t.Fatalf("expected prompt text in rendered message, got %q", plain)
	}
	if !strings.Contains(plain, "[with: test.txt]") {
		t.Fatalf("expected file attachment meta, got %q", plain)
	}
	if strings.Contains(plain, "100") || strings.Contains(plain, "BEGIN USER-PROVIDED FILE") {
		t.Fatalf("rendered message should hide embedded file body, got %q", plain)
	}
}

func TestMessageBlockRenderer_FileOnlyUserMessage_ShowsPlaceholderAndAttachmentMeta(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	text := llm.FormatEmbeddedFileText("test.txt", "text/plain", "100\n")
	msg := &session.Message{
		ID:          1,
		Role:        llm.RoleUser,
		TextContent: text,
		Parts:       []llm.Part{{Type: llm.PartFile, Text: text, FileData: &llm.ToolFileData{Filename: "test.txt"}}},
	}

	rendered := renderer.renderUserMessage(msg)
	plain := ui.StripANSI(rendered)
	if !strings.Contains(plain, "[file]") || !strings.Contains(plain, "[with: test.txt]") {
		t.Fatalf("expected file placeholder and attachment meta, got %q", plain)
	}
	if strings.Contains(plain, "100") {
		t.Fatalf("rendered file-only message should hide embedded file body, got %q", plain)
	}
}

func TestMessageBlockRenderer_RenderReasoningSummaryCollapsedByDefault(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:                  llm.PartText,
			Text:                  "Final answer.",
			ReasoningContent:      "**Inspecting repo**\n\nChecking files.",
			ReasoningKind:         llm.ReasoningKindSummary,
			ReasoningSummaryTitle: "Inspecting repo",
		}},
	}

	rendered := ui.StripANSI(renderer.renderAssistantMessage(msg))
	if !strings.Contains(rendered, "▸ Thought: Inspecting repo") {
		t.Fatalf("expected collapsed reasoning header, got %q", rendered)
	}
	if strings.Contains(rendered, "Checking files.") {
		t.Fatalf("collapsed reasoning should hide body, got %q", rendered)
	}
	if !strings.Contains(rendered, "Final answer.") {
		t.Fatalf("expected assistant text, got %q", rendered)
	}
}

func TestMessageBlockRenderer_LegacyEmptyKindReasoningRendersAsSummary(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	msg := &session.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:             llm.PartText,
			Text:             "Final answer.",
			ReasoningContent: "**Legacy summary**\n\nOlder saved content.",
		}},
	}

	rendered := ui.StripANSI(renderer.renderAssistantMessage(msg))
	if !strings.Contains(rendered, "▸ Thought: Legacy summary") {
		t.Fatalf("legacy empty-kind reasoning should render as a collapsed summary, got %q", rendered)
	}
	if strings.Contains(rendered, "Older saved content.") {
		t.Fatalf("legacy summary should be collapsed by default, got %q", rendered)
	}
}

func TestMessageBlockRenderer_RenderReasoningSummaryExpanded(t *testing.T) {
	renderer := NewMessageBlockRenderer(80, nil, false)
	cfg := config.DefaultReasoningConfig()
	cfg.Display = config.ReasoningDisplayExpanded
	renderer.SetReasoningConfig(cfg)
	msg := &session.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:             llm.PartText,
			Text:             "Answer.",
			ReasoningContent: "**Planning**\n\nI checked **tests**.",
			ReasoningKind:    llm.ReasoningKindSummary,
		}},
	}

	rendered := ui.StripANSI(renderer.renderAssistantMessage(msg))
	if !strings.Contains(rendered, "▾ Thought: Planning") {
		t.Fatalf("expected expanded reasoning header, got %q", rendered)
	}
	if !strings.Contains(rendered, "I checked") || !strings.Contains(rendered, "tests") {
		t.Fatalf("expected expanded reasoning body, got %q", rendered)
	}
}

func TestMessageBlockRenderer_HidesReasoningWhenOffOrStatusOnly(t *testing.T) {
	msg := &session.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:             llm.PartText,
			Text:             "Answer.",
			ReasoningContent: "**Planning**\n\nHidden body.",
			ReasoningKind:    llm.ReasoningKindSummary,
		}},
	}
	for _, display := range []string{config.ReasoningDisplayOff, config.ReasoningDisplayStatus} {
		renderer := NewMessageBlockRenderer(80, nil, false)
		cfg := config.DefaultReasoningConfig()
		cfg.Display = display
		renderer.SetReasoningConfig(cfg)
		rendered := ui.StripANSI(renderer.renderAssistantMessage(msg))
		if strings.Contains(rendered, "Thought:") || strings.Contains(rendered, "Hidden body") {
			t.Fatalf("display=%s should hide reasoning, got %q", display, rendered)
		}
		if !strings.Contains(rendered, "Answer.") {
			t.Fatalf("display=%s should keep assistant text, got %q", display, rendered)
		}
	}
}

func TestMessageBlockRenderer_RawReasoningIsCollapsedByDefaultAndExpandedByDetails(t *testing.T) {
	msg := &session.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:             llm.PartText,
			Text:             "Answer.",
			ReasoningContent: "raw thinking chain",
			ReasoningKind:    llm.ReasoningKindRaw,
		}},
	}

	renderer := NewMessageBlockRenderer(80, nil, false)
	rendered := ui.StripANSI(renderer.renderAssistantMessage(msg))
	if !strings.Contains(rendered, "▸ Thinking...") {
		t.Fatalf("raw reasoning should render as a collapsed generic thought by default, got %q", rendered)
	}
	if strings.Contains(rendered, "raw thinking chain") || strings.Contains(rendered, "Raw thinking") {
		t.Fatalf("collapsed raw reasoning should hide the body and avoid raw-specific label, got %q", rendered)
	}

	cfg := config.DefaultReasoningConfig()
	cfg.Display = config.ReasoningDisplayExpanded
	renderer.SetReasoningConfig(cfg)
	rendered = ui.StripANSI(renderer.renderAssistantMessage(msg))
	if !strings.Contains(rendered, "▾ Thinking...") || !strings.Contains(rendered, "raw thinking chain") {
		t.Fatalf("expanded details should show raw reasoning content, got %q", rendered)
	}
}

func TestMessageBlockRenderer_NeverRendersUnknownOrEncryptedReasoning(t *testing.T) {
	for _, kind := range []llm.ReasoningKind{llm.ReasoningKindUnknown, llm.ReasoningKindEncrypted} {
		renderer := NewMessageBlockRenderer(80, nil, false)
		msg := &session.Message{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{{
				Type:             llm.PartText,
				Text:             "Answer.",
				ReasoningContent: "do not show",
				ReasoningKind:    kind,
			}},
		}
		rendered := ui.StripANSI(renderer.renderAssistantMessage(msg))
		if strings.Contains(rendered, "do not show") || strings.Contains(rendered, "Thought:") {
			t.Fatalf("kind=%s should not render reasoning, got %q", kind, rendered)
		}
	}
}
