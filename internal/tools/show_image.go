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
	approval  *ApprovalManager
	config    *ToolConfig
	serveMode bool // When true, strip clipboard param from spec
}

// NewShowImageTool creates a new ShowImageTool.
func NewShowImageTool(approval *ApprovalManager, configs ...*ToolConfig) *ShowImageTool {
	return &ShowImageTool{
		approval: approval,
		config:   optionalToolConfig(configs),
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
	props := map[string]any{
		"file_path": map[string]any{
			"type":        "string",
			"description": "Path to the image file to display",
		},
		"prompt": map[string]any{
			"type":        "string",
			"description": "Optional question or instruction to guide image analysis (e.g., 'What text is visible?' or 'Describe the colors used')",
		},
	}
	desc := "Display an image to the user via terminal (icat) and optionally copy to clipboard."
	if t.serveMode {
		desc = "Display an image to the user."
	} else {
		props["copy_to_clipboard"] = map[string]any{
			"type":        "boolean",
			"description": "Also copy the image to system clipboard (default: true)",
			"default":     true,
		}
	}
	return llm.ToolSpec{
		Name:        ShowImageToolName,
		Description: desc,
		Schema: map[string]any{
			"type":                 "object",
			"properties":           props,
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

func (t *ShowImageTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a ShowImageArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, err.Error()))), nil
	}

	if a.FilePath == "" {
		return llm.TextOutput(formatToolError(NewToolError(ErrInvalidParams, "file_path is required"))), nil
	}

	resolvedPath, err := resolveToolPathWithConfig(a.FilePath, false, t.config)
	if err != nil {
		if toolErr, ok := err.(*ToolError); ok {
			return llm.TextOutput(formatToolError(toolErr)), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrInvalidParams, "cannot resolve path: %v", err))), nil
	}

	// Check permissions via approval manager
	if t.approval != nil {
		outcome, err := t.approval.CheckPathApproval(ShowImageToolName, resolvedPath, a.FilePath, false)
		if err != nil {
			if toolErr, ok := err.(*ToolError); ok {
				return llm.TextOutput(formatToolError(toolErr)), nil
			}
			return llm.TextOutput(formatToolError(NewToolError(ErrPermissionDenied, err.Error()))), nil
		}
		if outcome == Cancel {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrPermissionDenied, "access denied: %s", a.FilePath))), nil
		}
	}

	// Check file exists
	info, err := os.Stat(resolvedPath)
	if err != nil {
		if os.IsNotExist(err) {
			return llm.TextOutput(formatToolError(NewToolError(ErrFileNotFound, a.FilePath))), nil
		}
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "cannot stat file: %v", err))), nil
	}

	// Check format
	ext := strings.ToLower(filepath.Ext(resolvedPath))
	if !showImageSupportedFormats[ext] {
		return llm.TextOutput(formatToolError(NewToolErrorf(ErrUnsupportedFormat, "unsupported format: %s (supported: PNG, JPEG, GIF, WebP, BMP)", ext))), nil
	}

	// Copy to clipboard if requested (default: true, disabled in serve mode)
	copyClipboard := !t.serveMode && (a.CopyToClipboard == nil || *a.CopyToClipboard)
	if copyClipboard {
		// Read file data for clipboard
		data, err := os.ReadFile(resolvedPath)
		if err != nil {
			return llm.TextOutput(formatToolError(NewToolErrorf(ErrExecutionFailed, "failed to read image for clipboard: %v", err))), nil
		}
		image.CopyToClipboard(resolvedPath, data)
	}

	// Build result
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Image: %s\n", a.FilePath))
	sb.WriteString(fmt.Sprintf("Size: %d bytes\n", info.Size()))
	sb.WriteString(fmt.Sprintf("Format: %s\n", strings.TrimPrefix(ext, ".")))
	if copyClipboard {
		sb.WriteString("Copied to clipboard: yes\n")
	}
	if a.Prompt != "" {
		sb.WriteString(fmt.Sprintf("Focus: %s\n", a.Prompt))
	}

	return llm.ToolOutput{
		Content: sb.String(),
		Images:  []string{a.FilePath},
	}, nil
}
