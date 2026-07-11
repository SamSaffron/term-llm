package run

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

const (
	PlatformConsole  = "console"
	PlatformWeb      = "web"
	PlatformTelegram = "telegram"
	PlatformJob      = "jobs"
	PlatformChat     = "chat"
	PlatformExec     = "exec"
)

// ProgressiveOptions describes an iterative/progressive run. A nil *ProgressiveOptions
// on Request means a normal single execution.
type ProgressiveOptions struct {
	Timeout      time.Duration
	StopWhen     string
	ContinueWith string
}

// ProgressiveResult is the platform-neutral form of the progressive execution
// summary produced by the command-layer runner.
type ProgressiveResult struct {
	ExitReason    string         `json:"exit_reason"`
	Finalized     bool           `json:"finalized"`
	SessionID     string         `json:"session_id,omitempty"`
	Sequence      int            `json:"sequence,omitempty"`
	Reason        string         `json:"reason,omitempty"`
	Message       string         `json:"message,omitempty"`
	Progress      map[string]any `json:"progress,omitempty"`
	FinalResponse string         `json:"final_response,omitempty"`
	FallbackText  string         `json:"fallback_text,omitempty"`
}

// Request is a single LLM execution request. It intentionally carries execution
// semantics (agent, prompt/history, settings overrides, persistence and runtime
// capabilities) but not presentation details; presentation belongs to EventSink
// implementations owned by each platform.
type Request struct {
	Platform string

	AgentName string
	Prompt    string
	Messages  []llm.Message

	// Engine/ProviderInstance let stateful callers (chat/telegram) run through
	// the shared runner while reusing their session-scoped engine/provider/MCP
	// resources instead of rebuilding them every turn. When supplied, the runner
	// treats them as borrowed and does not close provider-owned resources.
	Engine           *llm.Engine
	ProviderInstance llm.Provider

	SessionID    string
	SessionName  string
	Resume       bool
	Persist      bool
	DeferSession bool
	// DisableRuntimePersistence keeps any configured store available for tool
	// wiring (notably spawn_agent) while preventing the shared runtime from
	// writing session rows. Platforms that already own persistence can use their
	// callbacks instead.
	DisableRuntimePersistence bool
	Stateful                  bool
	ReplaceHistory            bool

	Provider string
	Model    string
	Cwd      string

	Tools      string
	ReadDirs   []string
	WriteDirs  []string
	ShellAllow []string
	MCP        string
	Skills     string

	SystemMessage               string
	MaxTurns                    int
	MaxTurnsSet                 bool
	MaxOutputTokens             int
	ServiceTier                 string
	ServiceTierSet              bool
	ContextEstimateTotalTokens  int
	ContextEstimateMessageCount int
	Search                      *bool
	NoSearch                    bool

	Yolo     bool
	Auto     bool
	Debug    bool
	DebugRaw bool

	ForceExternalSearch     *bool
	DisableExternalWebFetch bool
	ExtraTools              []llm.ToolSpec
	ForceToolName           string
	LastTurnForceToolName   string
	IncludeConfiguredTools  *bool

	OnAssistantSnapshot    llm.AssistantSnapshotCallback
	OnResponseCompleted    llm.ResponseCompletedCallback
	OnTurnCompleted        llm.TurnCompletedCallback
	OnCompaction           llm.CompactionCallback
	OnSyntheticUserMessage func(context.Context, llm.Message) error
	OnEngineReady          func(*llm.Engine)
	OnEngineDone           func(*llm.Engine)

	Progressive *ProgressiveOptions

	// Sub-agent/session-linking options used by spawn_agent migrations.
	ParentSessionID          string
	IsSubagent               bool
	Depth                    int
	ApprovalRole             string
	ApprovalTranscriptPrefix []llm.Message
}

// EventSink receives the raw llm.Event stream for a run. Implementations should
// do rendering/translation only; they should not own runner wiring.
type EventSink interface {
	Event(ev llm.Event)
}

// ErrorEventSink is an optional EventSink capability for sinks that need to
// propagate consumer/backpressure failures to stop the producer.
type ErrorEventSink interface {
	EventWithError(ev llm.Event) error
}

// ApprovalPrompter is an optional EventSink capability. Interactive platforms
// implement it; headless platforms such as jobs intentionally do not.
type ApprovalPrompter interface {
	PromptApproval(target string, isWrite, isShell bool, workDir string) (tools.ApprovalResult, error)
}

// AskUserPrompter is an optional EventSink capability for the ask_user tool.
type AskUserPrompter interface {
	AskUser(ctx context.Context, questions []tools.AskUserQuestion) ([]tools.AskUserAnswer, error)
}

// GuardianEventSink is an optional EventSink capability for guardian review
// notices emitted by auto-approval mode.
type GuardianEventSink interface {
	GuardianEvent(event tools.GuardianEvent)
}

// Result summarizes execution independently of platform rendering.
type Result struct {
	SessionID string
	Provider  string
	Model     string
	Response  string
	Thinking  string

	Turns        int
	InputTokens  int
	OutputTokens int

	ExitReason  string
	Progressive *ProgressiveResult

	// Engine is exposed for legacy command UIs that provide post-run affordances
	// (for example exec's command help). New platform code should prefer events
	// and Result fields over reaching into the engine.
	Engine           *llm.Engine
	ProviderInstance llm.Provider
}

// Runner executes one Request and streams events to a sink.
type Runner interface {
	Run(ctx context.Context, req Request, sink EventSink) (Result, error)
}
