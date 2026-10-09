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
[MCP (Model Context Protocol)](https://modelcontextprotocol.io) lets you extend term-llm with external tools: browser automation, database access, API integrations, and more. term-llm is an MCP client as defined by the [MCP specification](https://modelcontextprotocol.io/specification/latest).

```bash
# Add from registry
term-llm mcp add playwright              # search and install
term-llm mcp add @anthropic/mcp-server-fetch

# Add from URL (HTTP transport)
term-llm mcp add https://developers.openai.com/mcp

# Add a bundled remote server
term-llm mcp add exa                     # Exa search/fetch MCP

# Use with any command
term-llm exec --mcp playwright "take a screenshot of google.com"
term-llm ask --mcp github "list my open PRs"
term-llm chat --mcp playwright,filesystem
```

`ask` uses the MCP manager’s 30-second startup timeout, or an earlier command deadline. If startup times out, the error names the affected servers.

### MCP Commands

| Command | Description |
|---------|-------------|
| `mcp add <name-or-url>` | Add server from registry or URL |
| `mcp list` | List configured servers |
| `mcp status [name]` | Show transport and safe authentication metadata |
| `mcp login <name>` | Sign in to a protected remote server |
| `mcp logout <name>` | Revoke and remove a stored grant |
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

**Bundled remote servers**:
```bash
term-llm mcp add exa       # Exa web_search_exa and web_fetch_exa over https://mcp.exa.ai/mcp
```

This adds Exa's free remote MCP endpoint. To use your own Exa key with this manually added MCP server, edit `mcp.json` and add an `x-api-key` header. The `search.exa_mcp.api_key` setting applies to term-llm's built-in `search.provider: exa_mcp` path.

**From the web UI**: in `term-llm serve web`, open **MCP servers** and choose **Add**. You can pick from the built-in catalogue (with registry search), paste a remote URL with optional headers, or enter a local command with optional environment variables. New servers are saved to `mcp.json` and turned on for the current chat. To remove a server, open its **⋯** menu and choose **Remove server**; you can undo this for a few seconds. Changing servers from the browser is allowed only when serve requires authentication or the request comes from the same machine. A local command runs on the machine hosting term-llm, with your user's permissions.

`mcp.json` is written atomically with private (`0600`) permissions. If it is a symlink, the file it points to is updated.

### Authentication

How a server authenticates depends on its transport, as the [MCP authorization specification](https://modelcontextprotocol.io/specification/latest/basic/authorization) recommends:

| Server | How credentials are supplied |
|---|---|
| stdio | Environment variables in `env`. MCP OAuth is never used. |
| HTTP with an `Authorization` header in `headers` | The static header is sent as-is. Automatic OAuth is off for that server. |
| HTTP with `"oauth": {"disabled": true}` | No credentials are added. |
| Any other HTTP server | Automatic MCP OAuth, described below. Servers that never ask for authentication connect normally, and nothing is stored for them. |

To keep static tokens and client secrets out of `mcp.json`, see [Keeping secrets out of `mcp.json`](#keeping-secrets-out-of-mcpjson).

### OAuth sign-in for remote servers

term-llm acts as the OAuth client described in the [MCP authorization specification](https://modelcontextprotocol.io/specification/latest/basic/authorization). It discovers the server's authorization server, registers itself if needed, runs a browser sign-in, and stores and refreshes the resulting grant.

Signing in is always something you start. Adding a URL doesn't contact the server, and background connections never open a browser or perform OAuth discovery or client registration. They can still refresh a stored grant. If a server needs authorization and there is no usable grant, it is marked as needing sign-in and its tools are unavailable until you sign in.

```bash
term-llm mcp add https://mcp.example.com/mcp   # saved as "example"
term-llm mcp login example
term-llm mcp status example
```

You can sign in and out from any term-llm surface:

| Surface | Sign in | Sign out |
|---|---|---|
| CLI | `term-llm mcp login <name>` | `term-llm mcp logout <name>` |
| Chat (TUI) | `/mcp login <name>`, or press Enter on a server marked "sign-in required" in the Ctrl+T picker | `/mcp logout <name>` |
| Web UI | **Sign in**, **Sign in again**, or **Retry** on the server's row in **MCP servers** | **Sign out** in the server's **⋯** menu |

All three share the same credential store, so signing in once covers every surface. In serve, a selected server waiting for sign-in reconnects on the next chat request or MCP settings check after another surface stores a usable grant.

If a requested server needs sign-in, `ask`, `exec`, `edit`, `loop`, and jobs stop with an error naming the server and the command to run, rather than continuing without its tools. The command is `term-llm mcp login <name>`, or `term-llm mcp login --force <name>` when the stored grant is still valid but lacks a scope the server requires.

#### `mcp login`

`mcp login` listens on `127.0.0.1` on a random port, prints the authorization URL, opens it in your browser, and waits up to 10 minutes for the redirect to `http://127.0.0.1:<port>/callback`. If the stored grant is still valid, it prints `Already signed in` and stops.

| Flag | Effect |
|---|---|
| `--no-browser` | Print the URL without opening a browser. Over SSH, forward the printed callback port first (for example `ssh -L <port>:127.0.0.1:<port> host`), then open the URL locally. |
| `--force` | Sign in again even when the stored grant is still valid. Use this to pick up new scopes or switch accounts. |

Chat opens the browser directly. If it can't, it cancels the attempt and suggests `term-llm mcp login <name> --no-browser`. Device-code sign-in ([RFC 8628](https://www.rfc-editor.org/rfc/rfc8628)) isn't supported.

#### `mcp status`

`mcp status [name]` reads only local state. It doesn't start any server. For each server it shows the transport, the authentication state, and, once you've signed in, the issuer, granted scopes, access-token expiry, and storage path. It never shows tokens.

| State | Meaning |
|---|---|
| `not needed` | stdio server, static `Authorization` header, or `oauth.disabled` |
| `signed out` | No grant is stored |
| `signed in` | A grant is stored, and its access token has no expiry or is valid for more than five minutes |
| `expired (refreshable)` | The access token expires within five minutes or was rejected with `401` by the MCP server, and the next connection will refresh it |
| `needs sign-in` | The refresh token was rejected (`invalid_grant`), or the access token expired or was rejected with `401` by the MCP server and there's no refresh token |
| `temporary refresh failure (retry)` | A refresh failed for some other reason, such as a network error. The grant is kept. |
| `waiting for browser` | This process has a sign-in in progress |

#### What happens during sign-in

The flow follows the MCP authorization specification and the standards it builds on:

1. **Challenge.** term-llm sends an unauthenticated request and reads the server's `401` [`WWW-Authenticate`](https://www.rfc-editor.org/rfc/rfc6750#section-3) challenge, including any `resource_metadata` and `scope` parameters.
2. **Protected resource metadata** ([RFC 9728](https://www.rfc-editor.org/rfc/rfc9728)). It fetches the `resource_metadata` URL from the challenge if one is given. Otherwise it tries `/.well-known/oauth-protected-resource/<path>` and then `/.well-known/oauth-protected-resource`. If none of these exist, it falls back to the [2025-03-26 behaviour](https://modelcontextprotocol.io/specification/2025-03-26/basic/authorization#server-metadata-discovery) and treats the server's origin as the authorization server.
3. **Authorization server metadata** ([RFC 8414](https://www.rfc-editor.org/rfc/rfc8414) or [OpenID Connect Discovery](https://openid.net/specs/openid-connect-discovery-1_0.html)). Both the path-inserted and root well-known locations are tried. If the server publishes no metadata, the 2025-03-26 default endpoints `/authorize`, `/token`, and `/register` are used.
4. **[Client registration](https://modelcontextprotocol.io/specification/latest/basic/authorization/client-registration)**, using the first option that applies:
   - A [Client ID Metadata Document](https://datatracker.ietf.org/doc/draft-ietf-oauth-client-id-metadata-document/), but only when you set `oauth.client_id_metadata_url` and the authorization server advertises `client_id_metadata_document_supported`. term-llm doesn't publish a metadata document of its own.
   - A pre-registered client: `oauth.client_id`, plus a secret if you configure one. The spec prefers pre-registration over a metadata document; term-llm checks the metadata document first, so don't set both unless you want the document to win.
   - Otherwise, [Dynamic Client Registration](https://www.rfc-editor.org/rfc/rfc7591) as a public native client named `term-llm`, with `token_endpoint_auth_method: none`. The issued client is stored with the grant and reused for refreshes and later sign-ins. A new loopback port doesn't require a new registration, because [RFC 8252 §7.3](https://www.rfc-editor.org/rfc/rfc8252#section-7.3) lets loopback redirects vary by port.
5. **Authorization.** term-llm opens an authorization-code request with [PKCE](https://www.rfc-editor.org/rfc/rfc7636) `S256`, a `state` value, and a [`resource` indicator](https://www.rfc-editor.org/rfc/rfc8707) set to the server URL. The callback accepts each `state` once, and the `iss` response parameter is checked against the expected issuer ([RFC 9207](https://www.rfc-editor.org/rfc/rfc9207)).
6. **Token exchange.** A refresh token is requested, and `offline_access` is added to the scopes when the authorization server lists it.

After sign-in, the access token is sent as a `Bearer` header on every request to the MCP server. It is never put in a URL.

#### Scopes

Which scopes are requested depends on `oauth.scopes`:

| `oauth.scopes` | Scopes requested |
|---|---|
| Omitted (default) | The challenge's `scope`, plus every scope in the authorization server's `scopes_supported` |
| `["read", "write"]` | The listed scopes plus the challenge's `scope` |
| `[]` | Only the challenge's `scope`. If the challenge has none, the protected-resource metadata's `scopes_supported` (the spec's own default). |

The default asks for more than the spec's least-privilege [scope selection strategy](https://modelcontextprotocol.io/specification/latest/basic/authorization#scope-selection-strategy). That strategy uses the protected resource's `scopes_supported`, which is meant to be a minimal set, rather than the authorization server's full list. Set `oauth.scopes` if a server advertises more scopes than you want to grant.

Once a grant exists, the configured scopes are combined with the scopes granted earlier. This matches the spec's [step-up](https://modelcontextprotocol.io/specification/latest/basic/authorization#step-up-authorization-flow) rule, which keeps permissions from being lost. As a result, removing scopes from `oauth.scopes` and running `mcp login --force` doesn't reduce them. To narrow a grant, run `mcp logout` and then `mcp login`.

#### Refresh and storage

The access token is refreshed when it is within five minutes of expiry. A rejected refresh (`invalid_grant`) moves the server to `needs sign-in`. Other failures keep the grant and show `temporary refresh failure (retry)`.

If the MCP server rejects the stored access token with `401`, term-llm marks that token expired and keeps the refresh token. The next connection refreshes it once; without a refresh token, the server needs sign-in, and a plain `mcp login` works without `--force`.

A `403 insufficient_scope` means the token lacks a scope for that request ([step-up](https://modelcontextprotocol.io/specification/latest/basic/authorization#step-up-authorization-flow)). That call fails, and the token stays valid for everything else. If it happens during startup, the server is marked as needing sign-in. To get the missing scope, run `term-llm mcp login --force <name>`, choose **Sign in again** in the Web UI, or press Enter on the server in the chat MCP picker. Sign-in requests the scopes described in [Scopes](#scopes), so if you set `oauth.scopes`, add the missing scope there first.

Registrations and grants are stored in `$XDG_CONFIG_HOME/term-llm/mcp_oauth.json` (normally `~/.config/term-llm/mcp_oauth.json`). They're keyed by the server URL, normalized to lowercase scheme and host with any default port and fragment removed; path and query are kept. Two `mcp.json` entries with the same URL therefore share one grant, and renaming an entry keeps its grant.

The store directory (the term-llm config directory) is set to `0700`, and the file and its lock to `0600`. Writes are atomic, and refreshes are serialized across term-llm processes so a rotated refresh token is never lost. Treat this file as a credential: don't commit it or expose it to a browser. The serve API never returns tokens or client secrets, and the Web UI never stores them.

#### Sign-out

`mcp logout <name>` revokes the stored refresh token, or the access token if there's no refresh token ([RFC 7009](https://www.rfc-editor.org/rfc/rfc7009)), and then deletes the local grant. Revocation is attempted only if the authorization server's metadata advertised a `revocation_endpoint` at sign-in. The local grant is deleted even when revocation fails, and the command then exits with the revocation error. `--local-only` skips revocation. Logging out when nothing is stored does nothing, so it's safe to repeat.

#### OAuth settings in `mcp.json`

Most servers need no OAuth settings. Use the `oauth` object when the authorization server requires a pre-registered client, when you host your own Client ID Metadata Document, or when the server advertises more scopes than you want to grant:

```json
{
  "servers": {
    "private-remote": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "oauth": {
        "client_id": "registered-client",
        "client_secret_env": "MCP_CLIENT_SECRET",
        "scopes": ["read", "write"]
      }
    }
  }
}
```

| Field | Meaning |
|---|---|
| `client_id` | Pre-registered client ID. Without it, term-llm uses Dynamic Client Registration. |
| `client_secret` | Secret for a confidential pre-registered client. Accepts deferred values (`op://`, `srv://`, `file://`, `$(...)`, `${VAR}`), resolved only when the server connects or sign-in starts. A reference that resolves to an empty value is an error. |
| `client_secret_env` | Name of an environment variable holding the secret. Ignored when `client_secret` is set. An unset or empty variable is an error. |
| `scopes` | Scopes to request instead of the default. See [Scopes](#scopes). |
| `client_id_metadata_url` | HTTPS URL of a [Client ID Metadata Document](https://datatracker.ietf.org/doc/draft-ietf-oauth-client-id-metadata-document/) you host. Used only when the authorization server advertises support for it. |
| `disabled` | `true` turns off automatic OAuth without adding a static header. |

Don't write secrets literally into `mcp.json`; use `client_secret_env` or a deferred `client_secret`.

#### Web sign-in through `term-llm serve`

Enabling a server that needs sign-in keeps it selected and shows **Sign in** on its row. You can continue chatting with the ready servers' tools while it waits for authorization.

In the Web UI, sign-in opens a popup. If the popup is blocked, the server's row offers the sign-in link to copy instead. The redirect goes to `<base>/v1/mcp/oauth/callback` on the serve process. By default the callback URL is built from the scheme and `Host` of the request that started sign-in. Set `--public-url` (or `TERM_LLM_SERVE_PUBLIC_URL`) when the URL the browser sees is different, for example behind a TLS-terminating proxy. The public URL must include the base path, such as `https://chat.example.com/ui/`. A node mounted behind `serve hub` must set it to its hub mount, such as `https://hub.example/node/<id>`, because the hub strips forwarding headers.

The callback route can't carry the serve bearer token or passkey session, so it skips serve authentication. Instead, the single-use OAuth `state` value is what authorizes the request. All other routes use normal serve authentication and are relative to the base path (`/ui` by default):

| Route | Purpose |
|---|---|
| `POST /v1/sessions/:id/mcp/:server/oauth/start` | Start sign-in. Body `{"force": false}`. Returns `202` with `flow_id`, `authorization_url`, `expires_at`, and `state`. |
| `POST /v1/sessions/:id/mcp/:server/oauth/cancel` | Cancel a pending sign-in. Body `{"flow_id": "..."}`. |
| `DELETE /v1/sessions/:id/mcp/:server/oauth` | Sign out, including remote revocation. |
| `GET /v1/mcp/oauth/flows/:flow_id` | Poll flow state: `starting`, `pending`, `succeeded`, `failed`, `canceled`, or `expired`. |
| `GET /v1/mcp/oauth/callback` | Authorization-server redirect target. |

Session routes return `409` while that session is generating a response. A sign-in expires after 10 minutes. When it succeeds, servers enabled in the session at completion reconnect. A server turned off while sign-in was pending stays off.

### Using MCP Tools

The `--mcp` flag works with all commands (`ask`, `exec`, `edit`, `chat`):

```bash
# Single server
term-llm ask --mcp fetch "summarize https://example.com"
term-llm exec --mcp playwright "take a screenshot of google.com"
term-llm edit --mcp github -f main.go "update based on latest API"

# Multiple servers (comma-separated)
term-llm chat --mcp playwright,filesystem,github

# In chat, open the MCP picker with Ctrl+T; press Enter to toggle a server.
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

### Keeping secrets out of `mcp.json`

`headers`, `env`, and `oauth.client_secret` values support deferred resolution, so the file itself can be tracked in dotfiles:

```json
{
  "servers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "op://Private/GitHub/token"
      }
    },
    "authenticated-api": {
      "type": "http",
      "url": "https://api.example.com/mcp",
      "headers": {
        "Authorization": "$(my-secret-tool read api-token)"
      }
    }
  }
}
```

Accepted forms are `op://`, `srv://`, `file://` (optionally with `#nested.field` for a JSON or YAML file), `$(...)` and `${VAR}`. A bare `$VAR` stays literal; `${VAR}` is an explicit environment reference. References replace the entire value: `Bearer ${TOKEN}` is not interpolated, so resolve the complete authorization header instead. Values are resolved when the server is connected, not when the config is read, and non-empty successful deferred results are memoized for the life of the process. Empty results from explicitly deferred header or environment values are errors; literal empty values are still allowed. Restart long-running processes after rotating secrets.

Only use trusted `mcp.json` files: `$(...)` executes shell commands with your user permissions. See [Secret management](/guides/secret-management/) for 1Password, macOS Keychain, and Linux keyring recipes, or [keep config and MCP credentials in one private `secrets.yml`](/guides/secret-management/#keep-all-your-configured-secrets-in-secretsyml).

### Deferred tool discovery

term-llm keeps small MCP catalogues simple and eagerly sends all schemas. When the complete authorised MCP catalogue exceeds 24 tools, it defers the long tail and lets the model search for the few schemas needed for the task.

Configure the policy in the main `config.yaml`:

```yaml
tool_discovery:
  mode: auto
  strategy: auto
  threshold: 24
  max_active_tools: 300
```

`mode` controls whether tools are deferred:

| Mode | Behaviour |
|---|---|
| `auto` | Eager at or below `threshold`; deferred above it |
| `eager` | Always send all authorised MCP schemas |
| `deferred` | Always defer eligible MCP schemas |

`strategy` controls how deferred schemas reach the model:

| Strategy | Behaviour |
|---|---|
| `auto` | Use an exactly supported provider-native path; otherwise use portable search |
| `portable` | Use term-llm's ordinary cross-provider `tool_search` tool |
| `native` | Require provider-native loading and fail clearly when unsupported |

`max_active_tools` limits the dynamic provider-visible working set, not the number of tools that may be discovered during a session. It defaults to 300; `0` also selects that default rather than disabling discovery. When the working set is full, term-llm first evicts an unpinned tool that has not yet been sent to the provider, then prefers tools that have never been called before falling back to least-recently-used called tools. Pinned and `always_load` tools do not consume this dynamic limit and are never evicted. Eviction only changes schema visibility, not authorization: evicted tools remain searchable and can be activated again. If every eligible victim has already been sent, changing the visible schema set may reset provider conversation or prompt-cache state to keep the next request correct.

ChatGPT OAuth `gpt-5.6-luna` supports native client-executed search. Qwen/Ollama and conventional providers use portable search. Native loading keeps selected schemas in provider discovery output rather than rebuilding the ordinary top-level tool array. Selected MCP tools are grouped under their server namespace on the ChatGPT wire, but discovery, authorization, and execution remain child-granular: selecting one child never loads its siblings. Namespace descriptions use bounded MCP server instructions/metadata when available.

The provider-neutral catalogue still retains each tool's flattened executable name (`server__tool`) alongside explicit namespace and child identity. Portable discovery and function-only providers continue using the flattened name, so sessions can move across providers. Native namespace calls are routed through the explicit identity metadata and must match a currently loaded, authorised child; term-llm does not infer authorization by splitting a name or by accepting a namespace alone.

Pin frequent tools so they remain immediately visible when the catalogue is deferred. `always_load` uses the original server tool name:

```json
{
  "servers": {
    "github": {
      "command": "github-mcp-server",
      "always_load": [
        "get_pull_request",
        "search_issues"
      ]
    }
  }
}
```

Discovery changes schema visibility only. MCP server enablement and the engine's allowed-tools policy remain authoritative for execution.

### Transport Types

| Type | Config | Description |
|------|--------|-------------|
| stdio | `command` + `args` | Runs as subprocess (npm/pypi packages) |
| http | `url` | Connects to remote HTTP endpoint |

Both follow the [MCP transports specification](https://modelcontextprotocol.io/specification/latest/basic/transports). HTTP servers use [Streamable HTTP](https://modelcontextprotocol.io/specification/latest/basic/transports/streamable-http); the legacy HTTP+SSE transport from the 2024-11-05 revision isn't supported. term-llm uses the official [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk), which negotiates protocol revisions from 2024-11-05 up to [2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28). See [Authentication](#authentication) for credentials.

### Serving tools via MCP

`term-llm serve mcp` runs an MCP server over HTTP, exposing term-llm's tools to any MCP client. This is the inverse of the client workflows above. Instead of consuming external tools, you are publishing your local tools for remote use.

See the dedicated [Serving tools via MCP](/guides/serve-mcp/) guide for full details, flags, and examples.
