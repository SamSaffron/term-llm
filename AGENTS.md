# Repository Guidelines

## Project Structure & Module Organization
- `main.go` is the CLI entry point.
- `cmd/` holds command wiring and top-level CLI helpers.
- `internal/config/`, `internal/llm/`, `internal/prompt/`, and `internal/ui/` contain the core implementation.
- `internal/llm/` has a clean `Provider` interface with implementations:
  - `anthropic.go` – Anthropic API (Claude)
  - `openai.go` – Standard OpenAI API
  - `codex.go` – ChatGPT backend via Codex OAuth
  - `gemini.go` – Google Gemini API (consumer API key)
  - `codeassist.go` – Google Code Assist API (gemini-cli OAuth)
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

## Configuration & Secrets
- Config lives at `~/.config/term-llm/config.yaml` (or `$XDG_CONFIG_HOME/term-llm/` if set).
- Set provider keys via environment variables: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GEMINI_API_KEY`, `ZEN_API_KEY`.
- Image providers have separate config under `image.{gemini,openai,flux}` with their own `api_key` settings.
- Environment variables `GEMINI_API_KEY`, `OPENAI_API_KEY`, `BFL_API_KEY` are used as fallbacks for image provider keys.
- Alternatively, use OAuth credentials from companion CLIs:
  - Codex OAuth credentials from `~/.codex/auth.json`
  - Claude Code credentials from system keychain
  - gemini-cli OAuth credentials from `~/.gemini/oauth_creds.json`
- OpenCode Zen (`default_provider: zen`) works without an API key (free tier), or set `ZEN_API_KEY` for paid models.
- Use `--provider` flag to override provider for testing: `term-llm exec --provider zen "list files"`
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
