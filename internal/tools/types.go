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
	ReadFileToolName         = "read_file"
	WriteFileToolName        = "write_file"
	EditFileToolName         = "edit_file"
	UnifiedDiffToolName      = "unified_diff"
	ShellToolName            = "shell"
	GrepToolName             = "grep"
	GlobToolName             = "glob"
	ViewImageToolName        = "view_image"
	ShowImageToolName        = "show_image"
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

// StandardToolNames returns the implicit tool set used by --tools all and
// disabled-list agents. Behavior-changing opt-in tools are intentionally absent.
// Hub delegation tools are included only when this process has Hub delegation
// credentials, so implicit tool lists do not advertise inoperable hub tools.
// Note: activate_skill is excluded as it requires a skills registry and is registered separately.
func StandardToolNames() []string {
	names := []string{
		ReadFileToolName,
		WriteFileToolName,
		EditFileToolName,
		UnifiedDiffToolName,
		ShellToolName,
		GrepToolName,
		GlobToolName,
		ViewImageToolName,
		ShowImageToolName,
		ImageGenerateToolName,
		AskUserToolName,
		SpawnAgentToolName,
		QueueAgentToolName,
		WaitForJobsToolName,
		RunAgentScriptToolName,
		InitiateHandoverToolName,
	}
	if HubDelegationConfigured() {
		names = append(names, HubDelegateToolName, HubCheckDelegationToolName)
	}
	return names
}

// ValidToolNames returns every accepted built-in tool name for validation and
// shell completion. Hub names remain valid even on standalone nodes so explicit
// configuration behaves consistently with the historical allowlist.
func ValidToolNames() []string {
	names := StandardToolNames()
	for _, name := range []string{HubDelegateToolName, HubCheckDelegationToolName, ManageWorkspaceToolName, UpdatePlanToolName} {
		found := false
		for _, existing := range names {
			if existing == name {
				found = true
				break
			}
		}
		if !found {
			names = append(names, name)
		}
	}
	return names
}

// validToolNames is a set of valid tool spec names for fast lookup.
// Note: activate_skill is excluded as it requires a skills registry and is registered separately.
var validToolNames = map[string]bool{
	ReadFileToolName:           true,
	WriteFileToolName:          true,
	EditFileToolName:           true,
	UnifiedDiffToolName:        true,
	ShellToolName:              true,
	GrepToolName:               true,
	GlobToolName:               true,
	ViewImageToolName:          true,
	ShowImageToolName:          true,
	ImageGenerateToolName:      true,
	AskUserToolName:            true,
	SpawnAgentToolName:         true,
	QueueAgentToolName:         true,
	WaitForJobsToolName:        true,
	RunAgentScriptToolName:     true,
	InitiateHandoverToolName:   true,
	ManageWorkspaceToolName:    true,
	UpdatePlanToolName:         true,
	HubDelegateToolName:        true,
	HubCheckDelegationToolName: true,
}

// ValidToolName checks if a name is a valid tool spec name.
func ValidToolName(name string) bool {
	return validToolNames[name]
}

// GetToolKind returns the kind for a tool spec name.
func GetToolKind(specName string) ToolKind {
	switch specName {
	case ReadFileToolName, ViewImageToolName:
		return KindRead
	case WriteFileToolName, EditFileToolName, UnifiedDiffToolName:
		return KindEdit
	case GrepToolName, GlobToolName:
		return KindSearch
	case ShellToolName:
		return KindExecute
	case ImageGenerateToolName, ShowImageToolName:
		return KindImage
	case AskUserToolName:
		return KindInteractive
	case SpawnAgentToolName, QueueAgentToolName, WaitForJobsToolName, InitiateHandoverToolName,
		HubDelegateToolName, HubCheckDelegationToolName:
		return KindAgent
	case RunAgentScriptToolName:
		return KindExecute
	case UpdatePlanToolName, ManageWorkspaceToolName:
		return KindSessionState
	case ActivateSkillToolName:
		return KindSkill
	default:
		return ""
	}
}
