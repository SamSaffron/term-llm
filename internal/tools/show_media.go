package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mediatype"
)

// MediaPublisher imports approved local media into storage owned by a
// presentation surface. Serve runtimes use this to make media durable and safe
// to publish without granting the HTTP layer arbitrary filesystem access.
type MediaPublisher interface {
	PublishMedia(ctx context.Context, sourcePath, mediaType string) (string, error)
}

// ShowMediaTool presents an existing local image, video, or audio to the user.
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
		Description: "Register an existing image, video, or audio so it can be placed in your response. The result returns an opaque " +
			"term-llm-media URL; embed that exact URL on its own line with Markdown image syntax and choose context-appropriate alt text. " +
			"Use this when the user should actually see or listen to the media, not merely when you need to inspect or analyze it. " +
			"Supports PNG, JPEG, GIF, WebP, BMP, MP4, WebM, MP3, WAV, Ogg Opus/Vorbis, FLAC, M4A, and audio-only WebM; images up to 25 MiB, videos up to 256 MiB, audio up to 100 MiB.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Path to an existing image, video, or audio to show to the user",
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

	if err := ctx.Err(); err != nil {
		return llm.ToolOutput{}, err
	}
	format, inspectErr := mediatype.Inspect(f, info.Size())
	if inspectErr != nil {
		if errors.Is(inspectErr, mediatype.ErrUnsupported) {
			return llm.TextOutput(formatToolError(NewToolError(ErrUnsupportedFormat,
				"unsupported media format (supported: PNG, JPEG, GIF, WebP, BMP, MP4, WebM, MP3, WAV, Ogg Opus/Vorbis, FLAC, M4A, audio-only WebM)"))), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot inspect file: %v", inspectErr))), nil
	}
	if err := ctx.Err(); err != nil {
		return llm.ToolOutput{}, err
	}
	mediaType := format.MIME
	if info.Size() > format.MaxBytes {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams,
			"%s is %.1f MiB; show_media supports %s files up to %d MiB", filepath.Base(a.Path), float64(info.Size())/(1<<20), format.Kind, format.MaxBytes>>20))), nil
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
	fmt.Fprintf(&result, "Media ready (%s).\n\n", mediaType)
	result.WriteString("To embed it in your response, use this exact URL in Markdown image syntax, even for audio:\n\n")
	fmt.Fprintf(&result, "%s\n\n", uri)
	result.WriteString("Choose concise, descriptive alt text appropriate to the surrounding response (for example, “Listen to the narrated summary”), and place the Markdown on its own line. Do not substitute a local path or write an HTML audio tag.\n")
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
