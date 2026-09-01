package run

import (
	"context"
	"time"

	"github.com/samsaffron/term-llm/internal/skills"
	"github.com/samsaffron/term-llm/internal/tools"
)

// ChildRunKind identifies why a fresh child runtime was started.
type ChildRunKind string

const (
	ChildRunSpawnAgent    ChildRunKind = "spawn_agent"
	ChildRunIsolatedSkill ChildRunKind = "isolated_skill"
	ChildRunCommitScope   ChildRunKind = "commit_scope"
	ChildRunCommitDraft   ChildRunKind = "commit_draft"
)

// SkillRunMetadata carries the resolved activation into a child engine without
// requiring the child model to rediscover or activate the skill.
type SkillRunMetadata struct {
	Name                string
	Source              string
	SourcePath          string
	RawArguments        string
	AllowedTools        []string
	AllowedToolsPresent bool
	ToolDefs            []skills.SkillToolDef
	Resources           []string
}

type HostOutputTool struct {
	Name        string
	Param       string
	Description string
	Schema      map[string]interface{}
}

// ChildRunRequest is the shared child-runtime contract used by spawn_agent and
// direct isolated skill invocation.
type ChildRunRequest struct {
	Kind            ChildRunKind
	RunID           string
	ChildSessionID  string
	AgentName       string
	Prompt          string
	ModelOverride   string
	ParentSessionID string
	BaseDir         string
	Depth           int
	Skill           *SkillRunMetadata

	// Specialized host-owned child runs use these overlays without mutating the
	// registry's shared agent definition.
	SkipOnComplete   bool
	MaxTurnsOverride int
	SystemSuffix     string
	DisableTools     bool
	OutputTool       *HostOutputTool
}

// ChildRunResult identifies the durable child session and collected output.
type ChildRunResult struct {
	RunID          string
	ChildSessionID string
	Output         string
	Provider       string
	Model          string
	StartedAt      time.Time
	CompletedAt    time.Time
}

type ChildAgentMetadata struct {
	Name   string
	Source string
}

type ChildAgentResolver interface {
	ResolveChildAgent(name string) (ChildAgentMetadata, error)
}

// ChildRunEventCallback receives structured child progress keyed by the direct
// run ID rather than a fabricated parent tool-call ID.
type ChildRunEventCallback func(runID string, event tools.SubagentEvent)

// ChildRunner executes a fresh child runtime synchronously. Callers normally
// invoke it from their own goroutine/tea.Cmd and cancel through ctx.
type ChildRunner interface {
	RunChild(ctx context.Context, request ChildRunRequest, callback ChildRunEventCallback) (ChildRunResult, error)
}
