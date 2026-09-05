# Documentation audit — 2026-09-05

Scope: all 47 Markdown pages under `content/`, including the homepage, search,
and section indexes. Checked the documentation against this checkout's CLI,
configuration schema/defaults, adapters, handlers, and deployment templates.
This is a source-backed audit, not a certification of every upstream service.

## Method and validation

- Inventoried every page and reviewed topics against their owning implementation.
  The table below identifies the areas checked, including pages that did not need
  a correction. “Retained” means no concrete discrepancy was found in that scope,
  not that every external dependency was exercised.
- Built a read-only CLI help probe with `go build`. Walked 161 command paths using
  `term-llm help ...`, with an isolated temporary home and no credentials. Checked
  569 extracted shell examples across 39 pages for command/flag spelling, stopping
  at `--` for passthrough commands. Examples themselves were **not executed**;
  required arguments, provider access, and semantic safety needed manual review.
- Parsed fenced YAML, including indented fences, and compared 152 recognized root
  configuration sections with `ConfigKeySpecs` / `IsKnownKey`. Agent custom-tool
  YAML was kept separate from global config. This checks key names, not full value
  semantics or server availability. These command/schema scans were one-off local
  audit probes, not additions to the runtime or committed test suite.
- `npm --prefix docs-site test` builds Hugo and Pagefind and runs the local browser
  suite: all 48 generated documents' local links/fragments/assets/metadata,
  representative accessibility checks, themes, navigation, search, clipboard,
  no-JavaScript/error fallbacks, mobile/tablet layouts, and all 41 articles at
  320px. The extra document is the existing legacy alias.
- Added durable regression checks in `scripts/check-site.mjs`: every built-in
  provider must have a row in the credentials inventory (names come from the Go
  registry), the provider reference joins accessibility/responsive checks, and
  every article gets narrow-viewport overflow validation.

## Page-by-page coverage

Paths below are relative to `content/`.

| Page | Source / checked area | Result |
|---|---|---|
| `_index.md` | `cmd/exec.go`, web runtime, homepage browser tests | Retained: interactive exec, full browser workspace, local/hosted distinction. |
| `search.md` | `static/site.js`, Pagefind and browser tests | Retained: working documentation search. |
| `architecture/_index.md` | Navigation and architecture overview | Retained. |
| `architecture/overview.md` | `internal/llm/factory.go`, serve platforms | Updated platform/Hub coverage and separate text/media provider boundaries. |
| `getting-started/_index.md` | Onboarding routes/navigation | Retained. |
| `getting-started/installation.md` | `.goreleaser.yml`, `install.sh`, `Makefile`, `go.mod` | Corrected release platforms and clarified source-build prerequisites. |
| `getting-started/providers-and-setup.md` | Provider factory and local adapters | Clarified native Ollama versus configured LM Studio and external routing. |
| `getting-started/quickstart.md` | CLI help, current Zen defaults, exec picker, serve auth | Retained: no-key trial caveats, selected-command execution, workspace controls. |
| `guides/_index.md` | Guide navigation and workflow coverage | Retained. |
| `guides/agent-containers.md` | `internal/contain/` image/bootstrap/templates | Corrected source-checkout directory tree; checked ports, service layout, schedules. |
| `guides/agents.md` | `cmd/agents.go`, `internal/agents/`, prompt includes | Retained: built-ins, agent commands, prompt inclusion and configuration. |
| `guides/audio-generation.md` | `cmd/audio.go`, `internal/audio/`, config defaults | Retained: provider selection, model/voice catalog, formats and credential paths. |
| `guides/autonomous-loops.md` | `cmd/loop.go`, approval resolution | Retained: stopping/history flags and unattended workspace rules. |
| `guides/benchmarking.md` | `cmd/benchmark.go`, `internal/benchmark/` | Retained: balanced workloads, cache/latency interpretation, context safety. |
| `guides/debugging.md` | Debug flags, CLI adapters, Responses transport | Corrected OpenAI/ChatGPT WebSocket defaults. |
| `guides/dv-hub.md` | Hub flags, reverse registration and node contract | Retained: term-llm-side configuration; external dv deployment not run. |
| `guides/file-editing.md` | `cmd/edit.go`, edit strategy/config resolution | Retained: context files, per-edit/dry-run flags, ranges and diff formats. |
| `guides/hub.md` | Hub command, auth/session store and reverse connector | Corrected obsolete in-memory-session wording; checked recovery/auth routes. |
| `guides/image-generation.md` | `internal/image/` adapters and image CLI | Corrected Venice editing/multi-image support and model/credential caveats. |
| `guides/job-runner.md` | Jobs CLI/API, session persistence, workspace approval | Corrected unattended permissions, reusable connection examples, pagination and scheduling. |
| `guides/mcp-servers.md` | MCP commands/auth and tool-discovery planner | Retained: commands, authentication, deferred-discovery thresholds. |
| `guides/memory.md` | Memory commands and `internal/memory/` | Retained: search/consolidation/fragment/insight commands and flags. |
| `guides/music-generation.md` | `cmd/music.go`, `internal/music/`, config defaults | Retained: provider selection, async controls, models and credentials. |
| `guides/notifications.md` | `cmd/notify.go`, Telegram/web push setup | Clarified usable recipients, shared data/config and accepted parse modes. |
| `guides/search.md` | Search defaults, adapters and routing | Retained: Exa MCP/Jina defaults, native/external priority and provider choices. |
| `guides/serve-mcp.md` | `cmd/serve_mcp.go`, permissions/tool registration | Retained: required tools, host/port/auth flags and approval model. |
| `guides/shell-integration.md` | Exec print-only/autorun paths, shell semantics | Fixed status propagation and explained exec-only helper and outer-eval risk. |
| `guides/skills.md` | Skills commands, frontmatter and loader | Retained: discovery, management and invocation controls. |
| `guides/telegram-bot.md` | Telegram platform, setup and access control | Retained: bot commands, allowlists and timeout configuration. |
| `guides/terminal-host-lifecycle.md` | Lifecycle command, manager and host adapters | Retained: conservative defaults, opt-ins, visible-chat authority and bounded shutdown. |
| `guides/text-embeddings.md` | Embedding adapters/config and credential resolution | Removed stale free-tier guarantees; clarified separate chat/embedding models. |
| `guides/transcription.md` | Transcribe CLI | Added supported short flags and debug option. |
| `guides/usage.md` | CLI flag definitions and exec flow | Corrected `-p` shorthand: provider, not print-only. |
| `guides/video-generation.md` | Video CLI, stdin and retrieval paths | Added provider/cleanup flags, stdin example and availability/cleanup caveats. |
| `guides/webrtc-direct-routing.md` | `cmd/serve_webrtc.go` | Removed unsupported YAML configuration; documented flags and diagnostics. |
| `guides/web-ui-and-api.md` | Serve handlers, file attribution, commit operations | Updated diffs, authenticated examples, native push/PR lifecycle and deployment wording. |
| `guides/widgets.md` | `internal/widgets/`, serve flags | Retained: discovery, manifest, routes, lifecycle and proxy security. |
| `reference/_index.md` | Reference navigation | Retained. |
| `reference/built-in-tools.md` | Tool registry, custom tools, approval and tracking | Replaced unsafe SQL interpolation; fixed gofmt example and local endpoint; distinguished agent YAML. |
| `reference/configuration.md` | `internal/config/schema.go`, provider/serve wiring | Corrected transport/default examples and removed unsupported WebRTC YAML. |
| `reference/profiling.md` | `cmd/pprof.go`, profiling startup | Retained: commands, registry discovery and 30-second CPU capture. |
| `reference/provider-setup-details.md` | Provider registry, factory, local/CLI adapters | Aligned local APIs, Zen defaults, vLLM reasoning, OAuth, transports and Bedrock caveats. |
| `reference/providers-and-models.md` | Registry, model discovery, credentials and adapters | Complete 20-provider inventory plus custom profiles; native Ollama/LM Studio, Astra/context, transport, search and reasoning corrections. |
| `reference/sessions.md` | Sessions CLI/store, project bindings and file history | Retained: persistence, sharing, branching, attribution windows and attention semantics. |
| `reference/sharing.md` | Sharing provider capability checks | Marked private-share example as requiring a capable custom provider. |
| `reference/usage-tracking.md` | Usage CLI and account-reporting integrations | Retained: local versus live usage and provider-specific filter limits. |
| `reference/version-and-updates.md` | Update command and compatibility handlers | Retained: update controls and current compatibility notes. |

## Limits and maintenance notes

- No personal configuration, credentials, or private provider caches were changed.
  No audit command invoked a model, created a remote job, or deployed a service.
- Vendor catalogs, pricing, free tiers, region availability, hosted tools, and
  subscription entitlements can change outside a term-llm release. Documentation
  distinguishes adapter behavior and shipped defaults from account guarantees.
  In particular, Bedrock's shared Anthropic implementation does not establish
  universal hosted-tool support or a single-region residency guarantee.
- External links and third-party installations were not exhaustively exercised.
  Internal links, fragments, generated search and local browser behavior were.
- This updates repository documentation; publishing term-llm.com remains the
  normal deployment/release process.
