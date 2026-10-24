package uploads

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ParseReferencePart resolves a strict input_upload part from trusted metadata.
func ParseReferencePart(part map[string]json.RawMessage) (core.Part, error) {
	for key := range part {
		if key != "type" && key != "upload_id" {
			return core.Part{}, fmt.Errorf("%w: input_upload accepts only upload_id", ErrInvalidUpload)
		}
	}
	ref, path, raw, err := ResolveReference(partString(part["upload_id"]))
	if err != nil {
		return core.Part{}, err
	}
	if !IsLLMImageType(ref.MediaType) {
		return FilePart(ref.Filename, ref.MediaType, path, raw, base64.StdEncoding.EncodeToString(raw)), nil
	}
	// DecodeConfig only, before any full image decode. Ref mode cannot pass an
	// invalid/huge image straight to the model when resizing refuses to decode.
	width, height := ImageDisplayDimensions(raw)
	if width <= 0 || height <= 0 || width > MaxImagePixels || height > MaxImagePixels/width {
		return core.Part{}, fmt.Errorf("%w: image dimensions are invalid or exceed %d pixels", ErrInvalidUpload, MaxImagePixels)
	}
	return ImagePart(raw, ref.MediaType, path), nil
}

// FilePart builds native and provider-safe fallback representations of a stored original.
func FilePart(filename, mediaType, path string, raw []byte, b64 string) core.Part {
	return core.Part{
		Type: core.PartFile, FilePath: path,
		Text:     FallbackText(filename, mediaType, path, raw),
		FileData: &core.ToolFileData{MediaType: mediaType, Base64: b64, Filename: filename, SizeBytes: int64(len(raw))},
	}
}

// ValidateAttachmentCount checks one message before resolving or saving any
// attachments. Historical messages retain independent limits, like legacy input.
func ValidateAttachmentCount(content json.RawMessage) error {
	var parts []map[string]json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return nil
	}
	count := 0
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(partString(part["type"]))) {
		case "input_upload", "input_image", "image_url", "input_file":
			count++
		}
	}
	if count > MaxAttachments {
		return fmt.Errorf("too many attachments (max %d)", MaxAttachments)
	}
	return nil
}

// ParseErrorType classifies bad references consistently across API formats.
func ParseErrorType(err error) string {
	errorType := "invalid_request_error"
	if errors.Is(err, ErrUploadNotFound) {
		errorType = "upload_not_found"
	} else if errors.Is(err, ErrInvalidUpload) {
		errorType = "invalid_upload"
	}
	return errorType
}

func partString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

// ImagePart preprocesses the model payload while preserving the original path
// and original, EXIF-aware display dimensions. Inline uploads intentionally keep
// their legacy invalid-image fallback; ParseReferencePart validates config first.
func ImagePart(raw []byte, mediaType, path string) core.Part {
	width, height := ImageDisplayDimensions(raw)
	sendRaw, sendMT := raw, mediaType
	if len(raw) > MaxLLMImageBytes {
		sendRaw, sendMT = ResizeImageForLLM(raw, mediaType)
	}
	return core.Part{Type: core.PartImage, ImagePath: path, ImageData: &core.ToolImageData{MediaType: sendMT, Base64: base64.StdEncoding.EncodeToString(sendRaw), Width: width, Height: height}}
}
