# Repository Guidelines

## Project Structure & Module Organization
- `main.go` is the CLI entry point.
- `cmd/` holds command wiring and top-level CLI helpers.
- `internal/config/`, `internal/llm/`, `internal/prompt/`, and `internal/ui/` contain the core implementation.
- `internal/llm/` has a clean `Provider` interface with three implementations:
  - `anthropic.go` – Anthropic API (Claude)
  - `openai.go` – Standard OpenAI API
  - `codex.go` – ChatGPT backend via Codex OAuth
- `term-llm` is the built binary when compiled locally.

## Build, Test, and Development Commands
- `go build` builds the `term-llm` binary in the repo root.
- `go install github.com/samsaffron/term-llm@latest` installs the latest release from upstream.
- `term-llm "your request"` runs the CLI once built or installed.
- `term-llm --config show` prints current config; `term-llm --config edit` opens it for editing.

## Configuration & Secrets
- Config lives at `~/.config/term-llm/config.yaml` (or `~/Library/Application Support/term-llm/` on macOS).
- Set provider keys via environment variables: `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`.
- Alternatively, use Codex OAuth credentials from `~/.codex/auth.json` (auto-detected if present).
- For Claude, credentials can be read from the Claude Code keychain.
- Do not commit API keys or local config changes.

## Coding Style & Naming Conventions
- Go formatting is standard `gofmt`; keep imports grouped by gofmt defaults.
- Use idiomatic Go names (CamelCase for exported, mixedCaps for unexported).
- Prefer small, focused functions and explicit error handling.

## Testing Guidelines
- No repository tests are currently present; add `*_test.go` files alongside the packages they cover.
- Use `go test ./...` for package-level test runs once tests exist.
- Name tests with `TestXxx` and include table-driven tests where appropriate.

## Commit & Pull Request Guidelines
- Commit messages in history are short, imperative, and unprefixed (e.g., “added shell integration for history”).
- Keep commits focused; avoid mixing unrelated changes.
- PRs should include a clear description, steps to validate (commands run), and any config or UX changes.
