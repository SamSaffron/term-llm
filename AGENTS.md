# Repository Guidelines

## Working Approach
- Work from the repository root. Find the owning package, nearby tests, and an existing pattern with `rg` before editing.
- Make the smallest coherent change; remove superseded paths unless compatibility is required and documented.
- Never change user config, credentials, caches, or generated artifacts to make tests pass.
- Before finishing, inspect `git diff`, run checks for every touched area, and report commands and omissions.

## Project Map
- `main.go`, `cmd/` – CLI entry point, commands, and cross-package wiring
- `internal/config/`, `internal/llm/` – config/provider resolution; provider implementations, engine, compaction, and mocks (`llm/factory.go` constructs providers)
- `internal/tools/`, `internal/edit/` – tool registry/permissions and file editing
- `internal/serve/`, `internal/serveui/` – server primitives and embedded web UI
- `internal/tui/`, `internal/ui/`, `internal/render/` – terminal presentation
- `internal/agents/`, `internal/jobs/`, `internal/memory/`, `internal/mcp/` – core subsystems
- `internal/testutil/` – engine/TUI harnesses, mock tools, and assertions
- `internal/terminal/`, `internal/reflow/` – application-owned nested modules

## Build and Test
Use the Go version in `go.mod`. Run the narrowest relevant test during development:

```sh
go test ./internal/llm -run '^TestName$'
```

Before completion, run root-module checks unless only documentation changed:

```sh
gofmt -w <changed-go-files>
go build ./...
go test ./...
go vet ./...
```

`go test ./...` may read global config and skills. For isolation, preserve the module cache but use a temporary home:

```sh
GOMODCACHE="$(go env GOMODCACHE)"
TEST_HOME="$(mktemp -d)"; trap 'rm -rf "$TEST_HOME"' EXIT
HOME="$TEST_HOME" XDG_CONFIG_HOME="$TEST_HOME/config" \
  XDG_DATA_HOME="$TEST_HOME/data" XDG_CACHE_HOME="$TEST_HOME/cache" \
  GOMODCACHE="$GOMODCACHE" go test ./...
```

JavaScript tests run through Go tests only when `node` is on `PATH`. Match extra checks in `.github/workflows/ci.yml` for browser lifecycle, release, or cross-platform code.

## Nested Terminal and Reflow Modules
`internal/reflow`, `internal/terminal/runtime`, and `internal/terminal/renderer` have their own `go.mod` files. Root `go test ./...` does not enter them.

- Changes there require `scripts/verify_nested_modules.sh`; set `VERIFY_RACE=1` for CI race coverage.
- For terminal changes, follow `internal/terminal/README.md` for architecture, compatibility, benchmarks, checkptr, fuzzing, and upstream sync. When relevant, run `scripts/fuzz_owned_renderer.sh` and `scripts/cross_build_owned_terminal.sh`.
- Runtime and renderer keep upstream module identities only for Bubbles/Huh compatibility; never replace either directory wholesale.
- Because local replacements break `go install ...@latest`, build a checkout or use a release artifact.

## Testing Conventions
- Add a regression test that fails before the fix when practical. Keep tests beside code as `*_test.go`; use table-driven cases for meaningful variants.
- Use `internal/llm/mock_provider.go` for scripted provider turns and recorded requests. Use `internal/testutil/harness.go` for engine-level behavior rather than invoking a live model.
- Test observable behavior and failure paths, not implementation trivia. Avoid network access, real API keys, timing sleeps, and dependence on the user's home directory.

## Common Change Paths
### Providers and configuration
- Trace provider changes through the implementation, `internal/llm/factory.go`, config schema/defaults, model listing, and tests; do not update only the transport.
- Config lives under `$XDG_CONFIG_HOME/term-llm/config.yaml` (falling back to `~/.config/term-llm/config.yaml`). Credential names and supported providers are defined in code; do not maintain a second static list here.

### Tools
- Wire tools through the registry in `internal/tools/` and update `internal/tools/permissions.go` when access policy changes.
- Cover permission denial as well as successful execution.

### Web UI (`internal/serveui/static/`)
- `markdown-setup.js` is the single source of truth for marked.js configuration in both browser and Node tests. Add rendering cases to `markdown_test.js`.
- For a new first-party asset, update the `//go:embed` inputs and render/version tables in `internal/serveui/embed.go`, plus `index.html` and `sw.js` (`SHELL_ASSETS`). Add or update `embed_test.go` coverage.

## Go Style and Commits
- Use standard `gofmt`; exported names are CamelCase and unexported names mixedCaps.
- Keep functions focused and errors explicit. Wrap propagated errors with context: `fmt.Errorf("operation: %w", err)`.
- If asked to commit, use a short imperative, unprefixed subject and keep unrelated changes out of the commit.
