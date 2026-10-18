// Package tools provides a permission-aware local tool system for term-llm.
package tools

import (
	"encoding/json"
	"fmt"

	planpkg "github.com/samsaffron/term-llm/internal/plan"
)

// ToolKind categorizes tools for permission grouping.
type ToolKind string

const (
	KindRead         ToolKind = "read"
	KindEdit         ToolKind = "edit"
	KindSearch       ToolKind = "search"
	KindExecute      ToolKind = "execute"
	KindImage        ToolKind = "image"
	KindInteractive  ToolKind = "interactive"
	KindAgent        ToolKind = "agent"         // For spawn_agent tool
	KindSkill        ToolKind = "skill"         // For activate_skill tool
	KindSessionState ToolKind = "session_state" // For session-scoped state such as update_plan
)

// MutatorKinds are tool kinds that can modify the filesystem.
var MutatorKinds = []ToolKind{KindEdit, KindExecute}

// ConfirmOutcome represents the result of a user confirmation prompt.
type ConfirmOutcome string

const (
	ProceedOnce          ConfirmOutcome = "once"        // Single approval
	ProceedAlways        ConfirmOutcome = "always"      // Session-scoped approval
	ProceedAlwaysAndSave ConfirmOutcome = "always_save" // Persist to config
	Cancel               ConfirmOutcome = "cancel"      // User denied
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
	// ErrCancelledByUser distinguishes a deliberate human stop from a generic
	// execution failure. A model that cannot tell them apart retries work the
	// user just asked it to abandon.
	ErrCancelledByUser ToolErrorType = "CANCELLED_BY_USER"
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
	Output      string         `json:"output"`
	Display     *ToolDisplay   `json:"display,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
	Error       *ToolError     `json:"error,omitempty"`
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
	UIGetSourceToolName      = "ui_get_source"
	UIExtensionsToolName     = "ui_extensions"
	UIActivateToolName       = "ui_activate_extensions"
	ReadFileToolName         = "read_file"
	WriteFileToolName        = "write_file"
	EditFileToolName         = "edit_file"
	UnifiedDiffToolName      = "unified_diff"
	ShellToolName            = "shell"
	GrepToolName             = "grep"
	GlobToolName             = "glob"
	ViewImageToolName        = "view_image"
	ShowImageToolName        = "show_image"
	ShowMediaToolName        = "show_media"
	ImageGenerateToolName    = "image_generate"
	AskUserToolName          = "ask_user"
	SpawnAgentToolName       = "spawn_agent"
	QueueAgentToolName       = "queue_agent"
	WaitForJobsToolName      = "wait_for_jobs"
	RunAgentScriptToolName   = "run_agent_script"
	InitiateHandoverToolName = "initiate_handover"
	ManageWorkspaceToolName  = "manage_workspace"
	UpdatePlanToolName       = planpkg.ToolName

	HubDelegateToolName        = "hub_delegate"
	HubCheckDelegationToolName = "hub_check_delegation"
)

// StandardToolNames returns the implicit tool set used by --tools all and disabled-list agents.
func StandardToolNames() []string {
	var names []string
	for _, d := range builtinTools {
		if d.DefaultEnabled && (!d.hubCredentials || HubDelegationConfigured()) {
			names = append(names, d.Name)
		}
	}
	return names
}

// ValidToolNames returns every accepted built-in name, including opt-in tools.
func ValidToolNames() []string {
	names := StandardToolNames()
	for _, d := range builtinTools {
		if !d.nonLocal && !d.hiddenCompletion && !containsBuiltinName(names, d.Name) {
			names = append(names, d.Name)
		}
	}
	return names
}
func containsBuiltinName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// ValidToolName checks if a name is a valid local tool spec name.
func ValidToolName(name string) bool { d := lookupBuiltin(name); return d != nil && !d.nonLocal }

// GetToolKind returns the permission policy class for a tool spec name.
func GetToolKind(name string) ToolKind {
	if d := lookupBuiltin(name); d != nil {
		return d.Policy
	}
	return ""
}
