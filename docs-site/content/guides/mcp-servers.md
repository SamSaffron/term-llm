---
title: "MCP servers"
weight: 8
description: "Add external tools via Model Context Protocol and use them from term-llm commands."
kicker: "Integrations"
source_readme_heading: "MCP Servers"
featured: true
next:
  label: Agents
  url: /guides/agents/
---
[MCP (Model Context Protocol)](https://modelcontextprotocol.io) lets you extend term-llm with external tools: browser automation, database access, API integrations, and more.

```bash
# Add from registry
term-llm mcp add playwright              # search and install
term-llm mcp add @anthropic/mcp-server-fetch

# Add from URL (HTTP transport)
term-llm mcp add https://developers.openai.com/mcp

# Use with any command
term-llm exec --mcp playwright "take a screenshot of google.com"
term-llm ask --mcp github "list my open PRs"
term-llm chat --mcp playwright,filesystem
```

### MCP Commands

| Command | Description |
|---------|-------------|
| `mcp add <name-or-url>` | Add server from registry or URL |
| `mcp list` | List configured servers |
| `mcp info <name>` | Show server info and tools |
| `mcp run <server> <tool> [args]` | Run MCP tool(s) directly |
| `mcp remove <name>` | Remove a server |
| `mcp browse [query]` | Browse/search the MCP registry |
| `mcp path` | Print config file path |

### Adding Servers

**From the registry** (stdio transport):
```bash
term-llm mcp add playwright           # search by name
term-llm mcp add @playwright/mcp      # exact package
term-llm mcp browse                   # interactive browser
```

**From a URL** (HTTP transport):
```bash
term-llm mcp add https://developers.openai.com/mcp
term-llm mcp add https://mcp.example.com/api
```

### Using MCP Tools

The `--mcp` flag works with all commands (`ask`, `exec`, `edit`, `chat`):

```bash
# Single server
term-llm ask --mcp fetch "summarize https://example.com"
term-llm exec --mcp playwright "take a screenshot of google.com"
term-llm edit --mcp github -f main.go "update based on latest API"

# Multiple servers (comma-separated)
term-llm chat --mcp playwright,filesystem,github

# In chat, toggle servers with Ctrl+M
```

### Running Tools Directly

Use `mcp run` to call MCP tools without going through the LLM:

```bash
# Simple key=value arguments
term-llm mcp run filesystem read_file path=/tmp/test.txt

# JSON arguments for complex values
term-llm mcp run server tool '{"nested":{"deep":"value"}}'

# Multiple tools in one invocation
term-llm mcp run server tool1 key=val tool2 key=val

# Read file contents into a parameter with @path
term-llm mcp run server tool content=@/tmp/big-file.txt

# Read from stdin with @-
cat data.json | term-llm mcp run server tool input=@-
```

### Configuration

MCP servers are stored in `~/.config/term-llm/mcp.json`:

```json
{
  "servers": {
    "playwright": {
      "command": "npx",
      "args": ["-y", "@playwright/mcp"]
    },
    "openai-docs": {
      "type": "http",
      "url": "https://developers.openai.com/mcp"
    },
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "ghp_xxx"
      }
    },
    "authenticated-api": {
      "type": "http",
      "url": "https://api.example.com/mcp",
      "headers": {
        "Authorization": "Bearer your-token"
      }
    }
  }
}
```

### Transport Types

| Type | Config | Description |
|------|--------|-------------|
| stdio | `command` + `args` | Runs as subprocess (npm/pypi packages) |
| http | `url` | Connects to remote HTTP endpoint |

HTTP transport uses [Streamable HTTP](https://modelcontextprotocol.io/specification/2025-03-26/basic/transports) (MCP spec 2025-03-26).

### Serving tools via MCP

`term-llm serve mcp` runs an MCP server over HTTP, exposing term-llm's tools to any MCP client. This is the inverse of the client workflows above. Instead of consuming external tools, you are publishing your local tools for remote use.

See the dedicated [Serving tools via MCP](/guides/serve-mcp/) guide for full details, flags, and examples.
