package serve

import (
	"io"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	runapproval "github.com/samsaffron/term-llm/internal/app/runtime/approval"
	"github.com/samsaffron/term-llm/internal/config"
)

// FlagState captures every serve flag whose explicitness affects startup.
type FlagState struct {
	Projects               bool
	NoProjects             bool
	ResponseTimeout        bool
	Auth                   bool
	NoAuth                 bool
	BasePath               bool
	Title                  bool
	DisableLocationSharing bool
	MaxTurns               bool
	Tools                  bool
	System                 bool
	Extensions             bool
	EnableWidgets          bool
}

// Invocation contains command-owned values needed by serve startup. It is
// deliberately free of Cobra and listener state so application startup can be
// called without a command object while the host retains its private test seam.
type Invocation struct {
	Flags           FlagState
	Stdout          io.Writer
	Stderr          io.Writer
	ResolveApproval func(*config.Config) (runapproval.ResolvedMode, error)
	DebugRaw        bool
	SessionStore    runtime.SessionStoreOverrides
	MemoryPaths     appmemory.Paths
}

// MCPInvocation contains command-owned values needed by the MCP server body.
// ResolveApproval stays deferred so validation and config loading keep their
// established precedence over parent/child approval arbitration.
type MCPInvocation struct {
	Stdout          io.Writer
	Stderr          io.Writer
	ResolveApproval func(*config.Config) (runapproval.ResolvedMode, error)
}
