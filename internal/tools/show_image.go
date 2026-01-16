package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/samsaffron/term-llm/internal/image"
	"github.com/samsaffron/term-llm/internal/llm"
)

// ShowImageTool implements the show_image tool for displaying images to users.
type ShowImageTool struct {
	approval *ApprovalManager
}

// NewShowImageTool creates a new ShowImageTool.
func NewShowImageTool(approval *ApprovalManager) *ShowImageTool {
	return &ShowImageTool{
		approval: approval,
	}
}

// ShowImageArgs are the arguments for show_image.
type ShowImageArgs struct {
	FilePath        string `json:"file_path"`
	CopyToClipboard *bool  `json:"copy_to_clipboard,omitempty"` // Default: true
	Prompt          string `json:"prompt,omitempty"`            // Steering prompt for image analysis
}

var showImageSupportedFormats = map[string]bool{
	".png":  true,
	".jpg":  true,
	".jpeg": true,
	".gif":  true,
	".webp": true,
	".bmp":  true,
}

func (t *ShowImageTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        ShowImageToolName,
		Description: "Display an image to the user via terminal (icat) and optionally copy to clipboard.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"file_path": map[string]any{
					"type":        "string",
					"description": "Path to the image file to display",
				},
				"copy_to_clipboard": map[string]any{
					"type":        "boolean",
					"description": "Also copy the image to system clipboard (default: true)",
					"default":     true,
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "Optional question or instruction to guide image analysis (e.g., 'What text is visible?' or 'Describe the colors used')",
				},
			},
			"required":             []string{"file_path"},
			"additionalProperties": false,
		},
	}
}

func (t *ShowImageTool) Preview(args json.RawMessage) string {
	var a ShowImageArgs
	if err := json.Unmarshal(args, &a); err != nil || a.FilePath == "" {
		return ""
	}
	return a.FilePath
}

func (t *ShowImageTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a ShowImageArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return formatToolError(NewToolError(ErrInvalidParams, err.Error())), nil
	}

	if a.FilePath == "" {
		return formatToolError(NewToolError(ErrInvalidParams, "file_path is required")), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ShowImageToolName, a.FilePath, a.FilePath, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return formatToolError(toolErr), nil
			}
			return formatToolError(NewToolError(ErrPermissionDenied, err.Error())), nil
		}
		if outcome == Cancel {
			return formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.FilePath)), nil
		}
	}

	// Check file exists
	info, err := os.Stat(a.FilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return formatToolError(NewToolError(ErrFileNotFound, a.FilePath)), nil
		}
		return formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot stat file: %v", err)), nil
	}

	// Check format
	ext := strings.ToLower(filepath.Ext(a.FilePath))
	if !showImageSupportedFormats[ext] {
		return formatToolError(NewToolErrorf(ErrUnsupportedFormat, "unsupported format: %s (supported: PNG, JPEG, GIF, WebP, BMP)", ext)), nil
	}

	// Display image
	image.DisplayImage(a.FilePath)

	// Copy to clipboard if requested (default: true)
	copyClipboard := a.CopyToClipboard == nil || *a.CopyToClipboard
	if copyClipboard {
		// Read file data for clipboard
		data, err := os.ReadFile(a.FilePath)
		if err != nil {
			return formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to read image for clipboard: %v", err)), nil
		}
		image.CopyToClipboard(a.FilePath, data)
	}

	// Build result
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Displayed image: %s\n", a.FilePath))
	sb.WriteString(fmt.Sprintf("Size: %d bytes\n", info.Size()))
	sb.WriteString(fmt.Sprintf("Format: %s", strings.TrimPrefix(ext, ".")))
	if copyClipboard {
		sb.WriteString("\nCopied to clipboard: yes")
	}
	if a.Prompt != "" {
		sb.WriteString(fmt.Sprintf("\nFocus: %s", a.Prompt))
	}

	return sb.String(), nil
}
