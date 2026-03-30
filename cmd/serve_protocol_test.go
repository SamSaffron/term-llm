package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseUserMessageContentLimitsNativeInlineImages(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+X2ioAAAAASUVORK5CYII="

	parts := make([]map[string]any, 0, maxAttachments+1)
	for i := 0; i < maxAttachments+1; i++ {
		parts = append(parts, map[string]any{
			"type":      "input_image",
			"image_url": fmt.Sprintf("data:image/png;base64,%s", tinyPNG),
			"filename":  fmt.Sprintf("image-%d.png", i),
		})
	}

	content, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}

	_, err = parseUserMessageContent(content)
	if err == nil {
		t.Fatalf("expected attachment limit error, got nil")
	}
	if !strings.Contains(err.Error(), "too many attachments") {
		t.Fatalf("expected attachment limit error, got %v", err)
	}
}
