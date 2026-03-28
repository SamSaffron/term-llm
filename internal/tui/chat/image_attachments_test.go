package chat

import (
	"encoding/base64"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestIsImagePasteAttempt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		msg  tea.KeyMsg
		want bool
	}{
		{name: "bracketed paste", msg: tea.KeyMsg{Type: tea.KeyRunes, Paste: true}, want: true},
		{name: "ctrl v", msg: tea.KeyMsg{Type: tea.KeyCtrlV}, want: true},
		{name: "ctrl shift v string", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v"), Alt: true}, want: false},
		{name: "plain rune", msg: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}, want: false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isImagePasteAttempt(tc.msg)
			if got != tc.want {
				t.Fatalf("isImagePasteAttempt() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandleKeyMsg_PastedImageAttachesToComposer(t *testing.T) {
	m := newTestChatModel(false)

	orig := readClipboardImage
	readClipboardImage = func() ([]byte, error) {
		data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7ZQz8AAAAASUVORK5CYII=")
		if err != nil {
			t.Fatalf("failed to decode png fixture: %v", err)
		}
		return data, nil
	}
	defer func() { readClipboardImage = orig }()

	m.setTextareaValue("before")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ignored"), Paste: true})

	if len(m.images) != 1 {
		t.Fatalf("expected 1 attached image, got %d", len(m.images))
	}
	if got := m.textarea.Value(); got != "before" {
		t.Fatalf("textarea changed after image paste: got %q", got)
	}
}

func TestHandleKeyMsg_ImageSelectionAndRemoval(t *testing.T) {
	m := newTestChatModel(false)
	m.images = []ImageAttachment{{MediaType: "image/png", Data: []byte("a")}, {MediaType: "image/png", Data: []byte("b")}}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.selectedImage != 0 {
		t.Fatalf("expected selected image index 0 after up, got %d", m.selectedImage)
	}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if len(m.images) != 1 {
		t.Fatalf("expected 1 image after removal, got %d", len(m.images))
	}
	if string(m.images[0].Data) != "b" {
		t.Fatalf("expected second image to remain after removal")
	}
}

func TestHandleKeyMsg_SendAllowsImageOnlyMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.images = []ImageAttachment{{MediaType: "image/png", Data: []byte("img")}}

	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.messages) == 0 {
		t.Fatal("expected a user message to be queued")
	}
	last := m.messages[len(m.messages)-1]
	if len(last.Parts) == 0 || last.Parts[0].Type != llm.PartImage {
		t.Fatalf("expected first part to be image, got %+v", last.Parts)
	}
}

func TestSendMessage_IncludesImageParts(t *testing.T) {
	m := newTestChatModel(false)
	m.images = []ImageAttachment{{MediaType: "image/png", Data: []byte("img-data")}}

	_, _ = m.sendMessage("describe this")

	last := m.messages[len(m.messages)-1]
	if len(last.Parts) < 2 {
		t.Fatalf("expected image + text parts, got %d", len(last.Parts))
	}
	if last.Parts[0].Type != llm.PartImage {
		t.Fatalf("expected first part image, got %q", last.Parts[0].Type)
	}
	if last.Parts[1].Type != llm.PartText {
		t.Fatalf("expected second part text, got %q", last.Parts[1].Type)
	}
	if len(m.images) != 0 {
		t.Fatalf("expected images to be cleared after send, got %d", len(m.images))
	}
}

func TestSendMessage_InjectsPlatformDeveloperMessageOnFirstTurnEvenWithSystemInstructions(t *testing.T) {
	m := newTestChatModel(false)
	m.platformDeveloperMessage = "You are running on the CLI chat platform."
	m.config.Chat.Instructions = "Base system instructions"

	_, _ = m.sendMessage("hello")
	if len(m.messages) != 3 {
		t.Fatalf("expected system + developer + user messages after first send, got %d", len(m.messages))
	}
	if m.messages[0].Role != llm.RoleSystem {
		t.Fatalf("expected first message role system, got %q", m.messages[0].Role)
	}
	if m.messages[1].Role != llm.RoleDeveloper {
		t.Fatalf("expected second message role developer, got %q", m.messages[1].Role)
	}
}

func TestSendMessage_InjectsPlatformDeveloperMessageOnlyOnFirstTurn(t *testing.T) {
	m := newTestChatModel(false)
	m.platformDeveloperMessage = "You are running on the CLI chat platform."

	_, _ = m.sendMessage("hello")
	if len(m.messages) != 2 {
		t.Fatalf("expected developer + user messages after first send, got %d", len(m.messages))
	}
	if m.messages[0].Role != llm.RoleDeveloper {
		t.Fatalf("expected first message role developer, got %q", m.messages[0].Role)
	}

	_, _ = m.sendMessage("again")
	devCount := 0
	for _, msg := range m.messages {
		if msg.Role == llm.RoleDeveloper {
			devCount++
		}
	}
	if devCount != 1 {
		t.Fatalf("developer message count = %d, want 1", devCount)
	}
}

func TestSendMessage_InjectsPlatformDeveloperMessageWhenOriginChanges(t *testing.T) {
	m := newTestChatModel(false)
	m.platformDeveloperMessage = "You are running on the CLI chat platform."
	m.sess.Origin = session.OriginWeb
	m.messages = []session.Message{{
		SessionID:   m.sess.ID,
		Role:        llm.RoleUser,
		Parts:       []llm.Part{{Type: llm.PartText, Text: "from web"}},
		TextContent: "from web",
	}}

	_, _ = m.sendMessage("now from tui")
	if len(m.messages) < 3 {
		t.Fatalf("expected at least 3 messages after origin change, got %d", len(m.messages))
	}
	if m.messages[0].Role != llm.RoleDeveloper {
		t.Fatalf("expected prepended developer message, got %q", m.messages[0].Role)
	}
	if m.sess.Origin != session.OriginTUI {
		t.Fatalf("session origin = %q, want %q", m.sess.Origin, session.OriginTUI)
	}
}
