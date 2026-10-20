package chat

import (
	"io"
	"os"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/debug"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// Options captures chat flag values without retaining Cobra state.
type Options struct {
	Debug          bool
	Search         bool
	NoSearch       bool
	Provider       string
	MCP            string
	MaxTurns       int
	NativeSearch   bool
	NoNativeSearch bool
	NoWebFetch     bool
	Tools          string
	ReadDirs       []string
	WriteDirs      []string
	ShellAllow     []string
	SystemMessage  string
	Agent          string
	Skills         string
	Resume         string
	Approval       string
	Yolo           bool
	AutoApproval   bool
	AutoSend       []string
	TextMode       bool
	DebugRaw       bool
	Changed        FlagState
}

// FlagState includes the historical system lookup, not system-message.
type FlagState struct{ Resume, Tools, System, MaxTurns, Search bool }

// Dependencies captures terminal/reload authority and deferred startup operations.
// ResolveApproval is called after config, session and settings resolution, with
// the persisted policy and any actual in-process handover carry supplied then.
type Dependencies struct {
	Stdout, Stderr                                   io.Writer
	SessionStore                                     runtime.SessionStoreOverrides
	MemoryPaths                                      appmemory.Paths
	LoadConfig                                       func() (*config.Config, error)
	ResolveApproval                                  func(*config.Config, *session.SessionApprovalMode, *tools.ApprovalMode) (approval.ResolvedMode, error)
	CreateDebugLogger                                func(*config.Config) (*debug.Logger, error)
	StartDiagnostics                                 func(io.Writer) (func(), error)
	Interactive, OutputInteractive, StdoutIsTerminal func() bool
	OpenTTY                                          func() (*os.File, *os.File, error)
	RestoreReloadState                               func(string, any) (bool, error)
	ExecReload                                       func(string) error
}
