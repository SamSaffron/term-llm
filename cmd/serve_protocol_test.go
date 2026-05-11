package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func TestParseUserMessageContent_RejectsInvalidSmallInlineImage(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	content, err := json.Marshal([]map[string]any{{
		"type":      "input_image",
		"image_url": "data:image/png;base64,!!!=",
		"filename":  "bad.png",
	}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	_, err = parseUserMessageContent(content)
	if err == nil {
		t.Fatal("parseUserMessageContent() error = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "bad.png") || !strings.Contains(err.Error(), "decode base64") {
		t.Fatalf("parseUserMessageContent() error = %v, want filename + decode error", err)
	}
}

func TestDecodeUploadedFile_RejectsOversizedPayloadBeforeDecode(t *testing.T) {
	b64 := strings.Repeat("A", base64.StdEncoding.EncodedLen(maxAttachmentBytes+1)-1) + "!"

	_, err := decodeUploadedFile("too-large.bin", b64)
	if err == nil {
		t.Fatal("decodeUploadedFile() error = nil, want size limit error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d MB limit", maxAttachmentBytes>>20)) {
		t.Fatalf("decodeUploadedFile() error = %v, want size limit error", err)
	}
	if strings.Contains(err.Error(), "decode base64") {
		t.Fatalf("decodeUploadedFile() error = %v, want size check before base64 decode", err)
	}
}

func TestParseUserMessageContent_InlineImagesDoNotHitUploadsDir(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	content := marshalInlineImageParts(t, 1)

	msg, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent() error = %v", err)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("len(msg.Parts) = %d, want 1", len(msg.Parts))
	}
	if msg.Parts[0].ImagePath != "" {
		t.Fatalf("msg.Parts[0].ImagePath = %q, want empty", msg.Parts[0].ImagePath)
	}

	uploadsDir := filepath.Join(dataHome, "term-llm", "uploads")
	if _, err := os.Stat(uploadsDir); !os.IsNotExist(err) {
		t.Fatalf("uploads dir stat err = %v, want not exist", err)
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
