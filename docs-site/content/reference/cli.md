---
title: "CLI command and flag reference"
weight: 5
description: "Generated command tree and flag help from the registered Cobra commands."
kicker: "CLI"
---
This index is generated from the CLI's registered command tree. Subcommands inherit the global flags listed under `term-llm`; use `term-llm <command> --help` for full usage and examples. Hidden and Cobra-internal help flags are omitted.

<!-- BEGIN GENERATED: cli -->
### `term-llm`

Your AI toolkit, from terminal to web

Global flags (inherited by subcommands):

| Flag | Type | Description |
|---|---|---|
| `--cpuprofile` | string | Write CPU profile to file |
| `--debug-raw` | bool | Emit raw debug logs with timestamps |
| `--memprofile` | string | Write memory profile to file |
| `--no-session` | bool | Disable session persistence (no reads/writes to sessions database) |
| `--pprof` | string | Start pprof debug server (optionally specify port) |
| `--session-db` | string | Override sessions database path (defaults to ~/.local/share/term-llm/sessions.db) |
| `--stats` | bool | Show session statistics (time, tokens, tool calls) |

### `term-llm agents`

Manage agents (named configuration bundles)

| Flag | Type | Description |
|---|---|---|
| `--builtin` | bool | Show only built-in agents |
| `--local` | bool | Show only project-local agents |
| `--user` | bool | Show only user-global agents |

### `term-llm agents clear`

Clear all preferences for an agent

### `term-llm agents copy`

Copy an agent for customization

| Flag | Type | Description |
|---|---|---|
| `--local` | bool | Copy to project's term-llm-agents/ instead of user config |

### `term-llm agents edit`

Open agent in $EDITOR

### `term-llm agents export`

Export an agent

### `term-llm agents export gist`

Export agent to GitHub Gist

| Flag | Type | Description |
|---|---|---|
| `--public` | bool | Create a public gist (default: secret) |

### `term-llm agents get`

Show preferences for an agent

### `term-llm agents import`

Import an agent

### `term-llm agents import gist`

Import agent from GitHub Gist

| Flag | Type | Description |
|---|---|---|
| `--local` | bool | Import to project's term-llm-agents/ instead of user config |

### `term-llm agents new`

Create a new agent from template

| Flag | Type | Description |
|---|---|---|
| `--local` | bool | Create in project's term-llm-agents/ instead of user config |

### `term-llm agents path`

Print agent directories

### `term-llm agents set`

Set preferences for an agent

### `term-llm agents show`

Display agent configuration

### `term-llm ask`

Ask a question and stream the answer

| Flag | Type | Description |
|---|---|---|
| `--agent, -a` | string | Use an agent (name or path to agent directory) |
| `--allow-project-skill-tools` | bool | Enable execution of project skill-declared script tools without approval (always allowed in yolo mode) |
| `--approval` | approval | Approval mode: prompt, auto (guardian-reviewed), or yolo |
| `--auto` | bool | Use guardian LLM review for unmatched shell and file approvals instead of prompting when safe |
| `--continue-with` | string | Custom continuation prompt for progressive timeout mode |
| `--debug, -d` | bool | Show debug information |
| `--fast` | bool | Use the configured fast provider/model instead of the default |
| `--file, -f` | stringArray | File(s) to include as context (supports globs, line ranges like file.go:10-20, 'clipboard') |
| `--inline-max-bytes` | string | Embed stdin/-f text up to this size inline instead of staging it for tools (e.g. 512K, 2M; overrides ask.stdin_inline_max_bytes) |
| `--json` | bool | Emit JSONL event stream on stdout (one event per line, implies --text) |
| `--max-output-tokens` | int | Maximum output tokens (0 = provider default) |
| `--max-turns` | int | Max agentic turns for tool execution |
| `--mcp` | string | Enable MCP server(s), comma-separated (e.g., playwright,filesystem) |
| `--native-search` | bool | Use provider's native search (override config) |
| `--no-native-search` | bool | Use external search tools instead of provider's native search |
| `--no-search` | bool | Disable web search and web fetch tools (overrides agent defaults) |
| `--no-web-fetch` | bool | Disable external URL fetching (read_url) when search is enabled |
| `--porcelain` | bool | Output plain text without tool status lines (implies --text) |
| `--progressive` | bool | Enable progressive execution with persisted best-so-far progress |
| `--provider, -p` | string | Override provider, optionally with model (e.g., openai:gpt-4o) |
| `--read-dir` | stringArray | Directories for read_file/grep/glob/view_image tools (repeatable) |
| `--resume, -r` | string | Continue a session (empty for most recent, or session ID) |
| `--search, -s` | bool | Enable web search for current information |
| `--shell-allow` | stringArray | Shell command patterns to allow (repeatable, glob syntax) |
| `--skills` | string | Skills mode: 'all'/'*' to enable all, 'none' to disable, names (comma-separated), or 'name,+' for explicit + auto |
| `--stop-when` | string | Progressive stop condition: done or timeout (defaults to done in progressive mode) |
| `--system-message, -m` | string | System message/instructions for the LLM (overrides config) |
| `--text, -t` | bool | Output plain text instead of rendered markdown |
| `--timeout` | duration | Set a hard deadline for the run (used by progressive execution for finalization budget) |
| `--tools` | string | Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_media,image_generate,ask_user,spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents),queue_agent,wait_for_jobs |
| `--write-dir` | stringArray | Directories for write_file/edit_file tools (repeatable) |
| `--yolo` | bool | Auto-approve all tool operations (for CI/container use, bypasses all prompts) |

### `term-llm audio`

Generate speech audio

| Flag | Type | Description |
|---|---|---|
| `--apply-language-text-normalization` | bool | ElevenLabs language text normalization, currently mainly Japanese |
| `--apply-text-normalization` | string | ElevenLabs text normalization (auto, on, off) |
| `--debug, -d` | bool | Show debug information |
| `--enable-logging` | bool | ElevenLabs request logging/history; set false for zero-retention-capable accounts |
| `--format` | string | Response format (Venice: mp3, opus, aac, flac, wav, pcm; Gemini: wav, pcm; ElevenLabs: mp3_44100_128, pcm_24000, wav_44100, etc.) |
| `--json` | bool | Emit machine-readable JSON to stdout |
| `--language` | string | Optional language hint (Venice model-specific; e.g. English or en) |
| `--model` | string | TTS model to use |
| `--next-request-ids` | string | ElevenLabs comma-separated next request IDs for continuity |
| `--next-text` | string | ElevenLabs continuity hint: text after this request |
| `--optimize-streaming-latency` | int | ElevenLabs latency optimization level (0 to 4); -1 omits it |
| `--output, -o` | string | Custom output path, or - for stdout |
| `--previous-request-ids` | string | ElevenLabs comma-separated previous request IDs for continuity |
| `--previous-text` | string | ElevenLabs continuity hint: text before this request |
| `--prompt` | string | Optional style prompt for supported models |
| `--pronunciation-dictionaries` | string | ElevenLabs comma-separated pronunciation dictionary IDs, optionally id:version |
| `--provider, -p` | string | Audio provider override (venice, gemini, elevenlabs) |
| `--seed` | string | ElevenLabs deterministic seed (0 to 4294967295) |
| `--similarity-boost` | float64 | ElevenLabs similarity boost (0 to 1); -1 omits it |
| `--speaker-boost` | bool | ElevenLabs speaker boost voice setting |
| `--speaker1` | string | Gemini multi-speaker label for the first speaker |
| `--speaker2` | string | Gemini multi-speaker label for the second speaker |
| `--speed` | float64 | Speech speed (Venice: 0.25 to 4.0; ElevenLabs: 0.7 to 1.2) |
| `--stability` | float64 | ElevenLabs voice stability (0 to 1); -1 omits it |
| `--streaming` | bool | Ask supported providers to stream; output is still collected before saving |
| `--style` | float64 | ElevenLabs style exaggeration (0 to 1); -1 omits it |
| `--temperature` | float64 | Sampling temperature for supported models (0 to 2); -1 omits it |
| `--top-p` | float64 | Nucleus sampling for supported models (0 to 1); -1 omits it |
| `--use-pvc-as-ivc` | bool | ElevenLabs workaround to use IVC instead of PVC for lower latency |
| `--voice1` | string | Gemini multi-speaker voice for --speaker1 |
| `--voice2` | string | Gemini multi-speaker voice for --speaker2 |
| `--voice` | string | Voice to use (provider/model-specific; ElevenLabs accepts voice_id or account voice name; Venice cloned voice handles vv_&lt;id&gt;) |

### `term-llm auth`

Sign in to OAuth providers (ChatGPT, Grok, GitHub Copilot)

### `term-llm auth login`

Sign in to an OAuth provider

### `term-llm auth logout`

Clear stored credentials for an OAuth provider

### `term-llm auth status`

Show authentication status for OAuth providers

### `term-llm benchmark`

Measure client-observed LLM streaming and input-processing performance

| Flag | Type | Description |
|---|---|---|
| `--allow-unknown-cache` | bool | Allow token fits when the adapter cannot report cache reads (results stay labeled unknown) |
| `--assume-context-limit` | string | Expert safety override for an unknown local Ollama context limit (supports K/M suffixes; does not configure num_ctx) |
| `--cache-tolerance` | float64 | Maximum cache-read ratio accepted as cold |
| `--cache` | string | Cache workload: cold (warm is reserved for a future separate workload) |
| `--concurrency` | int | Concurrent requests (schema version 1 supports only 1) |
| `--dry-run` | bool | Resolve and list every benchmark target without dispatching inference requests |
| `--include-managed-provider` | bool | Allow subprocess/CLI providers and label timing subprocess-inclusive |
| `--input-tokens` | string | Comma-separated input-token targets (supports K/M suffixes) |
| `--json` | bool | Emit one complete JSON report |
| `--jsonl` | string | Append each raw request record to this JSONL file as it completes |
| `--mode` | string | Workload mode: balanced, quick, decode, prefill, or long-context |
| `--output-tokens` | int | Requested output-token ceiling (0 uses the mode profile) |
| `--provider, -p` | string | Provider, optionally with model; Ollama without a model benchmarks all configured models |
| `--reasoning-effort` | string | Explicit provider reasoning effort |
| `--runs` | int | Measured runs per scenario (-1 uses the mode profile) |
| `--seed` | int64 | Deterministic workload seed |
| `--target-tolerance` | float64 | Maximum relative provider-token deviation from an input target |
| `--temperature` | float32 | Explicit sampling temperature (only sent when the adapter supports it) |
| `--timeout` | duration | Hard timeout per request |
| `--warmup` | int | Warmup runs per scenario (-1 uses the mode profile) |

### `term-llm chat`

Start an interactive chat session

| Flag | Type | Description |
|---|---|---|
| `--agent, -a` | string | Use an agent (name or path to agent directory) |
| `--allow-project-skill-tools` | bool | Enable execution of project skill-declared script tools without approval (always allowed in yolo mode) |
| `--approval` | approval | Approval mode: prompt, auto (guardian-reviewed), or yolo |
| `--auto-send` | stringArray | Queue message(s) to send automatically and exit after all responses (repeatable) |
| `--auto` | bool | Use guardian LLM review for unmatched shell and file approvals instead of prompting when safe |
| `--debug, -d` | bool | Show debug information |
| `--max-turns` | int | Max agentic turns for tool execution |
| `--mcp` | string | Enable MCP server(s), comma-separated (e.g., playwright,filesystem) |
| `--native-search` | bool | Use provider's native search (override config) |
| `--no-native-search` | bool | Use external search tools instead of provider's native search |
| `--no-search` | bool | Disable web search and web fetch tools (overrides agent defaults) |
| `--no-web-fetch` | bool | Disable external URL fetching (read_url) when search is enabled |
| `--provider, -p` | string | Override provider, optionally with model (e.g., openai:gpt-4o) |
| `--read-dir` | stringArray | Directories for read_file/grep/glob/view_image tools (repeatable) |
| `--resume, -r` | string | Resume session (empty for most recent, or session ID) |
| `--search, -s` | bool | Enable web search for current information |
| `--shell-allow` | stringArray | Shell command patterns to allow (repeatable, glob syntax) |
| `--skills` | string | Skills mode: 'all'/'*' to enable all, 'none' to disable, names (comma-separated), or 'name,+' for explicit + auto |
| `--system-message, -m` | string | System message/instructions for the LLM (overrides config) |
| `--text` | bool | Disable markdown rendering (plain text output) |
| `--tools` | string | Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_media,image_generate,ask_user,spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents),queue_agent,wait_for_jobs |
| `--write-dir` | stringArray | Directories for write_file/edit_file tools (repeatable) |
| `--yolo` | bool | Auto-approve all tool operations (for CI/container use, bypasses all prompts) |

### `term-llm classify`

Classify state with a classification provider

| Flag | Type | Description |
|---|---|---|
| `--answer` | string | Answer id for --format value |
| `--base-url` | string | Classification API base URL (defaults to selected provider base_url) |
| `--false-description` | string | NOUL false description |
| `--file, -f` | string | Read state from file ('-' for stdin) |
| `--format` | string | Output format: json, table, or value |
| `--image` | stringArray | Image file (PNG, JPEG, GIF, or WebP) or data:image URL to evaluate with the state (repeatable) |
| `--level` | stringArray | Score level (repeatable) |
| `--model` | string | Classification model (defaults to selected provider model) |
| `--name` | string | Single question answer id |
| `--option` | stringArray | Choice option key[=description] (repeatable) |
| `--output, -o` | string | Write output to file instead of stdout |
| `--pretty-print` | bool | Indent JSON output (requires --format json) |
| `--provider, -p` | string | Classification provider (defaults to classify.default_provider) |
| `--question` | string | Single question instructions |
| `--questions, -q` | string | Questions JSON/YAML file ('-' for stdin when state is not stdin) |
| `--state-json` | bool | Treat state input as JSON instead of a JSON string |
| `--timeout` | duration | Classification request timeout (defaults to selected provider timeout_seconds) |
| `--true-description` | string | NOUL true description |
| `--type` | string | Single question type: choice, score, or noul |

### `term-llm classify models`

List models for the selected classification provider

| Flag | Type | Description |
|---|---|---|
| `--base-url` | string | Classification API base URL (defaults to selected provider base_url) |
| `--format` | string | Output format: json or table |
| `--output, -o` | string | Write output to file instead of stdout |
| `--pretty-print` | bool | Indent JSON output (requires --format json) |
| `--provider, -p` | string | Classification provider (defaults to classify.default_provider) |
| `--timeout` | duration | Classification request timeout (defaults to selected provider timeout_seconds) |

### `term-llm config`

Manage term-llm configuration

### `term-llm config completion`

Generate shell completion script

| Flag | Type | Description |
|---|---|---|
| `--install` | bool | Install completions to standard location |

### `term-llm config edit`

Edit configuration file in $EDITOR

### `term-llm config edit-mcp`

Edit MCP configuration file in $EDITOR

### `term-llm config get`

Get a configuration value

### `term-llm config path`

Print configuration file path

### `term-llm config reset`

Reset configuration to defaults

### `term-llm config set`

Set a configuration value

### `term-llm config theme`

Select a UI color theme

### `term-llm contain`

Manage Docker Compose backed workspaces

### `term-llm contain exec`

Run a command in a contain workspace service

### `term-llm contain image`

Manage contain image assets

### `term-llm contain image sync`

Write managed contain image assets into the config directory

| Flag | Type | Description |
|---|---|---|
| `--force` | bool | Overwrite non-managed existing image files |

### `term-llm contain ls`

List contain workspaces and labelled Docker containers

### `term-llm contain new`

Create a new Docker Compose workspace from a deterministic template

| Flag | Type | Description |
|---|---|---|
| `--no-input` | bool | Do not prompt; use defaults and --set values only |
| `--set` | stringArray | Set template prompt value (key=value); may be specified multiple times |
| `--template` | string | Built-in template name or path to file/directory template |

### `term-llm contain rebuild`

Rebuild workspace images and recreate containers without deleting volumes

### `term-llm contain restart`

Restart a contain workspace

### `term-llm contain rm`

Permanently remove a contain workspace, including Compose-managed volumes

### `term-llm contain shell`

Open the configured shell in a contain workspace service

| Flag | Type | Description |
|---|---|---|
| `--user, -u` | string | Username or UID to use for the shell inside the container |

### `term-llm contain start`

Start an existing contain workspace, creating containers if needed

### `term-llm contain stop`

Stop a contain workspace without deleting resources

### `term-llm contain templates`

List built-in contain templates

### `term-llm debug-log`

Manage debug logs

| Flag | Type | Description |
|---|---|---|
| `--days` | int | Show sessions from last N days |

### `term-llm debug-log clean`

Remove old debug logs

| Flag | Type | Description |
|---|---|---|
| `--all` | bool | Remove all logs |
| `--days` | int | Remove logs older than N days |
| `--dry-run` | bool | Show what would be deleted |

### `term-llm debug-log disable`

Disable debug logging

### `term-llm debug-log enable`

Enable debug logging

### `term-llm debug-log export`

Export a session for sharing

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Export as JSON (default) |
| `--markdown` | bool | Export as markdown |
| `--raw` | bool | Export raw JSONL |
| `--redact` | bool | Redact sensitive content |

### `term-llm debug-log list`

List recent debug sessions

| Flag | Type | Description |
|---|---|---|
| `--days` | int | Show sessions from last N days |

### `term-llm debug-log path`

Show debug logs directory path

### `term-llm debug-log search`

Search across debug sessions

| Flag | Type | Description |
|---|---|---|
| `--days` | int | Search sessions from last N days |
| `--errors` | bool | Show only errors |
| `--provider` | string | Filter by provider |
| `--tool` | string | Filter by tool name |

### `term-llm debug-log show`

View a debug session

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output as pretty-printed JSON |
| `--raw` | bool | Output raw JSONL |
| `--tools` | bool | Highlight tool calls and arguments |

### `term-llm debug-log status`

Show debug logging status

### `term-llm debug-log tail`

Live tail of a debug session

| Flag | Type | Description |
|---|---|---|
| `--follow, -f` | bool | Follow for new entries |

### `term-llm doctor`

Check this install for broken configuration, credentials, and databases

| Flag | Type | Description |
|---|---|---|
| `--fix` | bool | Apply repairs for fixable findings |
| `--json` | bool | Output findings as JSON |
| `--only` | stringSlice | Run only these checks (db, config, credentials, mcp, stale, binaries) |
| `--skip-deferred` | bool | Skip resolving op://, srv://, file:// and $() config values |
| `--verbose, -v` | bool | Show checks that passed |
| `--yes, -y` | bool | Do not prompt before each repair |

### `term-llm edit`

Edit files using AI assistance

| Flag | Type | Description |
|---|---|---|
| `--approval` | approval | Approval mode: prompt, auto (guardian-reviewed), or yolo |
| `--auto` | bool | Use guardian LLM review for unmatched shell and file approvals instead of prompting when safe |
| `--context, -c` | stringArray | File(s) to include as read-only context (supports globs, 'clipboard') |
| `--debug, -d` | bool | Show debug information |
| `--diff-format` | string | Force diff format: 'udiff' or 'replace' (default: auto) |
| `--dry-run` | bool | Show what would change without applying |
| `--file, -f` | stringArray | File(s) to edit (required, supports line ranges like file.go:10-20) |
| `--mcp` | string | Enable MCP server(s), comma-separated (e.g., playwright,filesystem) |
| `--provider, -p` | string | Override provider, optionally with model (e.g., openai:gpt-4o) |
| `--read-dir` | stringArray | Directories for read_file/grep/glob/view_image tools (repeatable) |
| `--shell-allow` | stringArray | Shell command patterns to allow (repeatable, glob syntax) |
| `--skills` | string | Skills mode: 'all'/'*' to enable all, 'none' to disable, names (comma-separated), or 'name,+' for explicit + auto |
| `--system-message, -m` | string | System message/instructions for the LLM (overrides config) |
| `--tools` | string | Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_media,image_generate,ask_user,spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents),queue_agent,wait_for_jobs |
| `--write-dir` | stringArray | Directories for write_file/edit_file tools (repeatable) |
| `--yolo` | bool | Auto-approve all tool operations (for CI/container use, bypasses all prompts) |

### `term-llm embed`

Generate text embeddings using AI

| Flag | Type | Description |
|---|---|---|
| `--dimensions` | int | Custom output dimensions (0 = model default) |
| `--file, -f` | stringArray | Input file(s) to embed (can be specified multiple times) |
| `--format` | string | Output format: json, array, plain |
| `--output, -o` | string | Write output to file instead of stdout |
| `--provider, -p` | string | Override embedding provider (gemini, openai, chatgpt, venice, jina, voyage, ollama), optionally with model (e.g., venice:text-embedding-qwen3-8b) |
| `--similarity` | bool | Compare texts by cosine similarity instead of outputting vectors |
| `--task-type` | string | Gemini task type hint (e.g., RETRIEVAL_QUERY, RETRIEVAL_DOCUMENT, SEMANTIC_SIMILARITY) |

### `term-llm exec`

Translate natural language to CLI commands

| Flag | Type | Description |
|---|---|---|
| `--approval` | approval | Approval mode: prompt, auto (guardian-reviewed), or yolo |
| `--auto-pick, -a` | bool | Auto-execute the best suggestion without prompting |
| `--auto` | bool | Use guardian LLM review for unmatched shell and file approvals instead of prompting when safe |
| `--debug, -d` | bool | Show debug information |
| `--file, -f` | stringArray | File(s) to include as context (supports globs, 'clipboard') |
| `--max, -n` | int | Maximum number of options to show (0 = no limit) |
| `--max-turns` | int | Max agentic turns for tool execution |
| `--mcp` | string | Enable MCP server(s), comma-separated (e.g., playwright,filesystem) |
| `--native-search` | bool | Use provider's native search (override config) |
| `--no-native-search` | bool | Use external search tools instead of provider's native search |
| `--no-search` | bool | Disable web search and web fetch tools (overrides agent defaults) |
| `--no-web-fetch` | bool | Disable external URL fetching (read_url) when search is enabled |
| `--print-only` | bool | Print command instead of executing |
| `--provider, -p` | string | Override provider, optionally with model (e.g., openai:gpt-4o) |
| `--read-dir` | stringArray | Directories for read_file/grep/glob/view_image tools (repeatable) |
| `--search, -s` | bool | Enable web search for current information |
| `--shell-allow` | stringArray | Shell command patterns to allow (repeatable, glob syntax) |
| `--skills` | string | Skills mode: 'all'/'*' to enable all, 'none' to disable, names (comma-separated), or 'name,+' for explicit + auto |
| `--system-message, -m` | string | System message/instructions for the LLM (overrides config) |
| `--tools` | string | Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_media,image_generate,ask_user,spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents),queue_agent,wait_for_jobs |
| `--write-dir` | stringArray | Directories for write_file/edit_file tools (repeatable) |
| `--yolo` | bool | Auto-approve all tool operations (for CI/container use, bypasses all prompts) |

### `term-llm image`

Generate or edit images using AI

| Flag | Type | Description |
|---|---|---|
| `--aspect-ratio` | string | Image aspect ratio (for example 1:1, 16:9, or 9:16) |
| `--background` | string | Image background (auto, opaque, or transparent) |
| `--debug, -d` | bool | Show debug information |
| `--input, -i` | stringArray | Input image(s) to edit (can be specified multiple times) |
| `--no-clipboard` | bool | Skip clipboard copy |
| `--no-display` | bool | Skip terminal display |
| `--no-save` | bool | Don't save to default location (use with -o) |
| `--no-spinner` | bool | Disable interactive progress display |
| `--output, -o` | string | Custom output path |
| `--provider, -p` | string | Override image provider (built-in or configured openai_compatible name) |
| `--quality, -q` | string | Image quality (auto, low, medium, high, xhigh, or max) |
| `--size, -s` | string | Image resolution (must be 1K, 2K, or 4K) |

### `term-llm jobs`

Manage jobs v2 (definitions, runs, and events)

| Flag | Type | Description |
|---|---|---|
| `--all` | bool | Show all jobs, including completed once-off and finished agent jobs |
| `--json` | bool | Print JSON output |
| `--server` | string | Jobs API server base URL |
| `--timeout` | duration | HTTP timeout |
| `--token` | string | Bearer token for jobs API |

### `term-llm jobs active`

List active runs across all jobs

### `term-llm jobs create`

Create a job definition

| Flag | Type | Description |
|---|---|---|
| `--data` | string | Inline JSON/YAML definition payload |
| `--file` | string | Path to JSON/YAML definition file |

### `term-llm jobs delete`

Delete a job definition

| Flag | Type | Description |
|---|---|---|
| `--cancel-active` | bool | Cancel active runs before delete |

### `term-llm jobs get`

Get job definition

### `term-llm jobs list`

List job definitions

| Flag | Type | Description |
|---|---|---|
| `--all` | bool | Show all jobs, including completed once-off and finished agent jobs |

### `term-llm jobs pause`

Pause a job definition

### `term-llm jobs resume`

Resume a job definition

### `term-llm jobs run`

Run operations

### `term-llm jobs run cancel`

Cancel a run

### `term-llm jobs run events`

List run events

| Flag | Type | Description |
|---|---|---|
| `--limit` | int | Max events to return |
| `--offset` | int | Pagination offset |

### `term-llm jobs run get`

Get run details

### `term-llm jobs runs`

List runs (optionally filtered by job)

| Flag | Type | Description |
|---|---|---|
| `--limit` | int | Max runs to return |
| `--offset` | int | Pagination offset |

### `term-llm jobs trigger`

Trigger a manual run

### `term-llm jobs update`

Update a job definition

| Flag | Type | Description |
|---|---|---|
| `--data` | string | Inline JSON/YAML update payload |
| `--file` | string | Path to JSON/YAML update payload file |

### `term-llm lifecycle`

Inspect terminal-host lifecycle integration

### `term-llm lifecycle status`

Explain detected and enabled lifecycle adapters

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output as JSON |

### `term-llm live`

Inspect live voice diagnostics

### `term-llm live decisions`

List classifier route decisions

| Flag | Type | Description |
|---|---|---|
| `--since` | string | Only show decisions since a duration ago or RFC3339 timestamp |

### `term-llm loop`

Run an autonomous agent loop until completion

| Flag | Type | Description |
|---|---|---|
| `--agent, -a` | string | Use an agent (name or path to agent directory) |
| `--approval` | approval | Approval mode: prompt, auto (guardian-reviewed), or yolo |
| `--auto` | bool | Use guardian LLM review for unmatched shell and file approvals instead of prompting when safe |
| `--debug, -d` | bool | Show debug information |
| `--done-file` | string | FILE:TEXT - complete when file contains text |
| `--done` | string | Command that signals completion when it returns 0 |
| `--history` | int | Number of iteration summaries to inject into prompt |
| `--max-output-tokens` | int | Maximum output tokens (0 = provider default) |
| `--max-turns` | int | Max agentic turns for tool execution |
| `--max` | int | Maximum number of iterations (0 = unlimited) |
| `--mcp` | string | Enable MCP server(s), comma-separated (e.g., playwright,filesystem) |
| `--native-search` | bool | Use provider's native search (override config) |
| `--no-native-search` | bool | Use external search tools instead of provider's native search |
| `--no-search` | bool | Disable web search and web fetch tools (overrides agent defaults) |
| `--no-web-fetch` | bool | Disable external URL fetching (read_url) when search is enabled |
| `--provider, -p` | string | Override provider, optionally with model (e.g., openai:gpt-4o) |
| `--read-dir` | stringArray | Directories for read_file/grep/glob/view_image tools (repeatable) |
| `--search, -s` | bool | Enable web search for current information |
| `--shell-allow` | stringArray | Shell command patterns to allow (repeatable, glob syntax) |
| `--system-message, -m` | string | System message/instructions for the LLM (overrides config) |
| `--tools` | string | Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_media,image_generate,ask_user,spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents),queue_agent,wait_for_jobs |
| `--write-dir` | stringArray | Directories for write_file/edit_file tools (repeatable) |
| `--yolo` | bool | Auto-approve all tool operations (for CI/container use, bypasses all prompts) |

### `term-llm mcp`

Manage MCP (Model Context Protocol) servers

### `term-llm mcp add`

Add an MCP server from the registry, URL, or local command

### `term-llm mcp browse`

Browse the MCP registry

| Flag | Type | Description |
|---|---|---|
| `--no-tui` | bool | Use simple CLI output instead of interactive browser |

### `term-llm mcp info`

Show MCP server info and available tools

### `term-llm mcp list`

List configured MCP servers

### `term-llm mcp login`

Sign in to a remote MCP server

| Flag | Type | Description |
|---|---|---|
| `--force` | bool | Sign in again even when the current grant is valid |
| `--no-browser` | bool | Print the authorization URL without opening a browser |

### `term-llm mcp logout`

Revoke and remove an MCP OAuth grant

| Flag | Type | Description |
|---|---|---|
| `--local-only` | bool | Remove local credentials without remote revocation |

### `term-llm mcp path`

Print MCP configuration file path

### `term-llm mcp remove`

Remove an MCP server

### `term-llm mcp run`

Run MCP tool(s) directly

| Flag | Type | Description |
|---|---|---|
| `--timeout` | duration | Timeout for MCP server startup and tool execution |

### `term-llm mcp status`

Show MCP transport and authentication status

### `term-llm memory`

Mine and query long-term memory fragments

| Flag | Type | Description |
|---|---|---|
| `--agent` | string | Filter by agent |
| `--db` | string | Override memory database path |
| `--dry-run` | bool | Preview actions without writing changes |

### `term-llm memory consolidate`

Safely consolidate changed memory into recent.md

| Flag | Type | Description |
|---|---|---|
| `--apply` | bool | Write the consolidated recent.md (default is dry-run preview) |
| `--decay-limit` | int | Maximum low-score fragments to list in decay preview (0 = none) |
| `--half-life` | float64 | Half-life in days for non-destructive decay preview |
| `--model` | string | Override model used for consolidation |
| `--no-decay-preview` | bool | Skip the non-destructive decay preview |
| `--recent-max-bytes` | int | Maximum bytes to keep in recent.md |
| `--since` | duration | Override consolidate lookback window (e.g. 6h) |

### `term-llm memory fragments`

Inspect stored memory fragments

### `term-llm memory fragments add`

Add a memory fragment

| Flag | Type | Description |
|---|---|---|
| `--content` | string | Fragment content (defaults to stdin) |
| `--source` | string | Fragment source |

### `term-llm memory fragments delete`

Delete a memory fragment

### `term-llm memory fragments gc`

Garbage collect fragments already below the decay threshold

| Flag | Type | Description |
|---|---|---|
| `--delete` | bool | Actually delete fragments already below the stored decay threshold |
| `--half-life` | float64 | Decay half-life in days for --preview-decay |
| `--preview-decay` | bool | Compute non-destructive age-based decay candidates |

### `term-llm memory fragments list`

List memory fragments

| Flag | Type | Description |
|---|---|---|
| `--filter-path` | string | Filter fragments whose path contains this substring |
| `--limit` | int | Maximum number of fragments to return (0 = all) |
| `--since` | duration | Only show fragments updated within this duration (e.g. 24h) |

### `term-llm memory fragments show`

Show a memory fragment by path

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output fragment as JSON with all metadata |
| `--no-path` | bool | Suppress path header, print content only |

### `term-llm memory fragments sources`

Show session turn-range sources for a memory fragment

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output fragment sources as JSON |

### `term-llm memory fragments sync`

Sync on-disk fragment .md files into the memory database

| Flag | Type | Description |
|---|---|---|
| `--dir` | string | Root directory containing .md fragment files (required) |

### `term-llm memory fragments update`

Update a memory fragment

| Flag | Type | Description |
|---|---|---|
| `--content` | string | Fragment content (defaults to stdin) |

### `term-llm memory images`

Browse and search generated images

### `term-llm memory images list`

List generated images (newest first)

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output as JSON |
| `--limit` | int | Maximum number of results |
| `--offset` | int | Offset for pagination |

### `term-llm memory images search`

Search generated images by prompt

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output as JSON |
| `--limit` | int | Maximum number of results |

### `term-llm memory insights`

Manage behavioral insight rules

### `term-llm memory insights add`

Add a new insight

| Flag | Type | Description |
|---|---|---|
| `--category` | string | Category: anti-pattern \| communication-style \| domain-approach \| workflow \| anticipation |
| `--compact` | string | Short ≤15-word version for token-efficient injection (optional) |
| `--confidence` | float64 | Initial confidence 0.0–1.0 |
| `--content` | string | Insight rule text (required, or pipe via stdin) |
| `--trigger` | string | When this insight applies (optional description) |

### `term-llm memory insights decay`

Apply exponential confidence decay to stale insights

| Flag | Type | Description |
|---|---|---|
| `--half-life` | float64 | Decay half-life in days |

### `term-llm memory insights delete`

Delete an insight by ID

### `term-llm memory insights expand`

Preview the insight block that would be injected for a given query

| Flag | Type | Description |
|---|---|---|
| `--max-tokens` | int | Token budget for expansion |

### `term-llm memory insights gc`

Delete insights below minimum confidence threshold

| Flag | Type | Description |
|---|---|---|
| `--min-confidence` | float64 | Delete insights below this confidence |

### `term-llm memory insights list`

List insights, newest first

| Flag | Type | Description |
|---|---|---|
| `--limit` | int | Maximum insights to show (0 = all) |

### `term-llm memory insights reinforce`

Reinforce an insight (bump confidence + count)

### `term-llm memory insights search`

Search insights by BM25 relevance

| Flag | Type | Description |
|---|---|---|
| `--limit` | int | Maximum results |

### `term-llm memory insights update`

Update insight content by ID

| Flag | Type | Description |
|---|---|---|
| `--compact` | string | New compact form (leave empty to keep existing) |
| `--content` | string | New insight content (required, or pipe via stdin) |

### `term-llm memory mine`

Mine completed sessions into memory fragments

| Flag | Type | Description |
|---|---|---|
| `--batch-size` | int | Number of messages to fetch per pagination request |
| `--embed-provider` | string | Override embedding provider used in EMBED phase (optionally provider:model) |
| `--embed` | bool | Embed new/updated fragments after mining |
| `--fast-model` | bool | Use the configured fast provider/model for memory extraction |
| `--include-subagents` | bool | Include subagent sessions |
| `--insights` | bool | Also run insight extraction pass after fragment mining |
| `--limit` | int | Maximum number of extraction attempts (0 = all) |
| `--max-messages` | int | Maximum newly mined messages per session (0 = all) |
| `--max-output-tokens` | int | Maximum output tokens for one extraction request |
| `--model` | string | Override model used for memory extraction |
| `--promote-every` | duration | Minimum interval between auto-promote runs |
| `--promote` | string | Promotion mode: auto\|always\|never |
| `--prompt-max-tokens` | int | Approximate maximum prompt budget per extraction request |
| `--read-bytes` | int | Maximum bytes returned by fragment lookup tools during mining |
| `--since` | duration | Only mine sessions updated within this duration (e.g. 24h) |
| `--stats` | bool | Print prompt, tool, token, and timing stats for each mined batch |
| `--taxonomy-max-tokens` | int | Approximate maximum tokens for the compact fragment taxonomy map |
| `--tool-max-turns` | int | Maximum tool-assisted turns allowed for one extraction request |

### `term-llm memory promote`

Promote recently changed fragments into recent.md

| Flag | Type | Description |
|---|---|---|
| `--model` | string | Override model used for promote |
| `--recent-max-bytes` | int | Maximum bytes to keep in recent.md |
| `--since` | duration | Override promote lookback window (e.g. 6h) |

### `term-llm memory search`

Search memory fragments (hybrid BM25 + vector)

| Flag | Type | Description |
|---|---|---|
| `--bm25-only` | bool | Force BM25-only retrieval |
| `--decay` | bool | Apply stored decay score multiplier (opt-in) |
| `--embed-provider` | string | Override embedding provider for query embedding (optionally provider:model) |
| `--freshness-half-life` | float64 | Freshness half-life in days for --freshness/--recency |
| `--freshness` | bool | Apply non-persistent timestamp freshness multiplier (opt-in) |
| `--json` | bool | Output as JSON |
| `--limit` | int | Maximum number of results |
| `--recency` | bool | Alias for --freshness |
| `--touch` | bool | Record access metadata for returned fragments |

### `term-llm memory status`

Show memory mining status

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output as JSON |

### `term-llm memory update-recent`

Update recent.md with terse summaries of new session activity

| Flag | Type | Description |
|---|---|---|
| `--file, -f` | string | Path to recent.md file (overrides default agent path) |
| `--fragment-chars` | int | Max chars of recent memory fragments to include in the prompt |
| `--max-input-chars` | int | Max chars of session text to include in the prompt |
| `--model` | string | Override model used for update-recent |
| `--target-recent-tokens` | int | Target size of recent.md in tokens (~4 chars/token); high water mark is +20% |

### `term-llm models`

List available models from a provider

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output as JSON |
| `--provider, -p` | string | Provider to list models from (agy-bin, anthropic, chatgpt, claude-bin, copilot, cursor-bin, grok, grok-bin, nearai, ollama, openai, openai_compatible, opencode-go, openrouter, sambanova, venice, vllm, xai, zen) |
| `--refresh` | bool | Force a fresh ChatGPT model catalog fetch (report fetch errors) |

### `term-llm music`

Generate music or sound effects

| Flag | Type | Description |
|---|---|---|
| `--composition-plan-file` | string | ElevenLabs: JSON composition plan file; cannot be combined with prompt-only seed |
| `--debug, -d` | bool | Show debug information |
| `--delete-media-on-completion` | bool | Venice: delete queued media from provider storage after retrieval |
| `--detailed` | bool | ElevenLabs: use detailed endpoint and keep metadata when returned |
| `--duration` | int | Requested duration in seconds (Venice model-specific; ElevenLabs prompt mode: 3 to 600) |
| `--force-instrumental` | bool | Force instrumental generation where supported |
| `--format` | string | Output format (Venice: mp3, wav, flac by model; ElevenLabs: mp3_44100_128, pcm_24000, wav_44100, etc.) |
| `--json` | bool | Emit machine-readable JSON to stdout |
| `--language` | string | Language code for Venice models that support language_code |
| `--lyrics-file` | string | Read lyrics prompt/text from a file |
| `--lyrics-optimizer` | bool | Venice: auto-generate lyrics from prompt where supported |
| `--lyrics` | string | Lyrics prompt/text for lyric-capable Venice models |
| `--model` | string | Music model to use |
| `--output, -o` | string | Custom output path, or - for stdout |
| `--poll-interval` | duration | Venice poll interval |
| `--poll-timeout` | duration | Venice poll timeout |
| `--provider, -p` | string | Music provider override (venice, elevenlabs) |
| `--quote` | bool | Venice: return price quote instead of queueing generation |
| `--respect-sections-durations` | bool | ElevenLabs composition-plan mode: strictly respect section durations |
| `--seed` | string | ElevenLabs: deterministic seed (0 to 2147483647); cannot be used with prompt per API docs |
| `--sign-with-c2pa` | bool | ElevenLabs: sign generated mp3 with C2PA |
| `--speed` | float64 | Speed multiplier for Venice models that support speed |
| `--store-for-inpainting` | bool | ElevenLabs enterprise: store generated song for inpainting |
| `--streaming` | bool | ElevenLabs: use streaming music endpoint |
| `--voice` | string | Voice for Venice voice-enabled models |
| `--with-timestamps` | bool | ElevenLabs detailed endpoint: include word timestamps |

### `term-llm notify`

Send a notification to all configured platforms

| Flag | Type | Description |
|---|---|---|
| `--chat-id` | int64 | Telegram chat ID to send to |
| `--parse-mode` | string | Telegram parse mode: Markdown or HTML |

### `term-llm notify app`

Send a Web Push notification to all subscribed apps

### `term-llm notify telegram`

Send a Telegram message and log it to the session store

| Flag | Type | Description |
|---|---|---|
| `--chat-id` | int64 | Telegram chat ID to send to |
| `--parse-mode` | string | Telegram parse mode: Markdown or HTML |

### `term-llm notify web`

Send a Web Push notification to all subscribed browsers

### `term-llm pprof`

Connect to running pprof debug server

### `term-llm pprof block`

Capture blocking profile

### `term-llm pprof cpu`

Capture CPU profile

| Flag | Type | Description |
|---|---|---|
| `--duration, -d` | int | Profile duration in seconds |

### `term-llm pprof goroutine`

Dump goroutine stacks

### `term-llm pprof heap`

Capture heap profile

### `term-llm pprof mutex`

Capture mutex contention profile

### `term-llm pprof web`

Open pprof index in browser

### `term-llm process`

Inspect and restart local term-llm processes

### `term-llm process list`

List validated local instances and clean stale records

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Print full process records as JSON |

### `term-llm process restart`

Restart one registered instance and wait for the installed build

| Flag | Type | Description |
|---|---|---|
| `--no-wait` | bool | Return after signalling; does not confirm restart success |
| `--timeout` | duration | Limit CLI waiting only (0 waits until replacement); does not cancel a signalled restart |

### `term-llm process restart-all`

Restart a snapshot of your registered instances and wait for each

| Flag | Type | Description |
|---|---|---|
| `--no-wait` | bool | Return after signalling; does not confirm restart success |
| `--timeout` | duration | Limit CLI waiting only (0 waits until replacement); does not cancel a signalled restart |

### `term-llm providers`

List available LLM providers

| Flag | Type | Description |
|---|---|---|
| `--builtin` | bool | Show only built-in providers |
| `--configured` | bool | Show only configured providers |
| `--json` | bool | Output as JSON |

### `term-llm serve`

Run the agent as a server (web, api, jobs, Telegram, or any combination)

| Flag | Type | Description |
|---|---|---|
| `--agent, -a` | string | Use an agent (name or path to agent directory) |
| `--allow-project-skill-tools` | bool | Enable execution of project skill-declared script tools without approval (always allowed in yolo mode) |
| `--approval` | approval | Approval mode: prompt, auto (guardian-reviewed), or yolo |
| `--auth` | string | Auth mode: bearer, passkey (web only), or none |
| `--auto` | bool | Use guardian LLM review for unmatched shell and file approvals instead of prompting when safe |
| `--base-path` | string | URL prefix the UI uses for session URLs (e.g. /chat) |
| `--cors-origin` | stringArray | Allowed CORS origin (repeatable, or '*' for all) |
| `--debug, -d` | bool | Show debug information |
| `--disable-extensions` | bool | Disable all web extension code for this process |
| `--disable-location-sharing` | bool | Hide the web UI action for sharing the browser's current location |
| `--disable-widgets` | bool | Disable local widget apps under {base}/widgets/&lt;mount&gt;/ |
| `--enable-file-tracking` | bool | Enable session file-change tracking for this serve process |
| `--enable-widgets` | bool | Deprecated no-op; widgets are enabled by default |
| `--extensions-dir` | string | Directory containing web extension sub-directories |
| `--extensions` | stringSlice | Ordered web extension IDs (overrides serve.extensions; empty disables all) |
| `--files-dir` | string | Directory for serving arbitrary files (videos, PDFs, etc) at {base}/files/ |
| `--host` | string | Bind host |
| `--hub-connect` | string | Hub connection mode for this node: direct or reverse |
| `--hub-node-id` | string | This node's id on the hub (used with --hub-url) |
| `--hub-node-name` | string | This node's display name on the hub (used with --hub-url) |
| `--hub-register` | bool | Register this reverse node with the Hub before connecting |
| `--hub-registration-token` | string | Hub registration token for --hub-register (defaults to $TERM_LLM_HUB_REGISTRATION_TOKEN) |
| `--hub-url` | string | URL of the term-llm Hub this node belongs to (renders a Back to Hub link in the web UI) |
| `--jobs-workers` | int | Number of concurrent job workers for --platform jobs |
| `--max-turns` | int | Max agentic turns for tool execution |
| `--mcp` | string | Enable MCP server(s), comma-separated (e.g., playwright,filesystem) |
| `--native-search` | bool | Use provider's native search (override config) |
| `--no-auth` | bool | Disable auth (only allowed on loopback host) |
| `--no-native-search` | bool | Use external search tools instead of provider's native search |
| `--no-projects` | bool | Use the single-workspace Web UI for dedicated agent deployments |
| `--no-search` | bool | Disable web search and web fetch tools (overrides agent defaults) |
| `--passkey-auth-file` | string | Private Web passkey store (default: &lt;data-dir&gt;/web-auth/auth.json) |
| `--passkey-bootstrap-token-file` | string | Private file containing the first-passkey setup secret |
| `--passkey-recovery-token-file` | string | Private file containing a short-lived passkey recovery secret |
| `--passkey-trusted-proxy` | stringSlice | Trusted proxy IP/CIDR for passkey rate limits (repeatable; never determines the public origin) |
| `--port` | int | Bind port |
| `--print-passkey-bootstrap-token` | bool | Explicitly allow printing the one-time setup code to non-interactive output |
| `--projects` | bool | Strictly enable Web UI project selection |
| `--provider, -p` | string | Override provider, optionally with model (e.g., openai:gpt-4o) |
| `--public-url` | string | Browser-visible URL required for passkeys, also used for OAuth callbacks (defaults to $TERM_LLM_SERVE_PUBLIC_URL or the authenticated request origin) |
| `--read-dir` | stringArray | Directories for read_file/grep/glob/view_image tools (repeatable) |
| `--response-timeout` | duration | Maximum inactivity before the first or next completed LLM response (default 30m; pauses for interactive waits) |
| `--search, -s` | bool | Enable web search for current information |
| `--session-max` | int | Max stateful sessions in memory |
| `--session-ttl` | duration | Stateful session idle TTL |
| `--setup` | bool | Re-run setup wizard for selected platforms |
| `--shell-allow` | stringArray | Shell command patterns to allow (repeatable, glob syntax) |
| `--sidebar-sessions` | string | Default web sidebar session categories: all or a comma-separated list like chat,web,ask,plan,exec |
| `--suppress-server-tool-calls` | bool | Hide server-executed tool calls from API responses (use when proxying to external clients) |
| `--system-message, -m` | string | System message/instructions for the LLM (overrides config) |
| `--telegram-carryover-chars` | int | Characters of previous Telegram session context to carry into replacement sessions (0 disables) |
| `--title` | string | Override the web UI sidebar title (defaults to agent name or Chat) |
| `--token` | string | Bearer token for API auth (defaults to $TERM_LLM_SERVE_TOKEN, else auto-generated) |
| `--tool-map` | stringArray | Map client tool name to server tool (repeatable, format ClientName:ServerName) |
| `--tools` | string | Enable local tools (comma-separated, or 'all'): read_file,write_file,edit_file,shell,grep,glob,view_image,show_media,image_generate,ask_user,spawn_agent (includes wait_agent,continue_agent,cancel_agent,list_agents),queue_agent,wait_for_jobs |
| `--verbose` | bool | Log API request/response summaries to stderr |
| `--widgets-dir` | string | Directory containing widget sub-directories (default: ~/.config/term-llm/widgets) |
| `--write-dir` | stringArray | Directories for write_file/edit_file tools (repeatable) |
| `--yolo` | bool | Auto-approve all tool operations (for CI/container use, bypasses all prompts) |

### `term-llm serve hub`

Run the term-llm Hub: one dashboard over many term-llm web nodes

| Flag | Type | Description |
|---|---|---|
| `--auth` | string | Hub auth mode: bearer, passkey, or none (none is loopback-only) |
| `--base-path` | string | URL prefix for the Hub dashboard/API when mounted behind a reverse proxy (e.g. /hub) |
| `--config` | string | Path to a static nodes config file (YAML or JSON) |
| `--contain` | bool | Discover nodes from local contain workspaces |
| `--host` | string | Host to bind |
| `--nodes-file` | string | Path to the JSON store for dashboard-added nodes (default: &lt;data-dir&gt;/hub/nodes.json) |
| `--passkey-auth-file` | string | Passkey credential store (default: &lt;data-dir&gt;/hub/auth.json) |
| `--passkey-bootstrap-token-file` | string | Private file containing the first-passkey setup secret |
| `--passkey-recovery-token-file` | string | Private file containing a short-lived passkey recovery secret |
| `--passkey-trusted-proxy` | stringSlice | Trusted reverse-proxy IP or CIDR allowed to supply X-Forwarded-For (repeatable) |
| `--port` | int | Port to bind |
| `--print-passkey-bootstrap-token` | bool | Print a generated first-passkey setup code even when output is not interactive (unsafe for service logs) |
| `--public-url` | string | Stable browser-visible URL required for passkey auth (or $TERM_LLM_HUB_PUBLIC_URL) |
| `--registration-token` | string | Token that allows reverse nodes to self-register (defaults to $TERM_LLM_HUB_REGISTRATION_TOKEN; empty disables registration) |
| `--token` | string | Hub bearer token (auto-generated in bearer mode; optional explicit API token in passkey mode) |

### `term-llm serve mcp`

Run an MCP server exposing term-llm tools over HTTP

| Flag | Type | Description |
|---|---|---|
| `--approval` | approval | Approval mode: prompt, auto (guardian-reviewed), or yolo |
| `--auto` | bool | Use guardian LLM review for unmatched shell and file approvals (requires configured LLM provider) |
| `--debug, -d` | bool | Verbose logging |
| `--edit-format` | string | Edit tool flavor: edit_file (find/replace) or diff (unified diff) |
| `--host` | string | Bind address (use 0.0.0.0 for remote access) |
| `--port` | int | Bind port |
| `--read-dir` | stringArray | Allowed read directories (repeatable) |
| `--shell-allow` | stringArray | Allowed shell patterns (repeatable, glob syntax) |
| `--token` | string | Bearer token for auth (auto-generated if omitted) |
| `--tools` | string | Tools to expose (comma-separated, or 'all') [required] |
| `--write-dir` | stringArray | Allowed write directories (repeatable) |
| `--yolo` | bool | Auto-approve all tool actions |

### `term-llm service`

Install and manage per-user Web and Hub services

### `term-llm service install`

Install or reconcile a service (defaults to web)

| Flag | Type | Description |
|---|---|---|
| `--binary` | string | Executable to supervise (default: this executable) |
| `--dry-run` | bool | Print the launch plan and native definition without writing files or secrets |
| `--no-start` | bool | Write managed files without enabling or starting the service |
| `--print-setup-code` | bool | Explicitly print the temporary enrollment code even to redirected output |
| `--secret` | stringArray | Securely prompt for a named credential (repeatable; never a value on argv) |
| `--secrets-file` | string | Import literal KEY=value credentials from a private file |
| `--working-directory` | string | Working directory (default: home; preserved on reinstall) |
| `--yes, -y` | bool | Do not prompt to open the browser |

### `term-llm service logs`

Show native service logs

| Flag | Type | Description |
|---|---|---|
| `--follow, -f` | bool | Follow new log entries |

### `term-llm service open`

Open the service's configured browser URL

### `term-llm service recover`

Authorize temporary passkey recovery and restart

| Flag | Type | Description |
|---|---|---|
| `--print-setup-code` | bool | Explicitly print the temporary enrollment code even to redirected output |

### `term-llm service restart`

Restart an enabled service (interrupts active work)

| Flag | Type | Description |
|---|---|---|
| `--debug` | bool | Trace startup checks and native commands; include status and recent logs on failure |

### `term-llm service setup`

Create a fresh first-passkey setup code and restart

| Flag | Type | Description |
|---|---|---|
| `--print-setup-code` | bool | Explicitly print the temporary enrollment code even to redirected output |

### `term-llm service start`

Enable autostart and start

| Flag | Type | Description |
|---|---|---|
| `--debug` | bool | Trace startup checks and native commands; include status and recent logs on failure |

### `term-llm service status`

Show installed Web and Hub services

### `term-llm service stop`

Stop and disable autostart

### `term-llm service token`

Explicitly reveal the managed bearer token

### `term-llm service uninstall`

Remove service registration, preserving credentials and user data

### `term-llm sessions`

Manage chat sessions

### `term-llm sessions autotitle`

Generate candidate titles for recent sessions

| Flag | Type | Description |
|---|---|---|
| `--dry-run` | bool | Preview generated titles without saving |
| `--force` | bool | Regenerate even when a custom name already exists |
| `--limit` | int | Maximum number of recent sessions to inspect |
| `--min-age` | duration | Skip sessions updated more recently than this duration |
| `--verbose, -v` | bool | Print rejected candidates with rejection reason |

### `term-llm sessions browse`

Browse sessions interactively

### `term-llm sessions delete`

Delete a session

### `term-llm sessions export`

Export session as Markdown or HTML (.html output filename)

| Flag | Type | Description |
|---|---|---|
| `--include-audio` | bool | Include short audio in HTML exports (recordings and metadata copied as-is) |
| `--include-raw-reasoning` | bool | Include raw reasoning when reasoning.raw is enabled |
| `--include-reasoning` | bool | Include provider reasoning summaries in export |
| `--include-system` | bool | Include system prompt in export |

### `term-llm sessions export gist`

Export session to GitHub Gist

| Flag | Type | Description |
|---|---|---|
| `--include-raw-reasoning` | bool | Include raw reasoning when reasoning.raw is enabled |
| `--include-reasoning` | bool | Include provider reasoning summaries in export |
| `--include-system` | bool | Include system prompt in export |
| `--public` | bool | Create a public Gist (default: secret/unlisted, not private) |

### `term-llm sessions list`

List sessions

| Flag | Type | Description |
|---|---|---|
| `--limit` | int | Maximum number of sessions to list |
| `--mode` | string | Filter by mode (chat, ask, plan, exec) |
| `--provider` | string | Filter by provider |
| `--status` | string | Filter by status (active, complete, error, interrupted) |
| `--tag` | string | Filter by tag |

### `term-llm sessions name`

Set a custom name for a session

### `term-llm sessions reset`

Delete all sessions (requires confirmation)

| Flag | Type | Description |
|---|---|---|
| `--yes, -y` | bool | Delete without interactive confirmation |

### `term-llm sessions search`

Search sessions

### `term-llm sessions share`

Share a complete saved session

| Flag | Type | Description |
|---|---|---|
| `--include-audio` | bool | Include short audio (privacy-sensitive; recordings and metadata copied as-is) |
| `--include-raw-reasoning` | bool | Include raw model reasoning (privacy-sensitive; requires reasoning.raw/source permission) |
| `--json` | bool | Output the share result as JSON |
| `--new` | bool | Create a new share instead of updating a compatible session share |
| `--no-images` | bool | Omit images (uploads, generated images, and shown images) from the share |
| `--no-tools` | bool | Omit tool calls and tool output (which can contain secrets) from the share |
| `--visibility` | string | Share visibility: public, unlisted, or private (default: provider default) |

### `term-llm sessions show`

Show session details

| Flag | Type | Description |
|---|---|---|
| `--json` | bool | Output as JSON |

### `term-llm sessions tag`

Add tags to a session

### `term-llm sessions untag`

Remove tags from a session

### `term-llm skills`

Manage skills (portable instruction bundles)

| Flag | Type | Description |
|---|---|---|
| `--local` | bool | Show only project-local skills |
| `--source` | string | Filter by source: local, user, claude, codex, gemini, cursor |
| `--user` | bool | Show only user-global skills |

### `term-llm skills add`

Add skills from a GitHub repository

| Flag | Type | Description |
|---|---|---|
| `--all` | bool | Install all discovered skills without prompting |
| `--branch` | string | Git branch to fetch from (default: main) |
| `--no-tui` | bool | Non-interactive mode (use with --all) |
| `--path` | string | Path within repo to look for skills |

### `term-llm skills browse`

Browse the Skills marketplace

| Flag | Type | Description |
|---|---|---|
| `--ai` | bool | Use AI semantic search instead of keyword search |
| `--no-tui` | bool | Use simple CLI output instead of interactive browser |

### `term-llm skills copy`

Copy a skill for customization

| Flag | Type | Description |
|---|---|---|
| `--local` | bool | Copy to project's .skills/ instead of user config |

### `term-llm skills edit`

Open skill in $EDITOR

### `term-llm skills new`

Create a new skill from template

| Flag | Type | Description |
|---|---|---|
| `--local` | bool | Create in project's .skills/ instead of user config |

### `term-llm skills path`

Print skill directory path

### `term-llm skills show`

Display skill details

### `term-llm skills update`

Update skills from their remote sources

| Flag | Type | Description |
|---|---|---|
| `--dry-run` | bool | Show what would be updated without making changes |
| `--force` | bool | Update even if content hasn't changed |
| `--yes, -y` | bool | Skip confirmation prompts |

### `term-llm skills validate`

Validate a skill

| Flag | Type | Description |
|---|---|---|
| `--all` | bool | Validate all discovered skills |

### `term-llm transcribe`

Transcribe an audio file to text

| Flag | Type | Description |
|---|---|---|
| `--language` | string | Language hint for transcription (e.g. "en", "ja") |
| `--model` | string | Transcription model override |
| `--porcelain` | bool | Output only the transcript text |
| `--provider` | string | Transcription provider override: "openai", "chatgpt" (ChatGPT login, experimental), "mistral" (Voxtral), "venice", "elevenlabs", "local" (whisper.cpp server), "whisper-cli". Defaults to transcription.provider in config, or "openai". |
| `--timestamps` | bool | Request timestamp metadata where supported (ElevenLabs emits JSON with word timestamps) |

### `term-llm upgrade`

Upgrade term-llm to the latest release

| Flag | Type | Description |
|---|---|---|
| `--version` | string | Install a specific version (e.g. v0.2.0) |

### `term-llm usage`

Show usage, costs, and subscription limits

| Flag | Type | Description |
|---|---|---|
| `--breakdown` | bool | Show per-model breakdown |
| `--copilot-cost-center` | string | Filter enterprise Copilot usage by cost center ID |
| `--copilot-enterprise` | string | Enterprise slug for enterprise scope |
| `--copilot-entity` | string | Copilot billing entity (username, organization, or enterprise slug; defaults to authenticated user for user scope) |
| `--copilot-model` | string | Filter Copilot AI Credit usage by model |
| `--copilot-org` | string | Organization for org scope, or organization filter for enterprise scope |
| `--copilot-product` | string | Filter Copilot AI Credit usage by product |
| `--copilot-scope` | string | Copilot billing scope (user, org, enterprise) |
| `--copilot-user` | string | GitHub username for user scope, or user filter for organization/enterprise scope |
| `--daily` | bool | Show OpenRouter usage by UTC day instead of model |
| `--day` | int | Copilot usage day (1-31) |
| `--days` | int | OpenRouter calendar UTC days including today (1-3660) |
| `--include-external` | bool | Include externally-tracked term-llm usage (claude-bin and codex calls) |
| `--json` | bool | Output as JSON |
| `--month` | int | Copilot usage month (1-12; defaults to GitHub API current month) |
| `--provider, -p` | string | Filter by provider (agy-bin, chatgpt, grok, claude-bin, cursor-bin, opencode-go, openrouter, claude-code, copilot, term-llm, or all) |
| `--since` | string | Start date (YYYYMMDD) |
| `--until` | string | End date (YYYYMMDD) |
| `--year` | int | Copilot usage year (YYYY; defaults to GitHub API current year) |

### `term-llm version`

Print version information

### `term-llm video`

Generate videos with Venice AI

| Flag | Type | Description |
|---|---|---|
| `--aspect-ratio` | string | Aspect ratio, e.g. 16:9 or 9:16 |
| `--audio` | bool | Request audio when the model supports it |
| `--debug, -d` | bool | Show debug information |
| `--delete-remote` | bool | Delete remote media after successful retrieval |
| `--duration` | string | Video duration (5s or 10s) |
| `--input, -i` | string | Input image for image-to-video |
| `--json` | bool | Emit machine-readable JSON to stdout |
| `--model` | string | Venice video model to use |
| `--negative-prompt` | string | Negative prompt |
| `--no-wait` | bool | Queue the job and exit without waiting for completion |
| `--output, -o` | string | Custom output path |
| `--poll-interval` | duration | Polling interval while waiting for completion |
| `--provider, -p` | string | Video provider override (currently only venice) |
| `--quote-only` | bool | Quote the job and exit without queueing |
| `--reference, -r` | stringArray | Reference image(s) for models that support multi-reference consistency (repeatable) |
| `--resolution` | string | Video resolution (480p, 720p, 1080p) |
| `--timeout` | duration | Maximum time to wait for video generation |
<!-- END GENERATED: cli -->
