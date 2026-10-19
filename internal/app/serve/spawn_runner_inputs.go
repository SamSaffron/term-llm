package serve

import (
	"strings"

	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/app/runtime"
)

// SetRuntimeInputs stores invocation-local persistence and memory selections for
// child and nested runners. Callers set these before exposing the runner.
func (r *SpawnAgentRunner) SetRuntimeInputs(sessionStore runtime.SessionStoreOverrides, paths appmemory.Paths) {
	if r == nil {
		return
	}
	r.sessionStore = sessionStore
	r.memoryPaths = paths
}

// SetBaseDir sets the fallback per-session BaseDir inherited by spawned agents.
func (r *SpawnAgentRunner) SetBaseDir(dir string) {
	if r != nil {
		r.parentBaseDir = strings.TrimSpace(dir)
	}
}

// SetBaseDirFunc sets a resolver for the parent's current BaseDir. Interactive
// sessions can switch worktrees after the runner is wired, so a copied path is
// not sufficient. It must be configured before the runner is installed on a
// SpawnAgentTool; the resolver itself must be safe for concurrent calls.
func (r *SpawnAgentRunner) SetBaseDirFunc(fn func() string) {
	if r != nil {
		r.parentBaseDirFunc = fn
	}
}

func (r *SpawnAgentRunner) currentBaseDir() string {
	if r != nil && r.parentBaseDirFunc != nil {
		if dir := strings.TrimSpace(r.parentBaseDirFunc()); dir != "" {
			return dir
		}
	}
	if r == nil {
		return ""
	}
	return strings.TrimSpace(r.parentBaseDir)
}
