package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
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

func TestParseUserMessageContent_DownloadsRemoteResponsesImageURL(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	raw := mustDecodeDataURLBase64(t, onePixelPNGDataURL)
	server := newRemoteImageServer(t, "image/png", raw)
	defer server.Close()

	content, err := json.Marshal([]map[string]any{
		{
			"type":      "input_image",
			"image_url": server.URL + "/pixel.png",
		},
		{
			"type": "input_text",
			"text": "describe this image",
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	msg, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent() error = %v", err)
	}
	assertRemoteImagePart(t, msg, raw)
	if msg.Parts[1].Type != llm.PartText || msg.Parts[1].Text != "describe this image" {
		t.Fatalf("parts[1] = %+v, want trailing text", msg.Parts[1])
	}
}

func TestParseUserMessageContent_DownloadsRemoteChatImageURL(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	raw := mustDecodeDataURLBase64(t, onePixelPNGDataURL)
	server := newRemoteImageServer(t, "image/png", raw)
	defer server.Close()

	content, err := json.Marshal([]map[string]any{
		{
			"type":      "image_url",
			"image_url": map[string]any{"url": server.URL + "/pixel.png"},
		},
		{
			"type": "text",
			"text": "describe this image",
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	msg, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent() error = %v", err)
	}
	assertRemoteImagePart(t, msg, raw)
	if msg.Parts[1].Type != llm.PartText || msg.Parts[1].Text != "describe this image" {
		t.Fatalf("parts[1] = %+v, want trailing text", msg.Parts[1])
	}
}

func assertRemoteImagePart(t *testing.T, msg llm.Message, raw []byte) {
	t.Helper()

	if msg.Role != llm.RoleUser {
		t.Fatalf("msg.Role = %q, want %q", msg.Role, llm.RoleUser)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("len(msg.Parts) = %d, want 2", len(msg.Parts))
	}
	if msg.Parts[0].Type != llm.PartImage {
		t.Fatalf("parts[0].Type = %q, want %q", msg.Parts[0].Type, llm.PartImage)
	}
	if msg.Parts[0].ImageData == nil {
		t.Fatal("parts[0].ImageData = nil, want image data")
	}
	if msg.Parts[0].ImageData.MediaType != "image/png" {
		t.Fatalf("parts[0].ImageData.MediaType = %q, want image/png", msg.Parts[0].ImageData.MediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(msg.Parts[0].ImageData.Base64)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatal("decoded image bytes do not match downloaded image")
	}
	if msg.Parts[0].ImagePath == "" {
		t.Fatal("parts[0].ImagePath = empty, want saved upload path")
	}
	if _, err := os.Stat(msg.Parts[0].ImagePath); err != nil {
		t.Fatalf("saved upload missing at %q: %v", msg.Parts[0].ImagePath, err)
	}
}

func newRemoteImageServer(t *testing.T, mediaType string, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", mediaType)
		_, _ = w.Write(body)
	}))
}

func mustDecodeDataURLBase64(t *testing.T, dataURL string) []byte {
	t.Helper()
	_, b64 := parseDataURL(dataURL)
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("DecodeString() error = %v", err)
	}
	return data
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
