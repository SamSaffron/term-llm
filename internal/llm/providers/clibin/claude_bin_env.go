package clibin

import (
	"os"
	"strings"
)

// forcedClaudeCodeIsolationEnv disables Claude Code surfaces that can inject
// first-party prompt/tool behavior into claude-bin runs. term-llm owns tools,
// memory, background jobs, and project instructions for this provider; Claude
// Code is only the authenticated model transport plus MCP client.
var forcedClaudeCodeIsolationEnv = map[string]string{
	"CLAUDE_CODE_DISABLE_WORKFLOWS":            "1",
	"DISABLE_GROWTHBOOK":                       "1",
	"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	"CLAUDE_CODE_DISABLE_AGENT_VIEW":           "1",
	"CLAUDE_CODE_DISABLE_CRON":                 "1",
	"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS":     "1",
	"CLAUDE_CODE_DISABLE_AUTO_MEMORY":          "1",
	"CLAUDE_CODE_DISABLE_CLAUDE_MDS":           "1",
	"CLAUDE_CODE_DISABLE_GIT_INSTRUCTIONS":     "1",
	"CLAUDE_CODE_DISABLE_ADVISOR_TOOL":         "1",
	"CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS":   "1",
	// Tool search defers MCP tools until the model searches for them. Every
	// term-llm tool is an MCP tool, so it must never hide the catalogue.
	"ENABLE_TOOL_SEARCH": "false",
}

// ignoredInheritedClaudeCodeEnv lists shell variables aimed at interactive
// Claude Code that silently break or override claude-bin. Values set
// explicitly in the claude-bin provider env are still honoured.
//   - CLAUDE_CODE_SIMPLE skips OAuth, so subscription logins report "Not
//     logged in" even with CLAUDE_CODE_OAUTH_TOKEN set.
//   - CLAUDE_CODE_EFFORT_LEVEL would override term-llm's effort selection.
//   - MAX_MCP_OUTPUT_TOKENS would replace claudeCodeDefaultEnv's raised cap.
var ignoredInheritedClaudeCodeEnv = map[string]bool{
	"CLAUDE_CODE_SIMPLE":       true,
	"CLAUDE_CODE_EFFORT_LEVEL": true,
	"MAX_MCP_OUTPUT_TOKENS":    true,
}

// claudeCodeDefaultEnv is applied unless the provider env sets the key.
// term-llm truncates tool output itself (tools.max_tool_output_chars), so
// Claude Code's own MCP output cap must sit above it. When that cap trips,
// Claude Code replaces the result with "saved to a file, read it with Read",
// and claude-bin runs with Read disabled, so the model cannot recover.
var claudeCodeDefaultEnv = map[string]string{
	"MAX_MCP_OUTPUT_TOKENS": "200000",
}

// claudeMCPMaxResultSizeChars is published per tool as
// _meta["anthropic/maxResultSizeChars"]. Claude Code otherwise persists any
// result above ~50k characters to disk and shows the model a 2KB preview,
// which claude-bin's model cannot read past without the built-in Read tool.
const claudeMCPMaxResultSizeChars = 500_000

func (p *ClaudeBinProvider) buildCommandEnv(effort string) []string {
	runningAsRoot := getEuid() == 0
	env := os.Environ()
	filtered := env[:0]
	for _, e := range env {
		key := e
		if idx := strings.IndexByte(e, '='); idx >= 0 {
			key = e[:idx]
		}
		if p.preferOAuth && key == "ANTHROPIC_API_KEY" {
			continue
		}
		if ignoredInheritedClaudeCodeEnv[key] {
			continue
		}
		// Claude Code refuses bypassPermissions as root unless it sees an
		// explicit sandbox marker. term-llm owns tool execution, so force the
		// inert IS_SANDBOX marker below rather than inheriting a stale value.
		if runningAsRoot && key == "IS_SANDBOX" {
			continue
		}
		// Claude Code environment flags can enable hidden first-party prompt/tool
		// surfaces. Drop inherited values and force our isolation defaults below.
		if _, ok := forcedClaudeCodeIsolationEnv[key]; ok {
			continue
		}
		if len(p.extraEnv) > 0 {
			if _, ok := p.extraEnv[key]; ok {
				continue
			}
		}
		filtered = append(filtered, e)
	}
	if effort != "" {
		filtered = append(filtered, "CLAUDE_CODE_EFFORT_LEVEL="+effort)
	}
	for k, v := range p.extraEnv {
		if p.preferOAuth && k == "ANTHROPIC_API_KEY" {
			continue
		}
		if effort != "" && k == "CLAUDE_CODE_EFFORT_LEVEL" {
			continue
		}
		if runningAsRoot && k == "IS_SANDBOX" {
			continue
		}
		if _, ok := forcedClaudeCodeIsolationEnv[k]; ok {
			continue
		}
		filtered = append(filtered, k+"="+v)
	}
	for k, v := range claudeCodeDefaultEnv {
		if _, ok := p.extraEnv[k]; !ok {
			filtered = append(filtered, k+"="+v)
		}
	}
	if runningAsRoot && !envHasTruthy(filtered, "CLAUDE_CODE_BUBBLEWRAP") {
		filtered = append(filtered, "IS_SANDBOX=1")
	}
	// Always disable Claude Code features that can bleed first-party prompt/tool
	// behavior into term-llm runs; these must win over inherited or configured
	// values.
	for k, v := range forcedClaudeCodeIsolationEnv {
		filtered = append(filtered, k+"="+v)
	}
	return filtered
}

func envHasTruthy(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(e, prefix)))
			return v != "" && v != "0" && v != "false"
		}
	}
	return false
}
