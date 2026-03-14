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

Or:

```bash
go install github.com/samsaffron/term-llm@latest
```

## 30-second quickstart

No API key needed if you use Zen:

```bash
term-llm exec --provider zen "list files"
term-llm ask --provider zen "explain git rebase"
term-llm chat
```

If you already have a provider key:

```bash
export ANTHROPIC_API_KEY=your-key
# or OPENAI_API_KEY / GEMINI_API_KEY / OPENROUTER_API_KEY / XAI_API_KEY
```

## Read the docs

The detailed docs live at **https://term-llm.com** and are authored in Markdown in this repo, then built with Hugo.

- [Getting started](https://term-llm.com/getting-started/)
- [Guides](https://term-llm.com/guides/)
- [Architecture](https://term-llm.com/architecture/)
- [Reference](https://term-llm.com/reference/)

Common entry points:

- [Configuration](https://term-llm.com/reference/configuration/)
- [Usage](https://term-llm.com/guides/usage/)
- [Agents](https://term-llm.com/guides/agents/)
- [Skills](https://term-llm.com/guides/skills/)
- [MCP servers](https://term-llm.com/guides/mcp-servers/)
- [Memory](https://term-llm.com/guides/memory/)
- [Jobs](https://term-llm.com/guides/job-runner/)

## License

MIT
