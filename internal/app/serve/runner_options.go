package serve

import (
	"io"

	appmemory "github.com/term-llm/term-llm/internal/app/memory"
	"github.com/term-llm/term-llm/internal/app/runtime"
	runapproval "github.com/term-llm/term-llm/internal/app/runtime/approval"
	"github.com/term-llm/term-llm/internal/app/runtime/mcpstart"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/tools"
)

type cmdRunnerOptions struct {
	RestoreSettings    *SessionSettings
	RestoreAgentSkills *string
	Inputs             *SessionInputSelection
	ToolsSet           bool
	SystemMessageSet   bool
	Provider           string
	Fast               bool
	ConfigSet          bool
	ConfigProvider     string
	ConfigModel        string
	ConfigInstructions string
	ConfigMaxTurns     int
	Tools              string
	ReadDirs           []string
	WriteDirs          []string
	ShellAllow         []string
	MCP                string
	LoadMCPConfig      mcpstart.ConfigLoader
	SystemMessage      string
	MaxTurns           int
	DefaultMaxTurns    int
	Search             bool
	NoSearch           bool

	NativeSearch   bool
	NoNativeSearch bool

	Yolo                bool // Deprecated compatibility input; prefer ApprovalMode.
	Auto                bool // Deprecated compatibility input; prefer ApprovalMode.
	ApprovalMode        tools.ApprovalMode
	ApprovalModeSet     bool
	ApprovalSource      runapproval.ModeSource
	ApprovalHeadless    bool
	ApprovalPrepare     bool
	ApprovalDiagnostics bool
	Debug               bool
	DebugRaw            bool

	ErrWriter         io.Writer
	WireSpawn         func(*config.Config, *tools.ToolManager, bool) error
	Store             session.Store
	SessionStore      runtime.SessionStoreOverrides
	MemoryPaths       appmemory.Paths
	ParentApprovalMgr *tools.ApprovalManager
	// ChildRunObserver is the host's view of delegated runs. It must propagate
	// into nested runners, because a runner built for a child knows nothing
	// about serve otherwise.
	ChildRunObserver childRunObserver
	AgentOwner       *runtime.AgentHostOwner
}
