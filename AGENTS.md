# Repository Guidelines

## Project Structure
- `main.go` – CLI entry point
- `cmd/` – Command wiring and CLI helpers
- `internal/config/` – Configuration loading/saving
- `internal/llm/` – `Provider` interface + implementations (anthropic, openai, gemini, etc.)
- `internal/image/` – `ImageProvider` interface for image generation
- `internal/tools/` – Tool registry, execution, and permission checks
- `internal/edit/` – Edit parsing/matching/execution
- `internal/tui/`, `internal/ui/` – TUI layout and rendering
- `internal/search/` – Web search providers (Brave, Google, DuckDuckGo)
- `internal/mcp/` – MCP client/server wiring
- `internal/testutil/` – Test harness, mocks, and assertions

## Build & Test
- `go build` – Build binary
- `go test ./...` – Run root-module tests only
- `scripts/verify_nested_modules.sh` – Build, test, and vet reflow plus the owned Bubble Tea and Ultraviolet modules
- `VERIFY_RACE=1 scripts/verify_nested_modules.sh` – Include nested race tests
- `scripts/cross_build_owned_renderer.sh` – Cross-build release targets and relevant renderer platforms
- **Always run `gofmt -w .` after changes**

## Owned Modules
- `third_party/bubbletea` and `third_party/ultraviolet` are reduced, application-owned modules; `internal/reflow` is also a nested local replacement.
- Root `go test ./...` does not enter nested modules. Renderer/reflow changes are incomplete until the nested verification command passes.
- Follow `third_party/README.md` for compatibility, pruning, provenance, and selective upstream sync. Do not replace either renderer directory wholesale.
- `go install github.com/samsaffron/term-llm@latest` cannot use these local replacements. Build from a complete checkout or use release artifacts.

## Configuration
- Config: `~/.config/term-llm/config.yaml`
- API keys via env: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `OPENROUTER_API_KEY`, `XAI_API_KEY`, `SAMBANOVA_API_KEY`, `BFL_API_KEY`
- **Do not commit API keys or local config**

## Coding Style
- Standard `gofmt` formatting
- Idiomatic Go names (CamelCase exported, mixedCaps unexported)
- Small, focused functions with explicit error handling
- Wrap errors with context: `fmt.Errorf("operation failed: %w", err)`
- **When adding features, find similar existing code first**
- Remove superseded code, state, handlers, and tests in the same change. Do not leave dead or unreachable compatibility paths unless they are explicitly required and documented.

## Testing
- **Write a failing test first**, then fix code to make it pass
- Tests live alongside code as `*_test.go` files
- Use `internal/llm/mock_provider.go` for scripted LLM responses
- Use `internal/testutil/harness.go` for engine-level testing
- Use table-driven tests for multiple cases

## Adding Tools
- Wire through registry in `internal/tools/`
- Add permission checks in `internal/tools/permissions.go`

## Web UI (internal/serveui/static/)
- `markdown-setup.js`: **single source of truth for marked.js configuration**. Both the browser (loaded as a `<script>` before `app-core.js`) and the Node.js test suite (`require()`) use this file. Do not put `marked.use(...)` calls anywhere else.
- `markdown_test.js`: Node.js test runner for markdown rendering rules; run via `TestMarkdownSetupJS` in `embed_test.go`. Add cases here when changing markdown behaviour.
- When adding a new first-party JS file, wire it into: `index.html` (script tag), `sw.js` (SHELL_ASSETS), `embed.go` (`RenderIndexHTML` and `RenderServiceWorker` replacement tables).
- JS tests run automatically as part of `go test ./...` if `node` is in PATH.

## Commits
- Short, imperative, unprefixed messages (e.g., "add shell history integration")
- Keep commits focused; don't mix unrelated changes

Always build and test test changes you make, never commit anything the user will handle it.

This is a go project, files use TABS AND SPACES, be mindful when editing, always use `gofmt -w .` after making changes to ensure consistent formatting.
