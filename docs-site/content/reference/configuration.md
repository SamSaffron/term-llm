---
title: "Configuration"
weight: 2
description: "Inspect, edit, and understand the main config file, provider settings, search configuration, and per-command overrides."
kicker: "Config"
featured: true
next:
  label: Providers and models
  url: /reference/providers-and-models/
---
## Configuration commands

```bash
term-llm config
term-llm config edit
term-llm config path
term-llm config get default_provider
term-llm config set default_provider zen
term-llm config reset
term-llm config completion fish
```

The main config file lives at:

```text
~/.config/term-llm/config.yaml
```

## Configuration shape

A typical config has a few major parts:

- `default_provider` for the global LLM default
- `providers` for model-specific credentials and routing
- per-command blocks such as `exec`, `ask`, and `edit`
- feature-specific blocks such as `image`, `audio`, `music`, `embed`, `search`, `sessions`, `file_tracking`, `tools`, and `skills`

## Example

```yaml
default_provider: anthropic

providers:
  anthropic:
    model: claude-sonnet-4-6

  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
    credentials: codex
    # WebSocket transport is enabled by default for built-in OpenAI.
    # Set false to force HTTP/SSE.
    use_websocket: true

  xai:
    model: grok-4-1-fast

  nearai:
    model: zai-org/GLM-5.1-FP8
    fast_model: Qwen/Qwen3.6-35B-A3B-FP8

  sambanova:
    model: gpt-oss-120b
    fast_model: Meta-Llama-3.3-70B-Instruct

  claude-bin:
    model: opus
    env:
      IS_SANDBOX: "1"

  openrouter:
    model: x-ai/grok-code-fast-1
    app_url: https://github.com/samsaffron/term-llm
    app_title: term-llm

exec:
  suggestions: 3
  instructions: |
    I use Arch Linux with zsh.
    I prefer ripgrep over grep and fd over find.

ask:
  max_turns: 50
  instructions: |
    Be concise. I'm an experienced developer.

chat:
  max_turns: 200
  # smart: update terminal tab titles and tmux window names.
  # basic: only emit the terminal/tab OSC title.
  # off: do not touch terminal/window titles at all.
  terminal_title: smart

# Optional global approval override. When omitted, chat, ask, and ordinary
# serve platforms use auto; edit, exec, loop, and serve mcp use prompt.
# approval:
#   default_mode: prompt

guardian:
  # Optional: override the provider/model used for auto approval review.
  # Without overrides, Guardian uses the fast model on default_provider.
  # It never inherits per-command, per-surface, session, or agent overrides.
  # provider: anthropic
  # model: claude-sonnet-4-6
  # policy_path: ~/.config/term-llm/guardian-policy.md
  # Review timeout in seconds. Defaults to 90.
  # timeout_seconds: 90
  # Suspend every shell pattern while auto mode is active.
  classify_all_shell: false

edit:
  model: gpt-5.2-codex
  diff_format: auto

search:
  provider: exa_mcp
  fetch_provider: jina

  exa_mcp:
    url: https://mcp.exa.ai/mcp # optional; this is the default
    api_key: ${EXA_API_KEY} # optional

tools:
  max_tool_output_chars: 20000
```

## Approval modes

A blank configuration uses these built-in defaults:

| Surface | Default |
|---------|---------|
| `chat`, `ask`, ordinary `serve` platforms | `auto` |
| `edit`, `exec`, `loop`, `serve mcp` | `prompt` |

Ordinary `serve` means the `web`, `api`, `jobs`, and `telegram` platforms, including combined launches. To start one in human approval mode instead, pass `--approval prompt` or set `serve.approval_mode: prompt`. Standalone `term-llm serve mcp` remains prompt by default.

In **prompt** mode, actions outside deterministic file/directory and shell allowlists ask before proceeding. In **auto** mode, deterministic exact approvals and usable narrow patterns still run first. Guardian reviews unmatched shell commands, file reads, file writes and edits/diffs, grep/glob directory searches, image input/output paths, and explicit additional-workspace grants/elevations. Auto does not review MCP/network/service actions and is not yolo. Shell approvals from Guardian are cached only for the exact command and working directory; ordinary per-file reviews do not silently create workspace grants.

Local launches record the canonical runtime/project directory as a per-session **proposed primary workspace**, initially with zero file authority. `term-llm chat` asks for direct human whole-workspace read/write confirmation at startup whenever a registered local file, search, or image tool could need that authority; other interactive surfaces ask on first path access inside the proposal. This decision happens before Guardian or deterministic file rules. The prompt shows the workspace root, defaults to **Always allow** (remember it for future sessions), and also offers **Allow this session** or **Deny**; shell remains separately controlled. Esc/Ctrl+C on the proactive startup prompt defers the decision until a path tool is used, while explicit **Deny** remains latched for the session. Session-only allow is persisted with the root session so resume does not reprompt. Remembered allow stores the exact canonical root in `$XDG_CONFIG_HOME/term-llm/remembered-workspaces.yaml` (falling back to `~/.config`) and applies to later sessions with that same root. Deny blocks workspace path access, and no confirmation transport fails closed unless the workspace was previously remembered. While yolo is effective, file access bypasses this confirmation without prompting or persisting workspace authority; after leaving yolo, the next access prompts if the primary remains unconfirmed and unremembered. Worktree rebinding invalidates a mismatched primary confirmation while preserving additional grants.

Serve/web never derives even a proposal from daemon CWD. Only explicit request/session state can propose a workspace, and web uses the same dedicated first-access confirmation modal outside yolo. Before a session/request/worktree is explicitly bound, relative local-tool paths and shell calls without an absolute `working_dir` fail closed rather than using daemon CWD; absolute targets still pass through normal approval checks. Guardian and headless/synchronous operation cannot substitute for the human workspace decision; explicit yolo bypasses it for tool access without establishing primary authority.

Whenever any local file/search/image path tool is enabled—including through an explicit tool list—term-llm also exposes `manage_workspace`. Additional workspaces default to read-only, are canonicalized and narrowed to their enclosing Git worktree root when applicable, and can be explicitly elevated to write. Auto mode Guardian-reviews each new grant and each read-to-write elevation once; duplicate/weaker grants are idempotent. Grants can be listed or revoked, are shared immediately with child agents, never grant shell/MCP/network authority, and never modify project/global approvals. SQLite sessions persist non-yolo grants across resume and copy them to conversation branches; stores without the optional persistence capability keep runtime-only grants. Yolo grants are always ephemeral overlays: they are never persisted or branched, disappear throughout the parent/child tree on leaving yolo, and a temporary write elevation restores any durable read grant beneath it.

Choose a mode for one invocation with `--approval`:

```bash
term-llm chat --approval prompt   # conservative override of chat's auto default
term-llm serve web --approval prompt # conservative override of serve's auto default
term-llm ask --approval auto "fix this"
term-llm loop --approval yolo --done "go test ./..." "fix tests"
```

`--auto` and `--yolo` remain compatibility aliases for `--approval auto` and `--approval yolo`. The three flags are mutually exclusive. Yolo is CLI-only: it cannot be configured and is never restored on a cold resume.

Set one persistent global default for all surfaces:

```yaml
approval:
  default_mode: prompt # prompt or auto
```

Or override individual surfaces:

```yaml
approval:
  default_mode: auto

edit:
  approval_mode: prompt

serve:
  approval_mode: prompt # override ordinary serve's auto default
  projects:
    enabled: true
  mcp:
    approval_mode: prompt
```

Configuration accepts only `prompt` and `auto`. An omitted or empty surface value inherits an explicitly configured `approval.default_mode`; if neither is set, the built-in matrix applies. `term-llm config show` lists each effective surface mode and its resolution source, and `term-llm config get chat.approval_mode` reports the effective value when the key is unset. Resolution precedence is explicit CLI mode, persisted session mode (for chat resume), per-surface config, global config, then the built-in default. A legacy resumed chat with no stored mode resumes in prompt, and stored yolo is downgraded to prompt.

### Web project registry

`term-llm serve web` enables the project registry by default. A project is a shared, durable pointer to an existing directory on the server. Configure it with:

```yaml
serve:
  projects:
    enabled: true
```

Use `--no-projects` for single-workspace mode. This is the intended setup for dedicated personal agents and agent containers whose persistent workspace is the whole environment, rather than one project among many. It keeps the flat/date sidebar and binds first-party default-workspace requests to the serve startup directory's main Git root; a non-Git startup remains unbound. Use `--projects` for strict project-registry opt-in: startup fails if project storage is read-only/unsupported or the only bootstrap candidate is filesystem root. Default-enabled project startup is rollout-tolerant and instead warns and falls back to single-workspace mode for those cases. Project initialization runs only when the `web` platform is selected; API-, jobs-, Telegram-, and Hub-delegated work do not acquire an implicit project.

Project paths are absolute paths on the machine running term-llm. Symlinks are resolved. A directory inside a Git checkout is normalized to the main repository root; a non-Git directory remains exact. Filesystem root cannot be registered—use `--no-projects` single-workspace mode with the existing read/write directories, grants, shell policy, and approval mode for container-wide agents.

A registered project is metadata, not authorization. Selecting one does not bypass workspace confirmation, static read/write allowlists, shell approval, or Guardian.

Auto mode is intentionally narrower than yolo:

- `prompt`: ask before unapproved tool actions; existing pattern rules retain their prior behavior.
- `auto`: Guardian reviews supported unmatched local operations and fails closed on denials/review failures.
- `yolo`: auto-approve tool actions without prompting (explicit CLI use only).

Interactive Guardian initialization failure produces one warning and temporarily uses prompt mode; the requested auto policy remains saved so a later resume can retry. Headless auto runtimes fail startup if Guardian cannot initialize. Because ordinary `serve` now uses the auto built-in default, an unconfigured serve launch follows this fail-fast path; use `--approval prompt` or `serve.approval_mode: prompt` to opt into human approval mode instead. Once auto is running, a Guardian denial, contradictory allow, timeout, malformed response, unavailability, or transport failure returns an error to the agent in terminal, web/serve, and headless runtimes. It does not immediately fall through to a human prompt for that action.

Configure Guardian review with:

```yaml
guardian:
  provider: anthropic       # optional override
  model: claude-sonnet-4-6  # optional authoritative model pin
  policy_path: ~/.config/term-llm/guardian-policy.md # optional custom policy
  timeout_seconds: 90       # optional; default 90
  classify_all_shell: false # true sends every shell pattern through Guardian
```

Without explicit Guardian overrides, resolution uses the configured `fast_model` for the global `default_provider`, switching to its `fast_provider` when configured. Per-command, per-surface, session, and agent provider/model overrides do not change that target. `guardian.provider` explicitly selects another provider and its fast model without following that provider's `fast_provider`; `guardian.model` pins the model on `guardian.provider` or, when omitted, on the global default provider. For compatibility, a custom or local provider with no `fast_model` uses its configured `model` before considering a built-in fast default.

In auto mode, arbitrary-execution shell patterns are suspended before matching. The mechanical set includes executable globs (`*`, `*/bin/*`) plus wildcard arguments to interpreters, shells, elevation tools, and common dispatchers. Examples include `python *`, `python *.py`, `/usr/bin/python3 *`, `node *.js`, `bash *`, `env *`, `sudo *`, `uv run *`, `npx *`, and `pipx run *`. Existing generated rules of this form continue to work after an explicit switch to prompt mode but are intentionally ignored while auto is requested. Narrow fixed commands such as `git status`, `go test *`, `npm test`, `python script.py`, `uv run pytest`, `npx eslint`, and `pipx run black` remain deterministic unless `classify_all_shell` is true. Exact configured scripts, exact session commands, and exact Guardian command/workdir approvals remain active either way.

Configured, session, ancestor, and project pattern sets remain independent; term-llm does not union them to cover different segments of a compound command. Safe pipe targets can satisfy only a later pipe segment after the head has a usable deterministic approval, so they cannot restore a suspended head. A suspended pattern is not a deny: the unresolved exact action goes to Guardian.

Three consecutive Guardian policy denials or 20 total policy denials in the current auto epoch suspend auto across the root manager and all child agents. Successful Guardian approval resets only the consecutive count; reviewer failures do not count. The threshold-triggering action remains denied and the next approval-bearing action uses effective prompt mode. The requested auto policy remains responsible for shell-pattern filtering while suspended, so arbitrary-execution rules—or all rules under `classify_all_shell`—cannot become implicit post-breaker approvals. In the TUI, the first Shift+Tab after suspension calls the explicit auto-resume path, clears the latch/counters, and starts a fresh epoch; it does not jump directly to yolo. An explicit switch out of and back into auto also starts a fresh epoch. Suspension affects runtime behavior only, not the requested/persisted policy, so a cold resume may enter auto with a fresh epoch. Web/serve has no direct mode-cycle control from this feature; its existing approval transport handles later prompt-mode requests.

To override a denial deliberately, explicitly authorize the exact action in a subsequent message so Guardian can reassess it using the new trusted transcript evidence, or switch the session to prompt/yolo using existing controls. Guardian never directly offers a wildcard command pattern, directory, or repository grant after a denial.

> Privacy note: Guardian review receives approval evidence, including recent transcript snippets, tool call arguments/results, and deterministic approval context. If `guardian.provider` or the selected `fast_provider` differs from the chat provider, that evidence is sent to the resolved Guardian provider as well.

## Per-command overrides

Each command can override provider and model independently of the global default.

```yaml
default_provider: anthropic

providers:
  anthropic:
    model: claude-sonnet-4-6
  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
  zen:
    model: glm-4.7-free

exec:
  provider: zen
  model: glm-4.7-free

ask:
  model: claude-opus-4

edit:
  provider: openai
  model: gpt-5.2-codex
```

Precedence is:

1. CLI flag such as `--provider openai:gpt-5.2`
2. per-command config such as `exec.provider` or `ask.model`
3. global provider selection via `default_provider` and `providers.<name>.model`

## Agentic turn limits

Agentic commands can make multiple provider calls while they execute tools and feed results back to the model. `max_turns` caps that loop.

Defaults:

- `ask.max_turns`: `50`
- `exec` CLI flag default: `50`
- `chat.max_turns`: `200`
- Agent YAML `max_turns` overrides command/config defaults when an agent is selected.
- A CLI `--max-turns N` flag overrides both config and agent YAML.

```yaml
ask:
  max_turns: 50

chat:
  max_turns: 200
```

## Parallel tool execution

Models may request many independent tool calls in a single turn, such as several `read_file`, `grep`, or `glob` calls. term-llm executes independent tool calls concurrently when parallel tool calls are enabled by the provider/request, but caps one model turn at **20 concurrently running tool calls**. Additional tool calls from the same turn are queued and run as earlier calls finish.

This is a built-in safety limit rather than a config option today. It preserves useful batching while preventing a single response from spawning an unbounded number of shells, greps, reads, or subagents at once.

## Chat terminal titles

`chat.terminal_title` controls whether interactive chat updates terminal titles:

- `smart` (default): update the terminal/tab title and, inside tmux, rename the current tmux window.
- `basic`: emit only the terminal/tab title escape; tmux window names and Ghostty progress are not managed.
- `off`: emit no title/progress escape sequences and do not rename tmux windows.

```yaml
chat:
  terminal_title: smart
```

`chat.terminal_title_format` optionally customizes the text used for both the terminal/tab title and smart tmux window title. Leave it empty for the built-in smart format (`task · branch · agent · model`, with the `branch` segment omitted for root sessions and with streaming/attention variants).

```yaml
chat:
  terminal_title: smart
  terminal_title_format: "[{{env.DOCKER_CONTAINER_NAME}}] {{agent}} · {{task}} · {{model}}"
```

Supported placeholders:

- `{{title}}` - the built-in title for the current state.
- `{{stable_title}}` - built-in title without elapsed streaming seconds; useful for low-churn window names.
- `{{agent}}` - current agent name.
- `{{task}}` - session/task title, generated from the conversation when available.
- `{{model}}` - display model name.
- `{{branch}}` - `branch` when the current session is a child conversation path, otherwise empty.
- `{{phase}}` - current phase, such as `Thinking` or `Responding`.
- `{{state}}` - `idle`, `streaming`, or `attention`.
- `{{activity}}` - model name while idle, streaming activity while streaming, or `attention` while waiting for input.
- `{{elapsed}}` - streaming elapsed time such as `12s` when available.
- `{{attention}}` / `{{attention_marker}}` - `‼` when approval/ask-user/handover attention is needed, otherwise empty.
- `{{env "NAME"}}` or `{{env.NAME}}` - environment variable from the chat process. Use this sparingly: titles may appear in the terminal, window manager, tmux, screen shares, and logs. Do not include secrets.

The format is a Go `text/template`, so pipelines work too. A small `default` helper returns its first argument when the piped value is empty:

```yaml
chat:
  terminal_title_format: "[{{env \"DOCKER_CONTAINER_NAME\" | default \"host\"}}] {{title}}"
```

Title text is sanitized before emission: control characters are removed, whitespace is collapsed, and very long results are truncated.

Ghostty honors `OSC 2` title updates but may override or ignore program-supplied titles. In `smart` mode, term-llm emits Ghostty's `OSC 9;4` indeterminate progress indicator during chat turns and clears it afterward. Inside tmux, term-llm uses passthrough so Ghostty can receive the sequence; tmux 3.3+ may require `set -g allow-passthrough on` in `~/.tmux.conf`.

Ghostty title caveats:

- A Ghostty config `title = ...` forces a fixed title and ignores title escape sequences.
- A configured `title-command` takes precedence over title escape sequences.
- A title set with Ghostty's title prompt/actions (for example `prompt_surface_title`, `prompt_tab_title`, `set_surface_title`, or `set_tab_title`) can override terminal-supplied titles until cleared.
- Ghostty shell integration's `title` feature rewrites the title before/after shell commands. If you want manual/program titles to persist at the shell prompt, set this in Ghostty config and reload/restart the shell:

```ini
shell-integration-features = no-title
```

To debug Ghostty title handling outside term-llm, run this from a Ghostty shell:

```sh
printf '\033]2;ghostty title test\a'; sleep 10
```

To debug the Ghostty header progress indicator:

```sh
printf '\033]9;4;3\a'; sleep 10; printf '\033]9;4;0\a'
```

If the title changes only during `sleep` and resets afterward, Ghostty shell integration is overwriting it at the prompt. If it never changes, check for a fixed `title`, `title-command`, or a manual surface/tab title override.

## Reasoning and thinking display

Reasoning display controls how provider-marked thinking/summary content is shown in term-llm. It is separate from provider reasoning effort suffixes such as `openai:gpt-5.2-high`, `anthropic:...-thinking`, or `vllm` provider `-high`.

By default, term-llm shows display-safe provider summaries and non-encrypted provider thinking as collapsed thought blocks in interactive chat:

- Generic provider thinking renders as `▸ Thinking...`.
- Provider/summary titles render as `▸ Thought: <title>`.
- Expanding a block shows the body; encrypted reasoning/signatures are never displayed.
- In chat, `Ctrl+E` toggles thought detail globally and clicking a thought header toggles that block.
- Ctrl+O inspector shows non-encrypted reasoning details for saved messages.

Default policy:

```yaml
reasoning:
  display: auto                  # auto => collapsed
  source: summary_or_provider_safe
  status: title
  history: collapsed
  export: ask
  raw: false
  max_summary_chars: 12000
  max_raw_chars: 20000
  extract_titles: true
  hidden_label: Thinking...
  persist_summaries: true
```

Important options:

| Field | Values | Meaning |
|---|---|---|
| `display` | `auto`, `off`, `status`, `collapsed`, `expanded`, `raw` | Interactive display mode. `raw` still requires `raw: true`; otherwise it falls back to collapsed. |
| `source` | `summary_only`, `summary_or_provider_safe`, `all` | Which provider reasoning sources interactive UI may show. Raw export/replay requires `all`. |
| `status` | `none`, `generic`, `title`, `summary` | How reasoning affects the live status/spinner text. |
| `history` | `none`, `collapsed`, `expanded`, `transcript_only` | Whether saved/streamed thought blocks are visible in chat history. |
| `export` | `never`, `ask`, `summaries`, `raw` | What session export may include. Raw export also requires `raw: true` and `source: all`. |
| `raw` | boolean | Explicit safety gate for raw reasoning display/export. |
| `hidden_label` | string | Label for untitled collapsed blocks, default `Thinking...`. |

Per-surface overrides inherit the top-level reasoning policy:

```yaml
reasoning:
  display: collapsed
  chat:
    display: expanded
  ask:
    status: title
    history: none
  serve:
    display: off
```

For local debugging only, `TERM_LLM_SHOW_RAW_REASONING=1` forces `display: raw`, `source: all`, and `raw: true` for the resolved surface.

## Sessions config

```yaml
sessions:
  enabled: true
  max_age_days: 0
  max_count: 0
  path: ""
  strip_image_base64: false
```

Use this to control whether sessions are persisted, how long they are kept, and where the SQLite database lives. By default, uploaded image base64 is kept in the DB for portability; set `strip_image_base64: true` to store only image paths/metadata when a local `ImagePath` exists, reducing DB size at the cost of requiring the uploads directory to move with the database.

## File change tracking config

```yaml
file_tracking:
  enabled: false
  max_file_bytes: 2097152 # 2 MiB per-file content cap
  max_session_bytes: 104857600 # 100 MiB retained content per session
  max_total_bytes: 1073741824 # 1 GiB attributed-history data cap across sessions
  max_observation_rows: 10000
  max_observation_session_rows: 1000
  max_observation_bytes: 16777216 # 16 MiB independent metadata cap
  max_observation_session_bytes: 2097152 # 2 MiB per session
  max_observation_age_days: 30
  path: "" # optional attributed DB path override; observation sidecar shares its directory
```

Opt-in. When enabled, term-llm records retained before/after content only for attributed transitions: trusted direct mutation tools, or shell `transform`/`generate` claims declared before execution and subsequently verified. Materializations and unclaimed effects are metadata-only observations and never contribute Agent Changes totals.

Enable it with:

```bash
term-llm config set file_tracking.enabled true
```

or by adding the YAML above to your config file. To enable tracking only for one server invocation without changing the saved config, use:

```bash
term-llm serve web --enable-file-tracking
```

**Privacy note:** attributed content is persisted to `~/.local/share/term-llm/file_history.db`, separate from `sessions.db`. Retained blobs are gzip-compressed and content-addressed. Oversized, unsupported binary, unknown-side, and over-budget transitions keep provenance metadata but not a fabricated diff. `max_total_bytes` applies to growing attributed data with a small fixed structural reserve for mandatory indexes. History for deleted sessions is swept on startup, following `sessions.max_age_days`.

Observations and materializations contain bounded counts, roots, sampled paths, coverage, and closed diagnostic details only—never blobs, command text, environment values, headers, credentials, URL userinfo, or unsanitized URLs. They live in the physically separate `file_observations.db` sidecar and have independent row, byte, per-session, and age limits. Observation pressure therefore cannot evict attributed blobs.

`affected_paths` bounds shell inspection only and does not attribute transitions. Use pre-execution `output_claims` for task transforms/generators and `materialize` claims for imported content. Without claims, bounded Git/session-path discovery may produce observations, but never agent changes. Incomplete coverage and tracker failures are surfaced as diagnostics.

## MCP tool discovery config

```yaml
tool_discovery:
  mode: auto
  strategy: auto
  threshold: 24
  max_active_tools: 300
```

| Key | Default | Meaning |
|---|---:|---|
| `mode` | `auto` | `auto` eagerly exposes catalogues at or below `threshold` and defers larger catalogues; `eager` and `deferred` force either behavior. |
| `strategy` | `auto` | `auto` uses verified provider-native discovery when available and otherwise portable `tool_search`; `portable` or `native` force one path. |
| `threshold` | `24` | Authorized MCP tool count at or below which `auto` mode remains eager. |
| `max_active_tools` | `300` | Maximum unpinned, dynamically visible MCP schemas. A value of `0` selects the default 300. |

When the dynamic working set is full, term-llm first evicts tools that have not yet been sent to the provider, then never-called tools, and finally least-recently-used called tools. Pinned and MCP `always_load` tools are outside this limit and are never evicted. Eviction affects visibility and context usage, not authorization; an evicted tool remains discoverable and can be activated again. If all eligible victims were already sent, eviction may reset provider conversation or prompt-cache state so the changed schema surface is applied safely.

Tool-discovery diagnostics report `dynamic_active`, `dynamic_limit`, and `eviction_count`; the same count is exposed by the serve MCP session JSON response. See the [MCP servers guide](/guides/mcp-servers/#deferred-tool-discovery) for server configuration and discovery details.

## Search config

```yaml
search:
  provider: exa_mcp
  fetch_provider: jina
  force_external: false

  exa_mcp:
    url: https://mcp.exa.ai/mcp # optional; this is the default
    api_key: ${EXA_API_KEY} # optional, raises free-tier limits

  perplexity:
    api_key: ${PERPLEXITY_API_KEY}

  exa:
    api_key: ${EXA_API_KEY}

  parallel:
    api_key: ${PARALLEL_API_KEY}

  tavily:
    api_key: ${TAVILY_API_KEY}

  brave:
    api_key: ${BRAVE_API_KEY}

  google:
    api_key: ${GOOGLE_SEARCH_API_KEY}
    cx: ${GOOGLE_SEARCH_CX}
```

Defaults are `provider: exa_mcp` and `fetch_provider: jina`: external search uses Exa's remote MCP server, while `read_url` uses Jina Reader. Set `fetch_provider: exa_mcp` to fetch pages through Exa MCP as well, or `fetch_provider: none` to omit the external `read_url` tool.

See the [Search guide](/guides/search/) for detailed search configuration.

## Image, audio, music, transcription, and embedding config

```yaml
image:
  provider: gemini
  output_dir: ~/Pictures/term-llm

audio:
  provider: venice
  output_dir: ~/Music/term-llm
  venice:
    api_key: ${VENICE_API_KEY}
    model: tts-kokoro
    voice: af_sky
    format: mp3

music:
  provider: venice
  output_dir: ~/Music/term-llm
  venice:
    api_key: ${VENICE_API_KEY}
    model: elevenlabs-sound-effects-v2
    format: mp3
  elevenlabs:
    api_key: ${ELEVENLABS_API_KEY}
    model: music_v1
    format: mp3_44100_128

transcription:
  provider: venice
  venice:
    api_key: ${VENICE_API_KEY}
    model: nvidia/parakeet-tdt-0.6b-v3
  elevenlabs:
    api_key: ${ELEVENLABS_API_KEY}
    model: scribe_v2

embed:
  provider: gemini
```

Each feature block can hold provider-specific credentials and defaults. The image, audio, music, transcription, and embedding providers are independent of the main text provider.

## Provider-specific environment overrides

Providers that shell out to local CLIs can accept extra subprocess environment variables via `providers.<name>.env`.

For `claude-bin`, term-llm also disables Claude Code hooks by default so user-level Claude automation does not leak into inference sessions. Set `providers.claude-bin.enable_hooks: true` if you explicitly want Claude Code hooks to run.

Example for Claude Code when term-llm runs inside a trusted sandboxed container:

```yaml
providers:
  claude-bin:
    model: opus
    env:
      IS_SANDBOX: "1"
      # Generate a long-lived token with: claude setup-token
      # Useful in CI or headless environments where interactive login isn't possible
      CLAUDE_CODE_OAUTH_TOKEN: "your-oauth-token-here"
    # Optional: re-enable Claude Code hooks for this provider
    # enable_hooks: true
```

`providers.<name>.env` values support the same resolution rules as other deferred config values:

- `file://path` → trimmed file contents
- `file://path#json.path` → JSON field extracted from the file
- `op://...` → 1Password secret lookup
- `$()` → command output
- `${VAR}` / `$VAR` → environment variable expansion

This is passed only to the provider subprocess. It does not mutate your parent shell environment.

## Provider service tier

Built-in `openai` and `chatgpt` text providers support the Responses API `service_tier` field. Omit `service_tier` to send no service tier. Set it to `fast` (or the API value `priority`) to request fast/priority service for supported models and accounts:

```yaml
providers:
  openai:
    model: gpt-5.6-sol
    service_tier: fast

  chatgpt:
    model: gpt-5.6-sol-medium
    service_tier: priority
```

In chat mode, `/fast` toggles this service tier for the current session. It does not rewrite your config file.

## GPT-5.6 advanced Responses controls

The `responses` provider block configures execution features specific to **OpenAI API GPT-5.6**. It is separate from the top-level `reasoning` block, which controls whether reasoning is displayed, persisted, or exported.

```yaml
providers:
  openai:
    model: gpt-5.6-sol
    fast_model: gpt-5.6-luna
    responses:
      reasoning_mode: standard       # standard or pro
      reasoning_context: auto        # auto, current_turn, or all_turns
      multi_agent:
        enabled: true
        max_concurrent_subagents: 3  # defaults to 3 when enabled
      programmatic_tool_calling:
        enabled: true
        tools: [read_file, grep]
      prompt_cache:
        mode: explicit               # implicit or explicit
        ttl: 30m                     # currently the only supported TTL
```

Rules and caveats:

- These controls are accepted only by the built-in `openai` provider with `gpt-5.6-sol`, `gpt-5.6-terra`, or `gpt-5.6-luna`. term-llm rejects them for older models, custom/OpenAI-compatible providers, and ChatGPT OAuth.
- `reasoning_mode: pro` is public-API Pro mode and is not sent through ChatGPT OAuth. Codex's product-level Ultra option is also not a reasoning effort; it combines `max` effort with subagents.
- `reasoning_context` accepts only `auto`, `current_turn`, or `all_turns`.
- Enabling multi-agent without a concurrency value uses `3`. Multi-agent requests omit the normal reasoning-summary request, send the required beta header, and use HTTP/SSE even when provider WebSocket transport is enabled; WebSocket `response.inject` support is not yet implemented.
- Programmatic tool calling requires at least one listed tool, and every name must also be present in that request's tool definitions.
- Prompt-cache `mode` accepts `implicit` or `explicit`; the only supported explicit TTL is `30m`.
- Request-level options override populated provider defaults. Ephemeral internal requests, such as title and helper calls, do not inherit provider advanced defaults unless the caller explicitly supplies options.

In terminal chat, `/pro on`, `/pro off`, and `/pro status` set the session's reasoning mode. The setting is persisted with the session and automatically disabled if you switch to a provider/model that does not support it.

## Provider file upload policy

Provider configs can override which MIME types may be forwarded as native file/document inputs. This matters for the web/API `input_file` path: term-llm saves uploads locally first, then either sends a native file part, embeds text-like files as prompt text, or falls back to a local marker for unsupported binaries.

Built-in defaults are conservative:

- `openai`, `chatgpt`, `grok`, and `copilot` allow the native Responses MIME set by default: PDF; `text/*`; JSON and XML; Word (`.doc`/`.docx`), RTF, and OpenDocument text; Excel (`.xls`/`.xlsx`); and PowerPoint (`.ppt`/`.pptx`).
- Providers without an implemented native file path do not forward native file parts; they use text fallback/marker behavior instead.
- Text-like files (`txt`, `md`, `csv`, `tsv`, `json`, `yaml`, `xml`, `html`, and common code files) can still be embedded as ordinary text on providers without native file support, wrapped in explicit begin/end file markers.

Example custom policy:

```yaml
providers:
  openai:
    model: gpt-5.6-sol
    file_upload:
      native_mime_types:
        - application/pdf
        - text/plain
        - text/markdown
        - text/csv
        - application/json
      max_native_bytes: 20971520
      text_embed_mime_types:
        - text/plain
        - text/markdown
        - text/csv
        - application/json
      max_text_embed_bytes: 20971520
```

To disable native forwarding while keeping text fallback available:

```yaml
providers:
  openai:
    file_upload:
      native_mime_types: []
```

The server still enforces its upload limits (10 attachments, 20 MB decoded per attachment, and 50 MB total JSON request body). Provider-native limits may be lower or higher; if a provider rejects a native file type, remove it from `native_mime_types` so term-llm falls back to text/marker behavior.

## Provider WebSocket transport

Built-in `openai` and `chatgpt` text providers use the Responses WebSocket transport by default for lower-latency agent/tool loops. The WebSocket path keeps a persistent connection and, when safe, continues turns with `previous_response_id` plus only the new user/tool input. If the WebSocket connect/write step fails, term-llm falls back to HTTP/SSE; if a WebSocket continuation is rejected because the prior response state is unavailable, it retries that turn once with full state.

Disable it per provider if you need to force HTTP/SSE:

```yaml
providers:
  openai:
    use_websocket: false
  chatgpt:
    use_websocket: false
```

OpenAI-compatible providers (`type: openai_compatible`, including local/self-hosted endpoints and OpenRouter-style compatible APIs) do **not** enable WebSockets by default. They continue to use HTTP/SSE unless explicitly supported and wired by that provider.

## vLLM providers

Use `type: vllm` for vLLM servers that should receive reasoning-model chat-template controls. It uses the same `base_url`, `url`, `api_key`, `context_window`, and `max_output_tokens` fields as `openai_compatible`, but maps term-llm reasoning effort suffixes into vLLM request fields for supported model families:

```yaml
providers:
  cdck_qwen:
    type: vllm
    base_url: https://gpu-server.example.com:8000/v1
    model: Qwen/Qwen3.5-122B-A10B
    api_key: ${CDCK_QWEN_API_KEY}
    context_window: 200000
    max_output_tokens: 50000
```

```bash
term-llm ask -p cdck_qwen       "quick answer" # thinking disabled by default
term-llm ask -p cdck_qwen-low   "think a bit"  # budget 1024
term-llm ask -p cdck_qwen-high  "think hard"   # budget 10000
```

The suffix is stripped before the model name is sent upstream. For example `cdck_qwen-high` still sends `Qwen/Qwen3.5-122B-A10B` as the model and adds `chat_template_kwargs.enable_thinking=true` plus `thinking_token_budget=10000`. Plain/default Qwen requests send `enable_thinking=false` and omit `thinking_token_budget`; budgeted Qwen efforts require a vLLM server configured to accept `thinking_token_budget` (recent vLLM requires `--reasoning-config`).

For vLLM templates that use `chat_template_kwargs.thinking`, declare model capabilities in config and set `vllm_thinking_param: thinking`. term-llm treats that metadata as authoritative: it exposes only the declared effort profiles, uses the configured default for the bare provider/model, and passes enabled efforts through unchanged to vLLM.

```yaml
providers:
  cdck_deepseek:
    type: vllm
    base_url: https://gpu-server.example.com:8000/v1
    model: deepseek-ai/DeepSeek-V4-Flash
    vllm_thinking_param: thinking
    models:
      - id: deepseek-ai/DeepSeek-V4-Flash
        alias: deepseek-v4-flash
        reasoning_efforts: [none, low, high, max]
        default_reasoning_effort: high
```

With this configuration, the bare `cdck_deepseek` profile uses `high`. Shell completion for `-p cdck_deepseek-<TAB>` offers only `none`, `low`, `high`, and `max`. `none` sends `thinking=false` and omits top-level `reasoning_effort`; every enabled level sends `thinking=true` and the exact configured value as top-level `reasoning_effort`. These requests do not send `thinking_token_budget`.

Model names containing `deepseek` still select the `thinking` request shape automatically for backward compatibility. Explicit `vllm_thinking_param` is preferred for aliased deployments because capability and default selection remain config-driven.

term-llm persists streamed reasoning and replays it as assistant `reasoning` on future vLLM turns so vLLM's chat template and prefix cache can see the same prior reasoning. vLLM may still report `reasoning_tokens: 0` in usage metadata even when reasoning text is present; that is a vLLM accounting limitation.

## Dynamic secrets and endpoints

term-llm supports dynamic resolution for some config values:

- `op://...` for 1Password secret references
- `srv://...` for DNS SRV-based endpoint discovery
- `$()` for command-based resolution

Example:

```yaml
providers:
  production-llm:
    type: vllm
    model: Qwen/Qwen3-30B-A3B
    url: "srv://_vllm._tcp.ml.company.com/v1/chat/completions"
    api_key: "op://Infrastructure/vLLM Cluster/credential?account=company.1password.com"
```

These values are resolved lazily when term-llm actually needs them. Endpoint resolution applies to both provider `url` and `base_url`; `embed.ollama.base_url` uses the same resolver when the embedding provider is created.

## WebRTC direct routing config

```yaml
serve:
  webrtc:
    enabled: true
    signaling_url: https://signal.example.com/webrtc
    token: your-signaling-token
    stun_urls:
      - stun:stun.l.google.com:19302
    max_conns: 10
```

These values match the `--webrtc-*` CLI flags. See the [WebRTC direct routing](/guides/webrtc-direct-routing/) guide for full details.

## Skills config

```yaml
skills:
  enabled: true
  auto_invoke: true
  metadata_budget_tokens: 8000
  max_visible_skills: 50
  include_project_skills: true
  include_ecosystem_paths: true
  always_enabled: [git, code-review]
  never_auto: [expensive-api-skill]
```

Controls the skills system: portable instruction bundles that inject task-specific context into the system prompt. Skills are disabled by default; set `enabled: true` to allow auto-invocation, or use `--skills` on any command for one-off activation. See [Skills](/guides/skills/) for the full guide.

## Diagnostics

```yaml
diagnostics:
  enabled: true
```

When edit retries fail, diagnostics can capture prompts, partial responses, and failure context for inspection.

## Related pages

- [Providers and models](/reference/providers-and-models/)
- [Search](/guides/search/)
- [Sessions](/reference/sessions/)
- [Skills](/guides/skills/)
- [Text embeddings](/guides/text-embeddings/)
