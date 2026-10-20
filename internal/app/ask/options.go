package ask

import (
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/debug"
	"io"
	"time"
)

// Options is the invocation-scoped snapshot of ask flags.
type Options struct {
	Debug               bool
	Search              bool
	NoSearch            bool
	Text                bool
	Porcelain           bool
	JSON                bool
	Progressive         bool
	Provider            string
	Files               []string
	MCP                 string
	MaxTurns            int
	MaxOutputTokens     int
	InlineMaxBytes      string
	Timeout             time.Duration
	StopWhen            string
	ContinueWith        string
	NativeSearch        bool
	NoNativeSearch      bool
	NoWebFetch          bool
	Tools               string
	ReadDirs            []string
	WriteDirs           []string
	ShellAllow          []string
	SystemMessage       string
	Agent               string
	Approval            string
	Yolo                bool
	Auto                bool
	Fast                bool
	Skills              string
	Resume              string
	DebugRaw, ShowStats bool
	Changed             FlagState
}

// FlagState preserves explicit false/empty flags, including the historical system lookup.
type FlagState struct{ Resume, Tools, System, MaxTurns, Search, MCP bool }

// Dependencies are captured CLI inputs and deferred startup operations. Approval
// is resolved after progressive validation and config loading, not during capture.
type Dependencies struct {
	Stdin                io.Reader
	HasStdin             bool
	Stdout, Stderr       io.Writer
	Interactive          func() bool
	SessionStore         appruntime.SessionStoreOverrides
	MemoryPaths          appmemory.Paths
	LoadConfig           func() (*config.Config, error)
	ResolveApproval      func(*config.Config) (approval.ResolvedMode, error)
	NewProvider          func(*config.Config, bool) (core.Provider, error)
	CreateDebugLogger    func(*config.Config) (*debug.Logger, error)
	RunnerCleanupTimeout time.Duration
}
