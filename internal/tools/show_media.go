package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
)

const (
	maxShowMediaImageBytes int64 = 25 << 20
	maxShowMediaVideoBytes int64 = 256 << 20
)

// MediaPublisher imports approved local media into storage owned by a
// presentation surface. Serve runtimes use this to make media durable and safe
// to publish without granting the HTTP layer arbitrary filesystem access.
type MediaPublisher interface {
	PublishMedia(ctx context.Context, sourcePath, mediaType string) (string, error)
}

// ShowMediaTool presents an existing local image or video to the user.
type ShowMediaTool struct {
	approval    *ApprovalManager
	config      *ToolConfig
	publisherMu sync.RWMutex
	publisher   MediaPublisher
}

func NewShowMediaTool(approval *ApprovalManager, configs ...*ToolConfig) *ShowMediaTool {
	return &ShowMediaTool{approval: approval, config: optionalToolConfig(configs)}
}

func (t *ShowMediaTool) SetPublisher(publisher MediaPublisher) {
	t.publisherMu.Lock()
	defer t.publisherMu.Unlock()
	t.publisher = publisher
}

func (t *ShowMediaTool) mediaPublisher() MediaPublisher {
	t.publisherMu.RLock()
	defer t.publisherMu.RUnlock()
	return t.publisher
}

type ShowMediaArgs struct {
	Path    string `json:"path"`
	Caption string `json:"caption,omitempty"`
}

func (t *ShowMediaTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: ShowMediaToolName,
		Description: "Register an existing image or video so it can be placed in your response. The result returns an opaque " +
			"term-llm-media URL; embed that exact URL on its own line with Markdown image syntax and choose context-appropriate alt text. " +
			"Use this when the user should actually see the media, not merely when you need to inspect or analyze it. " +
			"Supports PNG, JPEG, GIF, WebP, BMP, MP4, and WebM; images may be up to 25 MiB and videos up to 256 MiB.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to an existing image or video to show to the user",
				},
				"caption": map[string]any{
					"type":        "string",
					"description": "Optional short caption shown with the media",
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func (t *ShowMediaTool) Preview(args json.RawMessage) string {
	var a ShowMediaArgs
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	return a.Path
}

func (t *ShowMediaTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a ShowMediaArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}
	if strings.TrimSpace(a.Path) == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "path is required"))), nil
	}
	resolved, err := resolveToolPathWithConfig(a.Path, false, t.config)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return llm.TextOutput(formatToolError(toolErr)), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err))), nil
	}
	if t.approval != nil {
		outcome, approvalErr := t.approval.CheckPathApprovalWithContext(ctx, ShowMediaToolName, resolved, a.Path, false)
		if approvalErr != nil {
			return pathApprovalErrorOutput("", approvalErr), nil
		}
		if outcome == Cancel {
			return pathApprovalErrorOutput("", NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.Path)), nil
		}
	}

	f, err := os.Open(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return llm.TextOutput(formatToolError(NewToolError(ErrFileNotFound, a.Path))), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot open file: %v", err))), nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot stat file: %v", err))), nil
	}
	if !info.Mode().IsRegular() {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "path must be a regular file"))), nil
	}

	header := make([]byte, 512)
	n, readErr := io.ReadFull(f, header)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot inspect file: %v", readErr))), nil
	}
	mediaType := detectShowMediaType(header[:n])
	if mediaType == "" {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrUnsupportedFormat,
			"unsupported media format (supported: PNG, JPEG, GIF, WebP, BMP, MP4, WebM)"))), nil
	}
	limit := maxShowMediaImageBytes
	kind := "Image"
	if strings.HasPrefix(mediaType, "video/") {
		limit = maxShowMediaVideoBytes
		kind = "Video"
	}
	if info.Size() > limit {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams,
			"%s is %.1f MiB; show_media supports %ss up to %d MiB", filepath.Base(a.Path), float64(info.Size())/(1<<20), strings.ToLower(kind), limit>>20))), nil
	}

	reference, err := newMediaReference()
	if err != nil {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot create media reference: %v", err))), nil
	}
	storedPath := ""
	if publisher := t.mediaPublisher(); publisher != nil {
		storedPath, err = publisher.PublishMedia(ctx, resolved, mediaType)
		if err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot publish media: %v", err))), nil
		}
		if strings.TrimSpace(storedPath) == "" {
			return llm.TextOutput(formatToolError(NewToolError(ErrExecutionFailed, "media publisher returned no stored path"))), nil
		}
	}

	name := filepath.Base(a.Path)
	caption := sanitizeMediaCaption(a.Caption)
	uri := "term-llm-media://" + reference
	var result strings.Builder
	result.WriteString("Media ready.\n\n")
	result.WriteString("To embed it in your response, use this exact URL in Markdown:\n\n")
	fmt.Fprintf(&result, "%s\n\n", uri)
	result.WriteString("Choose concise, descriptive alt text appropriate to the surrounding response, and place the Markdown on its own line.\n")
	fmt.Fprintf(&result, "Example: ![descriptive alt text](%s)\n", uri)
	if caption != "" {
		fmt.Fprintf(&result, "Suggested alt text: %s\n", caption)
	}
	return llm.ToolOutput{
		Content: result.String(),
		Media: []llm.MediaArtifact{{
			Reference:  reference,
			SourcePath: resolved,
			StoredPath: storedPath,
			MediaType:  mediaType,
			Name:       name,
			Caption:    caption,
		}},
	}, nil
}

func newMediaReference() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func sanitizeMediaCaption(value string) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, value))
	if len(value) > 500 {
		value = value[:500]
		for len(value) > 0 && !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.Join(strings.Fields(value), " ")
}

func isMP4Brand(brand []byte) bool {
	if len(brand) != 4 {
		return false
	}
	switch string(brand) {
	case "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "iso7", "iso8", "iso9",
		"mp41", "mp42", "avc1", "dash", "M4V ", "MSNV":
		return true
	default:
		return false
	}
}

func hasWebMDocType(data []byte) bool {
	for offset := 0; offset+3 <= len(data); offset++ {
		if data[offset] != 0x42 || data[offset+1] != 0x82 {
			continue
		}
		first := data[offset+2]
		width := 1
		for mask := byte(0x80); width <= 8 && first&mask == 0; mask >>= 1 {
			width++
		}
		if width > 8 || offset+2+width > len(data) {
			continue
		}
		length := uint64(first & (0xff >> width))
		for i := 1; i < width; i++ {
			length = length<<8 | uint64(data[offset+2+i])
		}
		start := offset + 2 + width
		if length == 4 && start+4 <= len(data) && bytes.Equal(data[start:start+4], []byte("webm")) {
			return true
		}
	}
	return false
}

// DetectShowMediaType returns a browser-safe media type for supported magic
// bytes, or an empty string when the data is not on show_media's allowlist.
func DetectShowMediaType(data []byte) string { return detectShowMediaType(data) }

func detectShowMediaType(data []byte) string {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp"
	case len(data) >= 2 && bytes.Equal(data[:2], []byte("BM")):
		return "image/bmp"
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")) && isMP4Brand(data[8:12]):
		return "video/mp4"
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0x1a, 0x45, 0xdf, 0xa3}) && hasWebMDocType(data):
		return "video/webm"
	default:
		// DetectContentType is deliberately only a conservative fallback for
		// signatures already represented by a browser-safe allowlist.
		detected := strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0])
		switch detected {
		case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp":
			return detected
		}
		return ""
	}
}
