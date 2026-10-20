package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/agents"
)

// The browser agent is the genuine built-in `developer` agent from
// internal/agents. Only its workspace/runtime guidance is added: the rest of
// the prompt (mission, approach, quality rules, output format) is unchanged.
const browserDeveloperAgentName = "developer"

// developerRepoLabel replaces {{git_repo}}. A browser tab has no git checkout,
// so the value is a short, bounded label rather than host-derived text.
const developerRepoLabel = "browser-bonsai"

var (
	developerPromptOnce sync.Once
	developerPromptBody string
)

func systemPrompt(workspace string) string {
	developerPromptOnce.Do(func() {
		developerPromptBody = loadDeveloperPrompt()
	})
	return developerPromptBody + "\n\n" + browserRuntimeGuidance(workspace)
}

// loadDeveloperPrompt resolves the embedded built-in developer agent through
// the real registry so the browser agent cannot silently drift from the CLI
// agent. Browser filesystem paths do not exist, so resolution falls through to
// the built-in definition; a tiny fallback keeps a failed lookup honest.
func loadDeveloperPrompt() string {
	registry, err := agents.NewRegistry(agents.RegistryConfig{UseBuiltin: true})
	if err == nil {
		if agent, getErr := registry.Get(browserDeveloperAgentName); getErr == nil {
			if prompt := strings.TrimSpace(agent.SystemPrompt); prompt != "" {
				return prepareDeveloperPrompt(prompt)
			}
		}
	}
	return prepareDeveloperPrompt(fallbackDeveloperPrompt)
}

const fallbackDeveloperPrompt = `You are a developer agent focused on implementing code changes for the {{git_repo}} project.

Use relative paths. Read the files you are going to modify, find the existing pattern, make the smallest coherent change, and verify it before finishing.`

// prepareDeveloperPrompt expands bounded template placeholders and drops
// guidance for capabilities a browser tab does not have.
func prepareDeveloperPrompt(prompt string) string {
	prompt = strings.ReplaceAll(prompt, "{{git_repo}}", developerRepoLabel)
	return strings.TrimSpace(stripSubagentGuidance(prompt))
}

// stripSubagentGuidance removes the spawned-subagent section: the browser demo
// has no spawn_agent tool, and leaving the instructions would invite calls that
// cannot succeed. Other sections are preserved verbatim.
func stripSubagentGuidance(prompt string) string {
	const marker = "\n### Subagents"
	start := strings.Index(prompt, marker)
	if start < 0 {
		return prompt
	}
	rest := prompt[start+len(marker):]
	if next := strings.Index(rest, "\n### "); next >= 0 {
		return prompt[:start] + rest[next:]
	}
	return prompt[:start]
}

func browserRuntimeGuidance(workspace string) string {
	return fmt.Sprintf(`## Browser runtime: browser-bonsai

You are the real built-in developer agent running as Go WebAssembly inside a browser tab. There is no application backend and no network: chat, tools, sessions, and files all stay on this page.

Workspace: %s — an in-memory filesystem shared by the file tools and the Linux guest. It disappears when the page reloads.

Available tools (and no others): read_file, write_file, edit_file, glob, shell.

- Ignore any instructions above that name tools or workflows you do not have: spawn_agent and subagents, grep, view_image, show_media, image_generate, ask_user, update_plan, web search, package installs, and git. Use the file tools and shell instead.
- shell runs commands in a real Linux guest (v86) inside this same browser tab over the same workspace: BusyBox sh, pipes, awk, sed, grep, coreutils, and CPython 3.12 as python/python3. The tested Python standard-library set is json, csv, statistics, unittest, and sqlite3; base64, binascii, gzip, and zipfile work through bundled zlib. Optional extension modules that need unbundled libraries (ssl, ctypes, bz2, lzma, readline) are unavailable. It is a real Linux system, but it is NOT the host machine and has no network; there is no pip or package installation, and node and Go are not installed. Do not assume other third-party Python packages exist.
- working_dir is always inside /workspace (default /workspace); the guest mounts the workspace files there. timeout_seconds defaults to 10 and is capped at 30; the first command may take longer because the VM boots lazily.
- Approval policy is YOLO: every tool call, including shell, is pre-approved and execution never prompts. Act autonomously, and stay inside the workspace.
- Work in small, verifiable steps: read the file, run the playground tests, fix the code, run them again, and report the real stdout/stderr.`, workspace)
}
