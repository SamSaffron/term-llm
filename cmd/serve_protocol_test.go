package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const onePixelPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4//8/AwAI/AL+KDvV3wAAAABJRU5ErkJggg=="

func TestResponsesCreateRequestPreservesServiceTierOverride(t *testing.T) {
	for _, body := range []string{`{"service_tier":"priority"}`, `{"service_tier":""}`} {
		var req responsesCreateRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if req.ServiceTier == nil {
			t.Fatalf("service_tier pointer is nil for %s", body)
		}
	}
	var omitted responsesCreateRequest
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.ServiceTier != nil {
		t.Fatalf("omitted service_tier = %#v, want nil", omitted.ServiceTier)
	}
}

func TestResolveRequestSessionIDHeaderCompatibility(t *testing.T) {
	tests := []struct {
		name      string
		canonical string
		legacy    string
		want      string
	}{
		{name: "canonical", canonical: "sess-new", want: "sess-new"},
		{name: "legacy", legacy: "sess-old", want: "sess-old"},
		{name: "canonical preferred", canonical: "sess-new", legacy: "sess-old", want: "sess-new"},
		{name: "trimmed", canonical: "  sess-trim  ", want: "sess-trim"},
		{name: "missing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tt.canonical != "" {
				req.Header.Set(requestSessionIDHeader, tt.canonical)
			}
			if tt.legacy != "" {
				req.Header.Set(legacyRequestSessionIDHeader, tt.legacy)
			}
			if got := resolveRequestSessionID(req); got != tt.want {
				t.Fatalf("resolveRequestSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func makeOrientation6JPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("jpeg.Encode() error = %v", err)
	}
	raw := encoded.Bytes()
	exif := []byte{
		'E', 'x', 'i', 'f', 0, 0,
		'M', 'M', 0, 42, 0, 0, 0, 8,
		0, 1,
		0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0,
		0, 0, 0, 0,
	}
	segmentLength := len(exif) + 2
	withEXIF := make([]byte, 0, len(raw)+len(exif)+4)
	withEXIF = append(withEXIF, raw[:2]...)
	withEXIF = append(withEXIF, 0xff, 0xe1, byte(segmentLength>>8), byte(segmentLength))
	withEXIF = append(withEXIF, exif...)
	withEXIF = append(withEXIF, raw[2:]...)
	return withEXIF
}

func TestNormalizeProviderModelEffort_ExplicitEffortWinsOverSuffix(t *testing.T) {
	model, effort := normalizeProviderModelEffort("chatgpt", "gpt-5.5-medium", "xhigh")
	if model != "gpt-5.5" || effort != "xhigh" {
		t.Fatalf("normalizeProviderModelEffort = (%q, %q), want gpt-5.5/xhigh", model, effort)
	}
}

func TestNormalizeProviderModelEffort_PromotesSuffixWhenEffortUnset(t *testing.T) {
	model, effort := normalizeProviderModelEffort("chatgpt", "gpt-5.5-medium", "")
	if model != "gpt-5.5" || effort != "medium" {
		t.Fatalf("normalizeProviderModelEffort = (%q, %q), want gpt-5.5/medium", model, effort)
	}
}

func TestNormalizeProviderModelEffort_PreservesNaturalMaxModelName(t *testing.T) {
	model, effort := normalizeProviderModelEffort("chatgpt", "gpt-5.1-codex-max", "xhigh")
	if model != "gpt-5.1-codex-max" || effort != "xhigh" {
		t.Fatalf("normalizeProviderModelEffort = (%q, %q), want gpt-5.1-codex-max/xhigh", model, effort)
	}
}

func TestResponseRequestedRuntimePrefersNestedReasoningEffort(t *testing.T) {
	req := responsesCreateRequest{
		Provider:        "openai",
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "low",
		Reasoning:       &responsesReasoningRequest{Effort: "high"},
	}

	got := responseRequestedRuntime(req, "")
	if got.effort != "high" {
		t.Fatalf("responseRequestedRuntime effort = %q, want high", got.effort)
	}
}

func TestValidateResponseReasoningModeUsesFinalProviderModel(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		model     string
		mode      string
		explicit  bool
		wantMode  string
		wantClear bool
		wantErr   bool
	}{
		{name: "supported explicit", provider: "openai", model: "gpt-5.6-terra", mode: " Pro ", explicit: true, wantMode: "pro"},
		{name: "unsupported explicit", provider: "chatgpt", model: "gpt-5.6-terra", mode: "pro", explicit: true, wantErr: true},
		{name: "unsupported persisted", provider: "chatgpt", model: "gpt-5.6-terra", mode: "pro", wantClear: true},
		{name: "invalid explicit", provider: "openai", model: "gpt-5.6-sol", mode: "turbo", explicit: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, clear, err := validateResponseReasoningMode(tt.provider, tt.model, tt.mode, tt.explicit)
			if (err != nil) != tt.wantErr || mode != tt.wantMode || clear != tt.wantClear {
				t.Fatalf("validateResponseReasoningMode() = (%q, %t, %v), want (%q, %t, err=%t)", mode, clear, err, tt.wantMode, tt.wantClear, tt.wantErr)
			}
		})
	}
}

func TestResponsesFinalResponseUsesProviderRawInputTokens(t *testing.T) {
	result := serveRunResult{Usage: llm.Usage{
		InputTokens:            150,
		CachedInputTokens:      600,
		CacheWriteTokens:       250,
		OutputTokens:           100,
		ProviderRawInputTokens: 1000,
		ReasoningTokens:        40,
	}}

	response := responsesFinalResponse(result, "gpt-5.6-sol", "resp_test", 1)
	usage := response["usage"].(map[string]any)
	if got := usage["input_tokens"]; got != 1000 {
		t.Fatalf("input_tokens = %v, want provider raw total 1000", got)
	}
	if got := usage["total_tokens"]; got != 1100 {
		t.Fatalf("total_tokens = %v, want 1100", got)
	}
	outputDetails := usage["output_tokens_details"].(map[string]any)
	if got := outputDetails["reasoning_tokens"]; got != 40 {
		t.Fatalf("reasoning_tokens = %v, want 40", got)
	}
}

func TestSetSessionNumberHeader(t *testing.T) {
	recorder := httptest.NewRecorder()
	setSessionNumberHeader(recorder, &serveRuntime{sessionMeta: &session.Session{Number: 42}})
	if got := recorder.Header().Get("x-session-number"); got != "42" {
		t.Fatalf("x-session-number = %q, want 42", got)
	}
}

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

func TestParseUserMessageContent_RejectsUntrustedImageDimensions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 400, 200)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("jpeg.Encode() error = %v", err)
	}

	content, err := json.Marshal([]map[string]any{{
		"type":      "input_image",
		"image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes()),
		"filename":  "landscape.jpg",
		"width":     200,
		"height":    400,
	}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	msg, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent() error = %v", err)
	}
	if len(msg.Parts) != 1 || msg.Parts[0].ImageData == nil {
		t.Fatalf("message parts = %#v, want one image", msg.Parts)
	}
	if got := msg.Parts[0].ImageData; got.Width != 400 || got.Height != 200 {
		t.Fatalf("image dimensions = %dx%d, want decoded 400x200 rather than untrusted swapped values", got.Width, got.Height)
	}
}

func TestParseUserMessageContent_PersistsEXIFOrientedImageDimensions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	raw := makeOrientation6JPEG(t, 400, 200)
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("image.DecodeConfig() error = %v", err)
	}
	if cfg.Width != 400 || cfg.Height != 200 {
		t.Fatalf("stored JPEG dimensions = %dx%d, want 400x200", cfg.Width, cfg.Height)
	}

	content, err := json.Marshal([]map[string]any{{
		"type":      "input_image",
		"image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw),
		"filename":  "rotated.jpg",
		"width":     200,
		"height":    400,
	}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	msg, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent() error = %v", err)
	}
	if len(msg.Parts) != 1 || msg.Parts[0].ImageData == nil {
		t.Fatalf("message parts = %#v, want one image", msg.Parts)
	}
	if got := msg.Parts[0].ImageData; got.Width != 200 || got.Height != 400 {
		t.Fatalf("display dimensions = %dx%d, want EXIF-oriented 200x400", got.Width, got.Height)
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

func TestDecodeUploadedFile_AllowsWrappedBase64(t *testing.T) {
	wrapped := "aGVs\r\nbG8="
	raw, err := decodeUploadedFile("hello.txt", wrapped)
	if err != nil {
		t.Fatalf("decodeUploadedFile() error = %v", err)
	}
	if string(raw) != "hello" {
		t.Fatalf("decodeUploadedFile() = %q, want hello", string(raw))
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

func TestParseUserMessageContent_InlineImagesAreSavedToUploadsDir(t *testing.T) {
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
	if msg.Parts[0].ImagePath == "" {
		t.Fatal("msg.Parts[0].ImagePath is empty, want saved upload path")
	}

	uploadsDir := filepath.Join(dataHome, "term-llm", "uploads")
	entries, err := os.ReadDir(uploadsDir)
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("uploads dir has %d files, want 1", len(entries))
	}
}

func TestParseUserMessageContent_LargeImageSavesOriginalButSendsResizedInline(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	raw := makeLargeJPEG(t)
	if len(raw) <= maxLLMImageBytes {
		t.Fatalf("test JPEG is %d bytes, want > %d", len(raw), maxLLMImageBytes)
	}
	content, err := json.Marshal([]map[string]any{{
		"type":      "input_image",
		"image_url": "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw),
		"filename":  "page.jpg",
	}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	msg, err := parseUserMessageContent(content)
	if err != nil {
		t.Fatalf("parseUserMessageContent() error = %v", err)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("len(msg.Parts) = %d, want 1", len(msg.Parts))
	}
	part := msg.Parts[0]
	if part.ImagePath == "" {
		t.Fatal("ImagePath is empty, want saved original upload path")
	}
	saved, err := os.ReadFile(part.ImagePath)
	if err != nil {
		t.Fatalf("read saved original: %v", err)
	}
	if !bytes.Equal(saved, raw) {
		t.Fatalf("saved image differs from original upload")
	}
	if part.ImageData == nil || part.ImageData.Base64 == "" {
		t.Fatalf("ImageData missing")
	}
	inline, err := base64.StdEncoding.DecodeString(part.ImageData.Base64)
	if err != nil {
		t.Fatalf("decode inline image: %v", err)
	}
	if len(inline) >= len(raw) {
		t.Fatalf("inline image is %d bytes, want resized smaller than original %d", len(inline), len(raw))
	}
	if part.ImageData.MediaType != "image/jpeg" {
		t.Fatalf("inline media type = %q, want image/jpeg", part.ImageData.MediaType)
	}
}

func makeLargeJPEG(t *testing.T) []byte {
	t.Helper()

	// Noisy pattern compresses poorly, so 1200x1200 at Q95 stays comfortably
	// over maxLLMImageBytes (the caller asserts this).
	img := image.NewRGBA(image.Rect(0, 0, 1200, 1200))
	for y := 0; y < 1200; y++ {
		for x := 0; x < 1200; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x*17 + y*31) & 0xff),
				G: uint8((x*47 + y*13) & 0xff),
				B: uint8((x*7 + y*19) & 0xff),
				A: 0xff,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("jpeg.Encode() error = %v", err)
	}
	return buf.Bytes()
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
