package clibin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/samsaffron/term-llm/internal/cache"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/procutil"
)

const (
	claudeBinModelCacheKey = "claude-bin"
	// claudeListModelsRequestID tags the stream-json control request so its
	// response can be told apart from any other line Claude Code prints.
	claudeListModelsRequestID = "term-llm-list-models"
	// claudeDefaultModelValue is Claude Code's "Default (recommended)" entry.
	// term-llm disables the feature flags that resolve it, so it would not
	// select the same model here; the model it resolves to is listed instead.
	claudeDefaultModelValue = "default"
	// claudeRefreshAttemptKey records the last completion-driven listing so a
	// failing or slow claude is not respawned on every tab press.
	claudeRefreshAttemptKey = "claude-bin-refresh-attempt"
	// claudeRefreshRetryInterval spaces out completion-driven listing attempts.
	claudeRefreshRetryInterval = 10 * time.Minute
	// claudeModelIDMaxLen bounds model IDs accepted from the CLI.
	claudeModelIDMaxLen = 128
)

// claudeCatalogGateEnv are the isolation variables that hide the account's
// model catalog: with either set, Claude Code skips the feature flags that
// list models such as Mythos. Model listing leaves them unset; every other
// isolation default still applies.
var claudeCatalogGateEnv = map[string]bool{
	"DISABLE_GROWTHBOOK":                       true,
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true,
}

// claudeEffortLevels lists the reasoning-effort suffixes recognised on
// claude-bin model names (e.g. "opus-max", "claude-mythos-5-1-high").
var claudeEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// claudeModelAliases are Claude Code's own short model names, in the order
// legacy shorthand names are matched against them.
var claudeModelAliases = []string{"opus", "fable", "haiku", "sonnet"}

// parseClaudeEffort splits a trailing effort suffix from a model name:
// "opus-max" -> ("opus", "max"), "claude-mythos-5-1-high" ->
// ("claude-mythos-5-1", "high"), "haiku" -> ("haiku", ""). Claude Code
// reports which efforts each model accepts; term-llm forwards the requested
// level and leaves validation to the CLI.
func parseClaudeEffort(model string) (string, string) {
	lower := strings.ToLower(model)
	for _, effort := range claudeEffortLevels {
		if strings.HasSuffix(lower, "-"+effort) && len(model) > len(effort)+1 {
			return model[:len(model)-len(effort)-1], effort
		}
	}
	return model, ""
}

// ValidateClaudeBinModel rejects model strings that are bare effort levels
// (e.g. "claude-bin:max"). Without this check the effort would be silently
// treated as the model name and CLAUDE_CODE_EFFORT_LEVEL would never be set.
func ValidateClaudeBinModel(model string) error {
	for _, effort := range claudeEffortLevels {
		if strings.EqualFold(strings.TrimSpace(model), effort) {
			return fmt.Errorf(
				"claude-bin model %q is an effort level, not a model; "+
					"did you mean \"claude-bin:opus-%s\"? "+
					"(run 'term-llm models --provider claude-bin' to see models and their efforts)",
				model, model,
			)
		}
	}
	return nil
}

// mapModelToClaudeArg converts a term-llm model name to the claude --model
// argument. Claude Code aliases, full model IDs ("claude-mythos-5-1") and
// bracketed variants ("sonnet[1m]") pass through unchanged. Older shorthand
// such as "opus-4" maps to the alias it names; anything else is passed through
// so Claude Code reports an unknown model instead of silently running another.
func mapModelToClaudeArg(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" || strings.HasPrefix(m, "claude-") || strings.Contains(m, "[") {
		return m
	}
	for _, alias := range claudeModelAliases {
		if strings.Contains(m, alias) {
			return alias
		}
	}
	return m
}

// RefreshClaudeBinModelsIfStale refreshes the on-disk model catalog when it is
// missing or older than the shared model-cache TTL, attempting at most once
// per retry interval so failures do not stall every completion. A failed
// refresh leaves any stale cache available as a completion fallback.
func RefreshClaudeBinModelsIfStale(ctx context.Context, model string, env map[string]string) error {
	cached, err := cache.ReadModelCache(claudeBinModelCacheKey)
	if err == nil && cache.IsCacheValid(cached) && (len(cached.ModelInfos) > 0 || len(cached.Models) > 0) {
		return nil
	}
	if attempt, err := cache.ReadModelCache(claudeRefreshAttemptKey); err == nil && time.Since(attempt.FetchedAt) < claudeRefreshRetryInterval {
		return nil
	}
	_ = cache.WriteModelCache(claudeRefreshAttemptKey, nil)
	_, err = NewClaudeBinProvider(model, env).ListModels(ctx)
	return err
}

// GetCachedClaudeBinModels returns the last live model list fetched from
// Claude Code. It never spawns claude, so it is safe for shell completion and
// provider pickers. Run `term-llm models --provider claude-bin` to refresh it.
func GetCachedClaudeBinModels() []string {
	models := GetCachedClaudeBinModelInfos()
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// GetCachedClaudeBinModelInfos returns cached live Claude Code model metadata,
// including each model's supported effort levels. Stale entries are still
// returned because they are preferable to the curated fallback.
func GetCachedClaudeBinModelInfos() []core.ModelInfo {
	cached, err := cache.ReadModelCache(claudeBinModelCacheKey)
	if err != nil || cached == nil || (len(cached.ModelInfos) == 0 && len(cached.Models) == 0) {
		return nil
	}
	return modelInfosFromCache(cached)
}

// RefreshClaudeBinCacheSync stores a freshly fetched Claude Code model list
// for completions, effort pickers and offline provider/model pickers.
func RefreshClaudeBinCacheSync(models []core.ModelInfo) {
	if len(models) == 0 {
		return
	}
	_ = cache.WriteModelInfoCache(claudeBinModelCacheKey, modelInfosToCache(models))
}

// buildModelListEnv mirrors the inference environment's credential handling
// and isolation defaults, except the variables in claudeCatalogGateEnv: the
// account-specific catalog (and models such as Mythos) is only listed with
// Claude Code's feature flags on. Inherited or configured values for those
// two variables are still honoured.
func (p *ClaudeBinProvider) buildModelListEnv() []string {
	env := make([]string, 0, len(os.Environ())+len(p.extraEnv)+len(forcedClaudeCodeIsolationEnv))
	skip := func(key string) bool {
		if p.preferOAuth && key == "ANTHROPIC_API_KEY" {
			return true
		}
		_, forced := forcedClaudeCodeIsolationEnv[key]
		return forced && !claudeCatalogGateEnv[key]
	}
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if _, ok := p.extraEnv[key]; ok || skip(key) {
			continue
		}
		env = append(env, e)
	}
	for k, v := range p.extraEnv {
		if !skip(k) {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range forcedClaudeCodeIsolationEnv {
		if !claudeCatalogGateEnv[k] {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func defaultClaudeModelsCommandOutput(ctx context.Context, p *ClaudeBinProvider) ([]byte, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// User and project settings can append a custom model to the list;
		// only the account catalog is wanted here.
		"--setting-sources", "",
		"--strict-mcp-config",
		"--no-session-persistence",
	}
	// A private empty directory keeps shared locations such as /tmp (and any
	// .git, .claude or CLAUDE.md planted there) out of Claude Code's startup.
	dir, err := os.MkdirTemp("", "term-llm-claude-models-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	cmd, err := newCLICommand(ctx, "claude", args, dir)
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = claudeCommandWaitDelay
	request := fmt.Sprintf(`{"type":"control_request","request_id":%q,"request":{"subtype":"list_models"}}`, claudeListModelsRequestID)
	// Claude Code answers control requests and exits once stdin closes.
	cmd.Stdin = strings.NewReader(request + "\n")
	cmd.Env = p.buildModelListEnv()
	auditedEnv, wireAudit, err := startCLIWireAudit("claude-bin-models", cmd.Env)
	if err != nil {
		return nil, err
	}
	cmd.Env = auditedEnv
	defer stopCLIWireAudit(wireAudit)
	cleanupProcess, err := procutil.PrepareCommand(cmd)
	if err != nil {
		return nil, fmt.Errorf("prepare claude command: %w", err)
	}
	defer cleanupProcess()
	return cmd.Output()
}

var claudeModelsCommandOutput = defaultClaudeModelsCommandOutput

// ListModels asks Claude Code for the account-specific model catalog through
// the stream-json list_models control request (the one the Agent SDK uses),
// then saves it in term-llm's shared model cache. No prompt is sent.
func (p *ClaudeBinProvider) ListModels(ctx context.Context) ([]core.ModelInfo, error) {
	output, err := claudeModelsCommandOutput(ctx, p)
	models, parseErr := parseClaudeListModelsOutput(output)
	if parseErr != nil {
		var rejected *claudeListModelsRejectedError
		switch {
		case errors.As(parseErr, &rejected) || err == nil:
			return nil, fmt.Errorf("list Claude models: %w", parseErr)
		case ctx.Err() != nil:
			return nil, fmt.Errorf("list Claude models: %w", ctx.Err())
		}
		return nil, fmt.Errorf("list Claude models: %w%s", err, claudeStderrSummary(err))
	}
	RefreshClaudeBinCacheSync(models)
	return models, nil
}

// claudeListModelsRejectedError is Claude Code's own error reply to the
// list_models request; it explains a failure better than the exit status.
type claudeListModelsRejectedError struct{ message string }

func (e *claudeListModelsRejectedError) Error() string {
	return "claude rejected list_models: " + e.message
}

// claudeStderrSummary returns the last stderr line of a failed claude run so
// auth and version failures are diagnosable from the error alone.
func claudeStderrSummary(err error) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(exitErr.Stderr)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return ""
	}
	if len(last) > 300 {
		last = last[:300] + "..."
	}
	return ": " + last
}

type claudeListedModel struct {
	Value                 string   `json:"value"`
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

type claudeControlResponseLine struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
		Response  struct {
			Models []claudeListedModel `json:"models"`
		} `json:"response"`
	} `json:"response"`
}

// parseClaudeListModelsOutput finds the list_models control response in
// Claude Code's stream-json output and converts it to term-llm model infos.
func parseClaudeListModelsOutput(output []byte) ([]core.ModelInfo, error) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var msg claudeControlResponseLine
		if json.Unmarshal(line, &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != claudeListModelsRequestID {
			continue
		}
		if msg.Response.Subtype != "success" {
			return nil, &claudeListModelsRejectedError{message: strings.TrimSpace(msg.Response.Error)}
		}
		models := claudeListedModelsToInfos(msg.Response.Response.Models)
		if len(models) == 0 {
			return nil, fmt.Errorf("no models found")
		}
		return models, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read claude output: %w", err)
	}
	return nil, fmt.Errorf("claude did not answer the list_models request (Claude Code may be too old)")
}

func claudeListedModelsToInfos(listed []claudeListedModel) []core.ModelInfo {
	models := make([]core.ModelInfo, 0, len(listed))
	seen := make(map[string]bool, len(listed))
	add := func(id, displayName string, m claudeListedModel) {
		id = strings.TrimSpace(id)
		if !validClaudeModelID(id) || seen[id] {
			return
		}
		seen[id] = true
		info := core.ModelInfo{ID: id, DisplayName: strings.TrimSpace(displayName), OwnedBy: "anthropic", InputPrice: -1, OutputPrice: -1}
		if m.SupportsEffort {
			info.ReasoningEfforts = cloneEfforts(m.SupportedEffortLevels)
		}
		models = append(models, info)
	}
	var defaultModel *claudeListedModel
	for i, m := range listed {
		if strings.TrimSpace(m.Value) == claudeDefaultModelValue {
			defaultModel = &listed[i]
			continue
		}
		add(m.Value, m.DisplayName, m)
	}
	if defaultModel != nil {
		name := defaultModel.Description
		if name == "" {
			name = defaultModel.ResolvedModel
		}
		add(defaultModel.ResolvedModel, name, *defaultModel)
	}
	return models
}

// validClaudeModelID rejects IDs that could corrupt completion output or the
// cache: empty, overlong, or containing whitespace or control characters.
func validClaudeModelID(id string) bool {
	if id == "" || len(id) > claudeModelIDMaxLen {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
