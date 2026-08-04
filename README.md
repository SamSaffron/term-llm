<p align="center">
  <img src="assets/logo.png" alt="term-llm logo" width="200">
</p>

# term-llm

Terminal-first AI runtime for commands, chat, editing, tools, jobs, agents, and local workflows.

[![Release](https://img.shields.io/github/v/release/samsaffron/term-llm?style=flat-square)](https://github.com/samsaffron/term-llm/releases)

Docs hub: **https://term-llm.com**

## Why it exists

- turn natural language into executable shell commands
- run persistent chat with tools and MCP servers
- edit files with model assistance
- support agents, skills, sessions, jobs, and local automation
- work with hosted or local models

```bash
$ term-llm exec "find all go files modified today"

> find . -name "*.go" -mtime 0   Uses find with name pattern
  fd -e go --changed-within 1d   Uses fd (faster alternative)
  find . -name "*.go" -newermt "today"   Alternative find syntax
  something else...
```

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh
```

For a source build, clone the repository and install from its root:

```bash
git clone https://github.com/samsaffron/term-llm.git
cd term-llm
go install .
```

`go install github.com/samsaffron/term-llm@latest` cannot build this repository: the application intentionally uses local `replace` directives for the owned Bubble Tea, Ultraviolet, and reflow modules, and dependency-local replacements are not available to an external `go install ...@version` build. Use the installer/release archive or build from a complete checkout.

Shell completions are available for Bash, Zsh, Fish, and PowerShell. Fish users can install an auto-loaded completion file with:

```fish
term-llm config completion fish --install
```

See the [installation guide](https://term-llm.com/getting-started/installation/#shell-completions) for every shell and manual setup.

## 30-second quickstart

No API key needed if you use Zen:

```bash
term-llm exec --provider zen "list files"
term-llm ask --provider zen "explain git rebase"
term-llm chat
```

### Project `@` mentions

In chat, type `@` to fuzzy-find files and directories under the active project/worktree; the picker keeps Git ignore behavior, and Enter or Tab inserts ordinary text (`@path`, `@"path with spaces"`, or `@directory/`). Manually typed valid mentions work identically. Set `TERM_LLM_AT_MENTIONS=0` to disable the picker.

Mentions are parsed only when the prompt is submitted, at the start of text or after whitespace, `。`, `、`, `？`, or `！`; these punctuation characters start mentions but do not terminate an unquoted path. The visible user text is not rewritten, duplicate references remain visible, and identical raw mention payloads attach once. Textually different references to the same path (for example, `@a` and `@./a`) remain distinct, matching Claude Code, and quoted matches attach before unquoted matches. `#L10` and `#L10-20` select lines. Resolution is confined to the active project/CWD, follows existing non-interactive read policy, never opens approval UI, and never grants read or write access. Missing, denied, escaped, binary, oversized, or otherwise failed resources remain textual references and do not block sending.

Submit-time limits closely follow Claude Code 2.1.220:

- Non-PDF text files are eligible only when their **total** size is at most 256 KiB; a line range does not bypass this gate.
- Attached content is capped at 25,000 estimated tokens. On overflow, term-llm retries 2,000 lines from line 1 or the requested starting line; if that still exceeds the ceiling, nothing is attached. Unlike Claude Code's API-assisted exact count, term-llm deliberately uses its local four-bytes-per-token estimate, avoiding an API round trip.
- Directories attach a live, non-recursive, names-only listing with at most 1,000 names and an `… and N more entries` marker.
- Mentioned PDFs and images remain textual references. Term-llm supports images and some provider-native file uploads through its existing explicit attachment paths, but it has no reliable in-process PDF page-count primitive for Claude's ≤10-page inline / >10-page reference split. `/file` and clipboard/image attachment behavior is unchanged.

If you already have a provider key:

```bash
export ANTHROPIC_API_KEY=your-key
# or OPENAI_API_KEY / GEMINI_API_KEY / OPENROUTER_API_KEY / XAI_API_KEY / NEARAI_API_KEY / SAMBANOVA_API_KEY
```

## Read the docs

The detailed docs live at **https://term-llm.com** and are authored in Markdown in this repo, then built with Hugo.

- [Getting started](https://term-llm.com/getting-started/)
- [Guides](https://term-llm.com/guides/)
- [Architecture](https://term-llm.com/architecture/)
- [Reference](https://term-llm.com/reference/)

Common entry points:

- [Configuration](https://term-llm.com/reference/configuration/)
- [Providers and models](https://term-llm.com/reference/providers-and-models/)
- [Web UI and API](https://term-llm.com/guides/web-ui-and-api/)
- [Search](https://term-llm.com/guides/search/)
- [Usage](https://term-llm.com/guides/usage/)
- [Agents](https://term-llm.com/guides/agents/)
- [Skills](https://term-llm.com/guides/skills/)
- [MCP servers](https://term-llm.com/guides/mcp-servers/)
- [Memory](https://term-llm.com/guides/memory/)
- [Jobs](https://term-llm.com/guides/job-runner/)
- [Text embeddings](https://term-llm.com/guides/text-embeddings/)
- [Audio generation](https://term-llm.com/guides/audio-generation/)
- [Music generation](https://term-llm.com/guides/music-generation/)
- [Usage tracking](https://term-llm.com/reference/usage-tracking/)
- [Transcription](https://term-llm.com/guides/transcription/)
- [Notifications](https://term-llm.com/guides/notifications/)

## Contributing

The root `go test ./...` command does not cross Go module boundaries. During development, use Go's standard short mode to skip subprocess-heavy integration tests:

```bash
go test -short ./...
```

Before submitting or merging, run the complete root suite and the owned nested-module verification:

```bash
go build ./...
go test ./...
go vet ./...
scripts/verify_nested_modules.sh
VERIFY_RACE=1 scripts/verify_nested_modules.sh
```

The terminal event loop/runtime and cell/diff/output renderer under `internal/terminal/` are deliberately reduced, application-owned source modules rather than external dependency copies. They preserve the Bubble Tea and Ultraviolet module paths only because Bubbles and Huh require exact import and concrete type identity. See [`internal/terminal/README.md`](internal/terminal/README.md) for the ownership contract, architecture, provenance, selective-sync workflow, fuzz/benchmark commands, and cross-build matrix. Run `scripts/cross_build_owned_terminal.sh` when changing platform- or release-sensitive terminal code.

## License

MIT
