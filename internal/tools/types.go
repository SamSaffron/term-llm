// Package tools provides a permission-aware local tool system for term-llm.
package tools

import (
	"encoding/json"
	"fmt"
)

// ToolKind categorizes tools for permission grouping.
type ToolKind string

const (
	KindRead    ToolKind = "read"
	KindEdit    ToolKind = "edit"
	KindSearch  ToolKind = "search"
	KindExecute ToolKind = "execute"
	KindImage   ToolKind = "image"
)

// MutatorKinds are tool kinds that can modify the filesystem.
var MutatorKinds = []ToolKind{KindEdit, KindExecute}

// ConfirmOutcome represents the result of a user confirmation prompt.
type ConfirmOutcome string

const (
	ProceedOnce          ConfirmOutcome = "once"         // Single approval
	ProceedAlways        ConfirmOutcome = "always"       // Session-scoped approval
	ProceedAlwaysAndSave ConfirmOutcome = "always_save"  // Persist to config
	Cancel               ConfirmOutcome = "cancel"       // User denied
)

// ToolErrorType provides structured errors for agent retry logic.
type ToolErrorType string

const (
	ErrFileNotFound       ToolErrorType = "FILE_NOT_FOUND"
	ErrInvalidParams      ToolErrorType = "INVALID_PARAMS"
	ErrPathNotInWorkspace ToolErrorType = "PATH_NOT_IN_WORKSPACE"
	ErrExecutionFailed    ToolErrorType = "EXECUTION_FAILED"
	ErrPermissionDenied   ToolErrorType = "PERMISSION_DENIED"
	ErrBinaryFile         ToolErrorType = "BINARY_FILE"
	ErrFileTooLarge       ToolErrorType = "FILE_TOO_LARGE"
	ErrImageGenFailed     ToolErrorType = "IMAGE_GEN_FAILED"
	ErrUnsupportedFormat  ToolErrorType = "UNSUPPORTED_FORMAT"
	ErrTimeout            ToolErrorType = "TIMEOUT"
	ErrSymlinkEscape      ToolErrorType = "SYMLINK_ESCAPE"
)

// ToolError provides structured error information for retry logic.
type ToolError struct {
	Type    ToolErrorType `json:"type"`
	Message string        `json:"message"`
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// NewToolError creates a new ToolError.
func NewToolError(errType ToolErrorType, message string) *ToolError {
	return &ToolError{Type: errType, Message: message}
}

// NewToolErrorf creates a new ToolError with formatted message.
func NewToolErrorf(errType ToolErrorType, format string, args ...interface{}) *ToolError {
	return &ToolError{Type: errType, Message: fmt.Sprintf(format, args...)}
}

// ToolDisplay contains optional display metadata for UI.
type ToolDisplay struct {
	Title    string `json:"title,omitempty"`
	Preview  string `json:"preview,omitempty"`
	FilePath string `json:"file_path,omitempty"`
}

// Attachment represents a file attachment in tool results.
type Attachment struct {
	Path     string `json:"path"`
	MimeType string `json:"mime_type,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

// ToolPayload is an optional JSON payload for tools that need display/metadata.
// This is encoded in ToolResult.Content when UI metadata is required.
type ToolPayload struct {
	Output      string            `json:"output"`
	Display     *ToolDisplay      `json:"display,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Attachments []Attachment      `json:"attachments,omitempty"`
	Error       *ToolError        `json:"error,omitempty"`
}

// ToJSON encodes the payload as JSON for use in ToolResult.Content.
func (p *ToolPayload) ToJSON() string {
	data, err := json.Marshal(p)
	if err != nil {
		return p.Output
	}
	return string(data)
}

// ToolMetadata contains execution metrics.
type ToolMetadata struct {
	ExecutionTimeMs   int64 `json:"execution_time_ms"`
	PermissionCheckMs int64 `json:"permission_check_ms,omitempty"`
	OutputBytes       int64 `json:"output_bytes"`
	Truncated         bool  `json:"truncated,omitempty"`
}

// Tool specification names
const (
	ReadFileToolName     = "read_file"
	WriteFileToolName    = "write_file"
	EditFileToolName     = "edit_file"
	ShellToolName        = "shell"
	GrepToolName         = "grep"
	GlobToolName         = "glob"
	ViewImageToolName    = "view_image"
	ImageGenerateToolName = "image_generate"
)

// CLI name to spec name mapping
var ToolNameMapping = map[string]string{
	"read":  ReadFileToolName,
	"write": WriteFileToolName,
	"edit":  EditFileToolName,
	"shell": ShellToolName,
	"grep":  GrepToolName,
	"find":  GlobToolName,
	"view":  ViewImageToolName,
	"image": ImageGenerateToolName,
}

// SpecToCliName returns the CLI name for a tool spec name.
var SpecToCliName = map[string]string{
	ReadFileToolName:      "read",
	WriteFileToolName:     "write",
	EditFileToolName:      "edit",
	ShellToolName:         "shell",
	GrepToolName:          "grep",
	GlobToolName:          "find",
	ViewImageToolName:     "view",
	ImageGenerateToolName: "image",
}

// AllToolNames returns all valid CLI tool names.
func AllToolNames() []string {
	return []string{"read", "write", "edit", "shell", "grep", "find", "view", "image"}
}

// ValidToolName checks if a name is a valid CLI tool name.
func ValidToolName(name string) bool {
	_, ok := ToolNameMapping[name]
	return ok
}

// GetToolKind returns the kind for a tool spec name.
func GetToolKind(specName string) ToolKind {
	switch specName {
	case ReadFileToolName, ViewImageToolName:
		return KindRead
	case WriteFileToolName, EditFileToolName:
		return KindEdit
	case GrepToolName, GlobToolName:
		return KindSearch
	case ShellToolName:
		return KindExecute
	case ImageGenerateToolName:
		return KindImage
	default:
		return ""
	}
}
