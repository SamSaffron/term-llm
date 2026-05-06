---
title: "Serving tools via MCP"
weight: 8
description: "Run term-llm as an MCP server over HTTP, exposing file, search, shell, and web tools to any MCP client."
kicker: "Remote tools"
next:
  label: MCP servers
  url: /guides/mcp-servers/
---

`term-llm serve mcp` starts an MCP server over HTTP that exposes term-llm's local tools to any MCP client. Use case: a cloud dev box runs the server, and a local term-llm instance (or any MCP-compatible client) connects to use its tools remotely.

## Quick start

```bash
# Expose all tools on localhost
term-llm serve mcp --tools all

# Expose just file reading and search
term-llm serve mcp --tools read_file,grep,glob

# Full power on a cloud dev box
term-llm serve mcp --tools all --host 0.0.0.0 --port 8080 \
  --write-dir /home/sam/project \
  --shell-allow "go *" --shell-allow "git *" --shell-allow "make *"
```

On startup the server prints the URL, auth token, and enabled tools:

```
MCP server listening on http://127.0.0.1:8080/mcp
auth token: abc123...
tools: edit_file, glob, grep, read_file, shell, web_search, write_file, ...
```

When binding to a wildcard address (`0.0.0.0` or `::`), the printed URL uses `127.0.0.1` for convenience. Remote clients should use the machine's actual hostname or IP.

## Connecting a client

From another terminal (or machine), add the server as an MCP endpoint:

```bash
term-llm mcp add http://devbox:8080/mcp   # prompted for token
term-llm mcp info devbox                    # verify tools
term-llm mcp run devbox shell command="echo hello"
term-llm chat --mcp devbox "what files are in this directory?"
```

Via SSH tunnel (no `--token` needed since traffic stays on localhost):

```bash
ssh -L 8080:localhost:8080 devbox 'term-llm serve mcp --tools all'
term-llm mcp add http://localhost:8080/mcp
```

## Available tools

The `--tools` flag is **required**. Pass a comma-separated list of tool names, or `all`. Only the tools listed below are accepted. Internal tools like `ask_user`, `spawn_agent`, and `view_image` are not available in MCP server mode.

### File & search

| Tool | Description |
|------|-------------|
| `read_file` | Read files on the remote machine |
| `write_file` | Write/create files on the remote filesystem |
| `edit_file` | Surgical find/replace edits (default edit tool) |
| `glob` | Find files by pattern |
| `grep` | Search file contents with regex |
| `shell` | Run shell commands (build, test, git, docker, etc.) |

### Web

| Tool | Description |
|------|-------------|
| `web_search` | Search the web via the server's configured search provider |
| `read_url` | Fetch a web page and return it as markdown |

`web_search` requires a search provider to be configured. If `all` is specified but no provider is available, a warning is logged and the tool is skipped.

### Image

| Tool | Description |
|------|-------------|
| `image_generate` | Generate images via the server's configured image provider |

### The `all` shorthand

`--tools all` expands to: `read_file`, `write_file`, `edit_file`, `shell`, `grep`, `glob`, `image_generate`, `web_search`, `read_url`.

Tools whose backing provider isn't configured (e.g. `web_search` without a search provider) are skipped with a warning.

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--tools` | *(required)* | Comma-separated tool names or `all` |
| `--edit-format` | `edit_file` | Edit tool flavor: `edit_file` (find/replace) or `diff` (unified diff) |
| `--host` | `127.0.0.1` | Bind address; use `0.0.0.0` for remote access |
| `--port` | `8080` | Bind port |
| `--token` | *(auto-generated)* | Bearer token for auth |
| `--read-dir` | *(none)* | Allowed read directories (repeatable) |
| `--write-dir` | *(none)* | Allowed write directories (repeatable) |
| `--shell-allow` | *(none)* | Allowed shell command patterns (repeatable, glob syntax) |
| `--yolo` | `false` | Auto-approve all tool operations |
| `--debug` | `false` | Verbose HTTP request logging |

## Edit format

By default, the `edit_file` tool (find/replace) is exposed. If the connecting LLM handles unified diffs better, use `--edit-format diff` to swap it for the `unified_diff` tool instead. Only one edit tool is exposed at a time to avoid confusing the LLM.

```bash
# Default: find/replace edit tool
term-llm serve mcp --tools all

# Swap to unified diff edit tool
term-llm serve mcp --tools all --edit-format diff
```

## Security

- **Auth**: Bearer token authentication on every request (constant-time comparison). Token is auto-generated if not provided.
- **Localhost by default**: Binds to `127.0.0.1`. You must explicitly pass `--host 0.0.0.0` to accept remote connections.
- **Transport security**: The server uses plain HTTP. When exposing beyond localhost, use an SSH tunnel, VPN, or TLS-terminating reverse proxy to protect traffic in transit.
- **Permissions**: `--read-dir`, `--write-dir`, and `--shell-allow` restrict what the tools can access. Without these flags (and without `--yolo`), tools will prompt for approval. Since the server is non-interactive, you should pre-configure permissions via flags.
- **No `ask_user`**: The server runs non-interactively. Use `--yolo` or permission flags to pre-authorize operations.

## Examples

```bash
# Read-only file browser
term-llm serve mcp --tools read_file,grep,glob \
  --read-dir /var/log --read-dir /etc

# Development server with restricted shell
term-llm serve mcp --tools all \
  --host 0.0.0.0 \
  --write-dir ~/project \
  --shell-allow "go *" --shell-allow "git *" --shell-allow "make *"

# CI/container use: auto-approve everything
term-llm serve mcp --tools all --yolo

# Custom port and token
term-llm serve mcp --tools all --port 9090 --token my-secret-token

# Remote access via SSH tunnel (recommended over --host 0.0.0.0)
ssh -L 8080:localhost:8080 devbox 'term-llm serve mcp --tools all'
```
