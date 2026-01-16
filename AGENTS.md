# Repository Guidelines

## Project Structure & Module Organization
- `main.go` is the CLI entry point.
- `cmd/` holds command wiring and top-level CLI helpers.
- `internal/config/`, `internal/llm/`, `internal/prompt/`, and `internal/ui/` contain the core implementation.
- `internal/llm/` has a clean `Provider` interface with implementations:
  - `anthropic.go` – Anthropic API (Claude)
  - `openai.go` – OpenAI Responses API (API key auth, OPENAI_API_KEY)
  - `codex.go` – OpenAI Responses API via Codex OAuth (~/.codex/auth.json)
  - `gemini.go` – Google Gemini API (consumer API key)
  - `gemini_cli.go` – Google Code Assist API (gemini-cli OAuth)
  - `zen.go` – OpenCode Zen API (free tier, no API key required)
- `internal/image/` has `ImageProvider` interface for image generation:
  - `gemini.go` – Gemini image generation (gemini-2.5-flash-image)
  - `openai.go` – OpenAI image generation (gpt-image-1)
  - `flux.go` – Black Forest Labs Flux (flux-2-pro, flux-kontext-pro)
  - `output.go` – Save, display (icat), clipboard utilities
- `term-llm` is the built binary when compiled locally.

## Build, Test, and Development Commands
- `go build` builds the `term-llm` binary in the repo root.
- `go install github.com/samsaffron/term-llm@latest` installs the latest release from upstream.
- `term-llm "your request"` runs the CLI once built or installed.
- `term-llm config` prints current config; `term-llm config edit` opens it for editing.
- `term-llm image "prompt"` generates an image; `term-llm image "edit" -i input.png` edits an existing image; `-i clipboard` edits from clipboard.
- Other common commands: `term-llm ask`, `term-llm chat`, `term-llm edit`, `term-llm exec`, `term-llm tools`, `term-llm mcp`, `term-llm models`, `term-llm completion`, `term-llm upgrade`.
- Go toolchain: `go.mod` specifies `go 1.25.5`; use a compatible toolchain.

## Configuration & Secrets
- Config lives at `~/.config/term-llm/config.yaml` (or `$XDG_CONFIG_HOME/term-llm/` if set).
- Set provider keys via environment variables: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `ZEN_API_KEY`.
- Image providers have separate config under `image.{gemini,openai,flux}` with their own `api_key` settings.
- Environment variables `GEMINI_API_KEY`, `OPENAI_API_KEY`, `BFL_API_KEY` are used as fallbacks for image provider keys.
- Alternatively, use OAuth credentials from companion CLIs:
  - Codex OAuth credentials from `~/.codex/auth.json`
  - gemini-cli OAuth credentials from `~/.gemini/oauth_creds.json`
- OpenCode Zen (`default_provider: zen`) works without an API key (free tier), or set `ZEN_API_KEY` for paid models.
- Use `--provider` flag to override provider for testing: `term-llm exec --provider zen "list files"`
- Do not commit API keys or local config changes.
- OpenRouter uses `OPENROUTER_API_KEY`; OpenAI-compatible providers can be configured with `base_url` + `model`.
- Local providers: Ollama/LM Studio are OpenAI-compatible (see `internal/llm/openai_compat.go`).
- Claude CLI (`claude-bin`) uses the local Claude Code credentials (no API key).

## Search & MCP
- Web search tools live in `internal/search/` (Brave, Google, DuckDuckGo); keys/config are required per provider.
- MCP client/server wiring is in `internal/mcp/`; `term-llm mcp` manages servers and tools.

## Tools & Permissions
- Tool registry and approval flow are in `internal/tools/` (read/write/edit/shell, etc.).
- When adding tools, wire them through the registry and ensure permission checks in `internal/tools/permissions.go`.

## Edit & TUI Architecture
- Edit parsing/matching/execution is in `internal/edit/`; keep parsing rules small and explicit.
- TUI layout, styles, and screen logic are in `internal/tui/` and `internal/ui/`.

## Coding Style & Naming Conventions
- Go formatting is standard `gofmt`; keep imports grouped by gofmt defaults.
- **Always run `gofmt -w .` after making changes** to ensure consistent formatting.
- Use idiomatic Go names (CamelCase for exported, mixedCaps for unexported).
- Prefer small, focused functions and explicit error handling.

## Testing Guidelines

### Test-Driven Development (TDD)
- **Always write a failing test first**, then fix the code to make it pass.
- When fixing bugs, first write a test that reproduces the bug, then fix it.
- Use `go test ./...` to run all tests.

### Testing Infrastructure
- `internal/llm/mock_provider.go` – MockProvider for scripted LLM responses
- `internal/testutil/` – Test harness and helpers:
  - `harness.go` – EngineHarness for engine-level testing
  - `screen.go` – Screen capture for TUI state verification
  - `mock_tool.go` – MockTool with invocation tracking
  - `assertions.go` – Test assertions (AssertContains, StripANSI, etc.)

### Writing Tests
```go
func TestFeature(t *testing.T) {
    h := testutil.NewEngineHarness()
    h.EnableScreenCapture()

    // Script mock provider responses
    h.Provider.AddToolCall("call_1", "tool_name", map[string]string{"arg": "value"})
    h.Provider.AddTextResponse("Expected response")

    // Add mock tools
    mockTool := h.AddMockTool("tool_name", "tool result")

    // Run and verify
    output, err := h.Run(ctx, llm.Request{...})
    require.NoError(t, err)
    testutil.AssertContains(t, output, "Expected response")

    // Verify tool was called
    if mockTool.InvocationCount() != 1 {
        t.Error("tool not called")
    }

    // Debug screen state
    if testutil.DebugScreensEnabled() {
        h.DumpScreen()
    }
}
```

### Screen State Testing
- Use `simulateXxxScreen()` functions to define expected screen states
- Test what should NOT appear (spinners, times) as well as what SHOULD
- Run with `DEBUG_SCREENS=1 go test -v` to see captured screens
- Run with `SAVE_FRAMES=1 go test` to save frame files for inspection

### Test Naming
- Name tests with `TestXxx` prefix
- Use table-driven tests for multiple cases
- Name screen state tests descriptively: `TestApprovalScreen_NoSpinnerOrTime`
- Integration tests may require network/credentials; keep them isolated and skip when not configured.

## Commit & Pull Request Guidelines
- Commit messages in history are short, imperative, and unprefixed (e.g., “added shell integration for history”).
- Keep commits focused; avoid mixing unrelated changes.
- PRs should include a clear description, steps to validate (commands run), and any config or UX changes.
- Release/update logic lives in `internal/update/` and `scripts/release.sh`; keep user-facing versioning consistent.
