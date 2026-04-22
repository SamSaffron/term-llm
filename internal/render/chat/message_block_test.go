package chat

import (
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
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
