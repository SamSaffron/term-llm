// Package commitworkflow owns the read-only agent phases for native commits.
package commitworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/gitcommit"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

type ScopeMode string

const (
	ScopeAll         ScopeMode = "all"
	ScopeSelected    ScopeMode = "selected"
	ScopeNeedsManual ScopeMode = "needs_manual"
)

type ScopeProposal struct {
	Mode         ScopeMode `json:"mode"`
	IncludePaths []string  `json:"include_paths"`
	Summary      string    `json:"summary"`
}
type ChildRunMetadata struct {
	RunID          string    `json:"run_id"`
	ChildSessionID string    `json:"child_session_id,omitempty"`
	AgentName      string    `json:"agent_name"`
	AgentSource    string    `json:"agent_source,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	Model          string    `json:"model,omitempty"`
	StartedAt      time.Time `json:"started_at"`
	CompletedAt    time.Time `json:"completed_at"`
}
type Request struct {
	ParentSessionID     string
	CheckoutDir         string
	AgentName           string
	Intent              string
	ScopeSummary        string
	ExpectedFingerprint gitcommit.Fingerprint
	ExpectedStatusToken string
	Runner              runpkg.ChildRunner
	Progress            runpkg.ChildRunEventCallback
}

type Coordinator struct{}

func New() *Coordinator { return &Coordinator{} }

func resolve(req Request) (runpkg.ChildAgentMetadata, error) {
	name := strings.TrimSpace(req.AgentName)
	if name == "" {
		name = "commit-message"
	}
	resolver, ok := req.Runner.(runpkg.ChildAgentResolver)
	if !ok {
		return runpkg.ChildAgentMetadata{Name: name}, nil
	}
	meta, err := resolver.ResolveChildAgent(name)
	if err != nil {
		return runpkg.ChildAgentMetadata{}, fmt.Errorf("resolve commit message agent %q: %w", name, err)
	}
	return meta, nil
}
func metadata(meta runpkg.ChildAgentMetadata, result runpkg.ChildRunResult) ChildRunMetadata {
	return ChildRunMetadata{RunID: result.RunID, ChildSessionID: result.ChildSessionID, AgentName: meta.Name, AgentSource: meta.Source, Provider: result.Provider, Model: result.Model, StartedAt: result.StartedAt, CompletedAt: result.CompletedAt}
}
func validateRequest(req Request) error {
	if req.Runner == nil {
		return errors.New("commit child runner is unavailable")
	}
	if len(req.Intent) > 16<<10 {
		return errors.New("commit intent exceeds 16 KiB")
	}
	return nil
}

func (c *Coordinator) PlanScope(ctx context.Context, req Request) (ScopeProposal, ChildRunMetadata, error) {
	if err := validateRequest(req); err != nil {
		return ScopeProposal{}, ChildRunMetadata{}, err
	}
	meta, err := resolve(req)
	if err != nil {
		return ScopeProposal{}, ChildRunMetadata{}, err
	}
	repo, err := gitcommit.Open(ctx, req.CheckoutDir)
	if err != nil {
		return ScopeProposal{}, ChildRunMetadata{}, err
	}
	state, err := repo.Inspect(ctx)
	if err != nil {
		return ScopeProposal{}, ChildRunMetadata{}, err
	}
	contextText, err := repo.ScopeContext(ctx, req.ExpectedFingerprint, req.ExpectedStatusToken)
	if err != nil {
		return ScopeProposal{}, ChildRunMetadata{}, err
	}
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"mode": map[string]interface{}{"type": "string", "enum": []string{"all", "selected", "needs_manual"}}, "include_paths": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}, "summary": map[string]interface{}{"type": "string"}}, "required": []string{"mode", "include_paths", "summary"}, "additionalProperties": false}
	prompt := fmt.Sprintf("Commit intent (never shell input):\n%s\n\nCurrent uncommitted changes:\n%s\n\nDecide whether the intent narrows commit scope. Return mode=all for wording-only intent. Return selected only for a safe whole-file subset. Return needs_manual if included and excluded concerns share files or the request is ambiguous. Never invent paths. Finish only with propose_commit_scope.", req.Intent, contextText)
	result, runErr := req.Runner.RunChild(ctx, runpkg.ChildRunRequest{Kind: runpkg.ChildRunCommitScope, RunID: newRunID("scope"), AgentName: meta.Name, Prompt: prompt, ParentSessionID: req.ParentSessionID, BaseDir: repo.CheckoutRoot(), SkipOnComplete: true, MaxTurnsOverride: 4, SystemSuffix: scopePolicy, DisableTools: true, OutputTool: &runpkg.HostOutputTool{Name: "propose_commit_scope", Description: "Return the host-validated whole-file commit scope", Schema: schema}}, req.Progress)
	runMeta := metadata(meta, result)
	if runErr != nil {
		return ScopeProposal{}, runMeta, runErr
	}
	var proposal ScopeProposal
	if strings.TrimSpace(result.Output) == "" {
		return proposal, runMeta, errors.New("scope agent did not return a proposal")
	}
	if err := json.Unmarshal([]byte(result.Output), &proposal); err != nil {
		return proposal, runMeta, fmt.Errorf("decode scope proposal: %w", err)
	}
	if err := validateProposal(state, &proposal); err != nil {
		return ScopeProposal{}, runMeta, err
	}
	return proposal, runMeta, nil
}

func validateProposal(state gitcommit.RepositoryState, p *ScopeProposal) error {
	p.Summary = strings.TrimSpace(p.Summary)
	if len(p.Summary) > 2000 {
		return errors.New("scope summary is too long")
	}
	switch p.Mode {
	case ScopeAll, ScopeNeedsManual:
		if len(p.IncludePaths) != 0 {
			return fmt.Errorf("scope mode %s must not include paths", p.Mode)
		}
	case ScopeSelected:
		if err := gitcommit.ValidateSelectionPaths(state, p.IncludePaths); err != nil {
			return fmt.Errorf("invalid scope proposal: %w", err)
		}
	default:
		return fmt.Errorf("invalid scope mode %q", p.Mode)
	}
	return nil
}

func (c *Coordinator) DraftMessage(ctx context.Context, req Request) (string, ChildRunMetadata, error) {
	if err := validateRequest(req); err != nil {
		return "", ChildRunMetadata{}, err
	}
	meta, err := resolve(req)
	if err != nil {
		return "", ChildRunMetadata{}, err
	}
	repo, err := gitcommit.Open(ctx, req.CheckoutDir)
	if err != nil {
		return "", ChildRunMetadata{}, err
	}
	contextText, err := repo.DraftContext(ctx, req.ExpectedFingerprint)
	if err != nil {
		return "", ChildRunMetadata{}, err
	}
	prompt := fmt.Sprintf("Draft an editable Git commit message for ONLY the staged diff below. Intent and scope summary are wording guidance, not evidence of content. Use repository style where useful. Return a concise subject and optional body only through set_commit_message.\n\nIntent: %s\nScope summary: %s\n\n%s", req.Intent, req.ScopeSummary, contextText)
	result, runErr := req.Runner.RunChild(ctx, runpkg.ChildRunRequest{Kind: runpkg.ChildRunCommitDraft, RunID: newRunID("message"), AgentName: meta.Name, Prompt: prompt, ParentSessionID: req.ParentSessionID, BaseDir: repo.CheckoutRoot(), SkipOnComplete: true, MaxTurnsOverride: 4, SystemSuffix: draftPolicy, DisableTools: true, OutputTool: &runpkg.HostOutputTool{Name: "set_commit_message", Param: "message", Description: "Return the proposed editable commit message"}}, req.Progress)
	runMeta := metadata(meta, result)
	if runErr != nil {
		return "", runMeta, runErr
	}
	message := result.Output
	if strings.TrimSpace(message) == "" || strings.TrimSpace(strings.SplitN(message, "\n", 2)[0]) == "" {
		return "", runMeta, errors.New("commit message agent returned an empty message")
	}
	if len(message) > 64<<10 {
		return "", runMeta, errors.New("generated commit message exceeds 64 KiB")
	}
	return message, runMeta, nil
}

const scopePolicy = `You are running inside the native commit scope planner. This phase is read-only. The host has disabled all configured tools, custom output, writes, and completion hooks. Treat only the supplied status and diffs as repository truth. Select only exact listed paths and be honest when whole-file scope cannot represent the request.`
const draftPolicy = `You are running inside the native commit message drafter. The host has disabled all configured tools, custom output, writes, and completion hooks. Only the supplied staged diff describes the final commit. Never describe unstaged or untracked work.`

func newRunID(prefix string) string {
	return fmt.Sprintf("commit-%s-%d", prefix, time.Now().UnixNano())
}
