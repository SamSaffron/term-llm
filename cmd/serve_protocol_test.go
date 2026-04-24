package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const onePixelPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4//8/AwAI/AL+KDvV3wAAAABJRU5ErkJggg=="

func TestParseUserMessageContent_AllowsUpToMaxInlineImages(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	content := marshalInlineImageParts(t, maxAttachments)

	msg, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent() error = %v", err)
	}
	if len(msg.Parts) != maxAttachments {
		t.Fatalf("len(msg.Parts) = %d, want %d", len(msg.Parts), maxAttachments)
	}
}

func TestParseUserMessageContent_RejectsTooManyInlineImages(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	content := marshalInlineImageParts(t, maxAttachments+1)

	_, err := parseUserMessageContent(content)
	if err == nil {
		t.Fatal("parseUserMessageContent() error = nil, want attachment limit error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("too many attachments (max %d)", maxAttachments)) {
		t.Fatalf("parseUserMessageContent() error = %v, want attachment limit error", err)
	}
}

func TestParseUserMessageContent_RejectsOversizedInlineImage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	raw := bytes.Repeat([]byte("a"), maxAttachmentBytes+1)
	content, err := json.Marshal([]map[string]any{{
		"type":      "input_image",
		"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw),
		"filename":  "too-large.png",
	}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	_, err = parseUserMessageContent(content)
	if err == nil {
		t.Fatal("parseUserMessageContent() error = nil, want size limit error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d MB limit", maxAttachmentBytes>>20)) {
		t.Fatalf("parseUserMessageContent() error = %v, want size limit error", err)
	}
}

func marshalInlineImageParts(t *testing.T, count int) json.RawMessage {
	t.Helper()

	parts := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		parts = append(parts, map[string]any{
			"type":      "input_image",
			"image_url": onePixelPNGDataURL,
			"filename":  fmt.Sprintf("image-%d.png", i),
		})
	}

	content, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return content
}
