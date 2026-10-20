package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/agents"
	appmemory "github.com/samsaffron/term-llm/internal/app/memory"
	"github.com/samsaffron/term-llm/internal/app/runtime"
	runapproval "github.com/samsaffron/term-llm/internal/app/runtime/approval"
	bootstrap "github.com/samsaffron/term-llm/internal/app/runtime/bootstrap"
	progressive "github.com/samsaffron/term-llm/internal/app/runtime/progressive"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/modelpolicy"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type serveJobsExecResult struct {
	Progressive *progressive.RunResult
}

type serveJobsExecutor func(ctx context.Context, cfg JobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error)

type jobsV2LLMRunner struct {
	exec serveJobsExecutor
}

type jobsV2NotifyOrigin struct {
	Origin         string `json:"origin,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	TelegramChatID int64  `json:"telegram_chat_id,omitempty"`
}

type JobsV2LLMConfig struct {
	AgentName      string              `json:"agent_name"`
	ModelPolicy    *modelpolicy.Policy `json:"model_policy,omitempty"`
	ParentModel    *runpkg.ParentModel `json:"parent_model,omitempty"`
	Instructions   string              `json:"instructions"`
	Progressive    bool                `json:"progressive,omitempty"`
	StopWhen       string              `json:"stop_when,omitempty"`
	ContinueWith   string              `json:"continue_with,omitempty"`
	PersistSession *bool               `json:"persist_session,omitempty"`
	SessionID      string              `json:"session_id,omitempty"`
	SessionName    string              `json:"session_name,omitempty"`
	NotifyWhenDone bool                `json:"notify_when_done,omitempty"`
	NotifyOrigin   *jobsV2NotifyOrigin `json:"notify_origin,omitempty"`

	// cwd is REQUIRED: it roots this run's file/shell tools at a directory so a
	// job never silently inherits the jobs server's process working directory.
	// It is NOT an os.Chdir (see resolveJobLLMSettings) — the server runs many
	// runs concurrently in one process.
	Cwd string `json:"cwd"`

	// Ask-compatible execution overrides. All optional; omitting a field preserves
	// the previous behavior (it falls back to the serve-level flag / agent / config).
	Provider        string   `json:"provider,omitempty"`          // e.g. "chatgpt" or "chatgpt:gpt-5.5-xhigh"
	Model           string   `json:"model,omitempty"`             // model override (alt to provider:model)
	ReadDir         []string `json:"read_dir,omitempty"`          // additional read roots
	WriteDir        []string `json:"write_dir,omitempty"`         // additional write roots
	Tools           string   `json:"tools,omitempty"`             // tool set override ("all" or csv)
	MaxTurns        int      `json:"max_turns,omitempty"`         // agentic turn cap
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"` // 0 = provider default
	Search          bool     `json:"search,omitempty"`
	SystemMessage   string   `json:"system_message,omitempty"`
	Skills          string   `json:"skills,omitempty"`
}

func (c JobsV2LLMConfig) sessionPersistenceEnabled() bool {
	if c.PersistSession == nil {
		return true
	}
	return *c.PersistSession
}

func (c JobsV2LLMConfig) effectiveSessionID() string {
	if id := strings.TrimSpace(c.SessionID); id != "" {
		return id
	}
	return session.NewID()
}

func (r *jobsV2LLMRunner) Run(ctx context.Context, job jobsV2Job, pw progressWriter) (jobsV2RunResult, error) {
	if r.exec == nil {
		return jobsV2RunResult{}, fmt.Errorf("llm runner is not configured")
	}
	var cfg JobsV2LLMConfig
	if err := json.Unmarshal(job.RunnerConfig, &cfg); err != nil {
		return jobsV2RunResult{}, fmt.Errorf("invalid llm runner config: %w", err)
	}
	if strings.TrimSpace(cfg.AgentName) == "" {
		return jobsV2RunResult{}, fmt.Errorf("llm runner agent_name is required")
	}
	if strings.TrimSpace(cfg.Instructions) == "" {
		return jobsV2RunResult{}, fmt.Errorf("llm runner instructions is required")
	}
	// cwd is mandatory: an llm job must declare where its file/shell tools are
	// rooted rather than silently inheriting the jobs server's working directory.
	if strings.TrimSpace(cfg.Cwd) == "" {
		return jobsV2RunResult{}, fmt.Errorf("llm runner cwd is required")
	}
	cfg.SessionID = cfg.effectiveSessionID()
	if strings.TrimSpace(cfg.SessionName) == "" {
		cfg.SessionName = jobsV2SessionName(job)
	}
	progressiveOpts := progressive.AskOptions{
		Enabled:      cfg.Progressive,
		Timeout:      time.Duration(job.TimeoutSeconds) * time.Second,
		StopWhen:     progressive.StopWhen(strings.TrimSpace(cfg.StopWhen)),
		ContinueWith: cfg.ContinueWith,
	}
	if err := progressive.ValidateAskOptions(&progressiveOpts); err != nil {
		return jobsV2RunResult{}, err
	}
	// Write resolved defaults back so the exec closure can use cfg directly.
	cfg.StopWhen = string(progressiveOpts.StopWhen)
	cfg.ContinueWith = progressiveOpts.ContinueWith
	res := jobsV2RunResult{SessionID: cfg.SessionID}
	// Delegated hub jobs carry their delegation id as a hub-written label;
	// surface it on the context so hub_delegate chains depth/loop tracking
	// without trusting the model to pass parent_delegation_id.
	if delegationID := hubDelegationIDFromJobLabels(job.Labels); delegationID != "" {
		ctx = tools.WithHubDelegationID(ctx, delegationID)
	}
	var output runtime.RunnerOutput
	progressTracker := progressive.NewTracker()
	execResult, err := r.exec(ctx, cfg, func(ev core.Event) {
		if ev.Type != core.EventTextDelta || !cfg.Progressive {
			output.Event(ev)
		}
		switch ev.Type {
		case core.EventToolCall:
			if ev.Tool != nil && cfg.Progressive && progressive.IsProgressTool(ev.Tool.Name) {
				progressTracker.ObserveToolCall(strings.TrimSpace(ev.Tool.ID), strings.TrimSpace(ev.Tool.Name), ev.Tool.Arguments)
			}
		case core.EventUsage:
			if ev.Use != nil {
				res.InputTokens += ev.Use.InputTokens
				res.OutputTokens += ev.Use.OutputTokens
				res.TurnCount++
				// Flush accumulated response to DB after each turn so callers can see partial output.
				if pw != nil {
					if !cfg.Progressive {
						pw("response_flush", output.Response(), nil)
					}
					pw("turn_complete", fmt.Sprintf("turn %d complete (%d in, %d out tokens)", res.TurnCount, ev.Use.InputTokens, ev.Use.OutputTokens), map[string]any{
						"turn":          res.TurnCount,
						"input_tokens":  ev.Use.InputTokens,
						"output_tokens": ev.Use.OutputTokens,
					})
				}
			}
		case core.EventToolExecStart:
			if cfg.Progressive && progressive.IsProgressTool(ev.ToolName) {
				return
			}
			if pw != nil {
				info := ev.ToolInfo
				if info == "" {
					info = ev.ToolName
				}
				pw("tool_start", fmt.Sprintf("→ %s: %s", ev.ToolName, info), map[string]any{
					"tool": ev.ToolName,
					"info": info,
					"id":   ev.ToolCallID,
				})
			}
		case core.EventToolExecEnd:
			if cfg.Progressive && progressive.IsProgressTool(ev.ToolName) {
				if commit := progressTracker.CommitToolCall(strings.TrimSpace(ev.ToolCallID), strings.TrimSpace(ev.ToolName), ev.ToolSuccess); commit != nil && pw != nil {
					message := commit.Message
					if message == "" {
						message = "progress updated"
					}
					envelope := progressive.BuildRunResult(cfg.SessionID, "", commit.Final, commit, output.Response())
					pw("progress_update", message, envelope)
				}
				return
			}
			if pw != nil {
				status := "ok"
				if !ev.ToolSuccess {
					status = "failed"
				}
				pw("tool_end", fmt.Sprintf("← %s (%s)", ev.ToolName, status), map[string]any{
					"tool":    ev.ToolName,
					"success": ev.ToolSuccess,
					"id":      ev.ToolCallID,
				})
			}
		case core.EventPhase:
			if pw != nil && ev.Text != "" {
				pw("phase", ev.Text, map[string]any{"text": ev.Text})
			}
		}
	})
	res.Thinking = output.Thinking()
	if !cfg.Progressive {
		res.Response = output.Response()
	}
	if execResult.Progressive != nil {
		if strings.TrimSpace(execResult.Progressive.SessionID) == "" {
			execResult.Progressive.SessionID = cfg.SessionID
		}
		res.ExitReason = execResult.Progressive.ExitReason
		res.Response = progressive.OutputText(*execResult.Progressive)
	}
	return res, err
}

func newServeJobsV2Manager(cfg *config.Config, workers int, approval runapproval.ResolvedMode, notifyDone jobsV2RunDoneNotifier, sessionStore runtime.SessionStoreOverrides, paths appmemory.Paths, opts Options, debugRaw bool) (*jobsV2Manager, error) {
	return newJobsV2ManagerWithNotifier("", workers, NewServeJobsExecutor(cfg, approval, sessionStore, paths, opts, debugRaw), notifyDone)
}

func cloneConfigForServeJob(src *config.Config) *config.Config {
	return runtime.CloneConfig(src)
}

// jobLLMServeDefaults captures serve-command-level flag values that act as
// fallbacks for a job's own (more specific) runner config. A job that omits a
// parity field inherits the serve-level default, preserving prior behavior.
type jobLLMServeDefaults struct {
	Provider      string
	Tools         string
	ReadDirs      []string
	WriteDirs     []string
	ShellAllow    []string
	MCP           string
	SystemMessage string
	MaxTurns      int
	Search        bool
	NoSearch      bool
	MemoryPaths   appmemory.Paths
}

// applyJobLLMProviderModel applies the job's provider/model selection onto jobCfg,
// reusing the same override machinery as `ask` and spawn_runner.go.
//
// The job's provider behaves like `ask`'s --provider: it may carry a
// provider:model form and takes precedence over the serve-level --provider
// (serveProviderFlag) when set. The job's model is an additional override applied
// last, so it wins as the most specific selector for the active provider.
func applyJobLLMProviderModel(jobCfg *config.Config, cfg JobsV2LLMConfig, serveProviderFlag, agentProvider, agentModel string) error {
	providerFlag := strings.TrimSpace(cfg.Provider)
	if providerFlag == "" {
		providerFlag = serveProviderFlag
	}
	if err := bootstrap.ApplyProviderOverridesWithAgent(jobCfg, jobCfg.Ask.Provider, jobCfg.Ask.Model, providerFlag, agentProvider, agentModel); err != nil {
		return err
	}
	if model := strings.TrimSpace(cfg.Model); model != "" {
		if err := runtime.ApplyAgentModelOverride(jobCfg, model); err != nil {
			return fmt.Errorf("apply model override %q: %w", model, err)
		}
	}
	return nil
}

// resolveJobLLMSettings merges a job's runner config with the serve-level defaults
// into the final SessionSettings and user prompt. Every parity field on
// jobsV2LLMConfig is optional; an omitted field falls back to the serve default so
// existing jobs resolve to exactly the same settings as before.
//
// cwd is rooted WITHOUT os.Chdir: the jobs server runs many runs concurrently in
// one process, so a global chdir would race every other run. Instead, cwd is
// appended to the read/write tool roots and set as the shell tool's working
// directory (exec.Cmd.Dir).
func resolveJobLLMSettings(jobCfg *config.Config, agent *agents.Agent, cfg JobsV2LLMConfig, def jobLLMServeDefaults) (SessionSettings, string, error) {
	toolsFlag := def.Tools
	if strings.TrimSpace(cfg.Tools) != "" {
		toolsFlag = cfg.Tools
	}
	systemMessage := def.SystemMessage
	if strings.TrimSpace(cfg.SystemMessage) != "" {
		systemMessage = cfg.SystemMessage
	}
	providerFlag := strings.TrimSpace(cfg.Provider)
	if providerFlag == "" {
		providerFlag = def.Provider
	}
	maxTurns := def.MaxTurns
	maxTurnsSet := false
	if cfg.MaxTurns > 0 {
		maxTurns = cfg.MaxTurns
		maxTurnsSet = true
	}
	// CLI flags replace (not extend) agent dirs, so combine the serve-level dirs
	// with the job's extra dirs before handing them to ResolveSettingsInDir.
	readDirs := append(append([]string{}, def.ReadDirs...), cfg.ReadDir...)
	writeDirs := append(append([]string{}, def.WriteDirs...), cfg.WriteDir...)

	settings, err := ResolveSettingsInDir(jobCfg, agent, CLIFlags{
		Provider:        providerFlag,
		Tools:           toolsFlag,
		ReadDirs:        readDirs,
		WriteDirs:       writeDirs,
		ShellAllow:      def.ShellAllow,
		MCP:             def.MCP,
		SystemMessage:   systemMessage,
		MaxTurns:        maxTurns,
		MaxTurnsSet:     maxTurnsSet,
		MaxOutputTokens: cfg.MaxOutputTokens,
		Search:          def.Search || cfg.Search,
		NoSearch:        def.NoSearch && !cfg.Search,
		Platform:        runpkg.PlatformJob,
		MemoryPaths:     def.MemoryPaths,
	}, jobCfg.Ask.Provider, jobCfg.Ask.Model, jobCfg.Ask.Instructions, jobCfg.Ask.MaxTurns, 50, cfg.Cwd)
	if err != nil {
		return SessionSettings{}, "", err
	}
	if strings.TrimSpace(cfg.Cwd) != "" {
		settings.PrimaryWorkspace = settings.BaseDir
	}

	return settings, cfg.Instructions, nil
}

func ServeJobsRunnerOptions(opts Options, debugRaw bool, approval runapproval.ResolvedMode, sessionStore runtime.SessionStoreOverrides, paths appmemory.Paths) cmdRunnerOptions {
	return cmdRunnerOptions{
		Provider:         opts.Provider,
		Tools:            opts.Tools,
		ReadDirs:         append([]string(nil), opts.ReadDirs...),
		WriteDirs:        append([]string(nil), opts.WriteDirs...),
		ShellAllow:       append([]string(nil), opts.ShellAllow...),
		MCP:              opts.MCP,
		SystemMessage:    opts.SystemMessage,
		MaxTurns:         opts.MaxTurns,
		Search:           opts.Search,
		NoSearch:         opts.NoSearch,
		NativeSearch:     opts.NativeSearch,
		NoNativeSearch:   opts.NoNativeSearch,
		ApprovalMode:     approval.Mode,
		ApprovalModeSet:  true,
		ApprovalSource:   approval.Source,
		ApprovalHeadless: true,
		Debug:            opts.Debug,
		DebugRaw:         debugRaw,
		ErrWriter:        io.Discard,
		SessionStore:     sessionStore,
		MemoryPaths:      paths,
	}
}

func NewServeJobsExecutor(baseCfg *config.Config, approval runapproval.ResolvedMode, sessionStore runtime.SessionStoreOverrides, paths appmemory.Paths, opts Options, debugRaw bool) serveJobsExecutor {
	return func(ctx context.Context, cfg JobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
		var search *bool
		if cfg.Search {
			v := true
			search = &v
		}

		var progressive *runpkg.ProgressiveOptions
		if cfg.Progressive {
			progressive = &runpkg.ProgressiveOptions{
				StopWhen:     cfg.StopWhen,
				ContinueWith: cfg.ContinueWith,
			}
		}

		runner := NewCmdRunner(baseCfg, ServeJobsRunnerOptions(opts, debugRaw, approval, sessionStore, paths))

		result, err := runner.Run(ctx, runpkg.Request{
			Platform:        runpkg.PlatformJob,
			AgentName:       cfg.AgentName,
			ModelPolicy:     policyOrEmpty(cfg.ModelPolicy),
			ParentModel:     parentModelOrEmpty(cfg.ParentModel),
			Prompt:          cfg.Instructions,
			SessionID:       cfg.SessionID,
			SessionName:     cfg.SessionName,
			Persist:         cfg.sessionPersistenceEnabled(),
			Provider:        cfg.Provider,
			Model:           cfg.Model,
			Cwd:             cfg.Cwd,
			Tools:           cfg.Tools,
			ReadDirs:        append([]string(nil), cfg.ReadDir...),
			WriteDirs:       append([]string(nil), cfg.WriteDir...),
			MaxTurns:        cfg.MaxTurns,
			MaxOutputTokens: cfg.MaxOutputTokens,
			Search:          search,
			SystemMessage:   cfg.SystemMessage,
			Skills:          cfg.Skills,
			Progressive:     progressive,
		}, eventSinkFunc(onEvent))
		return serveJobsExecResult{Progressive: progressiveFromRunResult(result.Progressive)}, err
	}
}

func policyOrEmpty(p *modelpolicy.Policy) modelpolicy.Policy {
	if p != nil {
		return *p
	}
	return modelpolicy.Policy{}
}

func parentModelOrEmpty(p *runpkg.ParentModel) runpkg.ParentModel {
	if p != nil {
		return *p
	}
	return runpkg.ParentModel{}
}
