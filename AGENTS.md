# Repository Guidelines

## Working Approach
- Work from the repository root. Find the owning package, nearby tests, and an existing pattern with `rg` before editing.
- Use `./tmp` for scratch space; it is gitignored.
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
Use the Go version in `go.mod`. Run a focused test while developing, then use the changed-area checker (which includes reverse-dependent Go packages and an isolated temporary HOME):

```sh
go test ./internal/llm -run '^TestName$'
make check-changed
make check-changed ARGS=--all # full suite before finishing
```

`make check-changed ARGS='--base <ref> --dry-run'` previews a custom diff. The checker includes staged, unstaged, and untracked files; `--all` runs the full root-module, nested-module, frontend, complexity/size, and plan checks. Format changed Go files with `gofmt -w <changed-go-files>` before running it. When running Go tests manually, preserve `GOMODCACHE="$(go env GOMODCACHE)"` and set `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, and `XDG_CACHE_HOME` under a temporary directory: tests may otherwise read global config and skills. `make build` builds the frontend and CLI; JavaScript tests run through Go tests only when `node` is on `PATH`. Match extra checks in `.github/workflows/ci.yml` for browser lifecycle, release, or cross-platform code.

Size and function ratchets live in `ledger/`. Run `make complexity` after Go edits. New non-test Go files must stay at or under 1,000 lines; recorded oversize files may grow by at most 25 lines and packages over 15,000 lines by at most 2%. Shrinks, moves and renames only print notes; run `make complexity-baseline` to lock them in and review the ledger diff. Plan statuses are checked by `make plans-lint`.

### Frontend formatting and linting

The Preact frontends live in `frontend/`. Chat uses `frontend/src/main.tsx`; every Hub surface and Hub style is owned by `frontend/src/hub/`, with Hub transport under `frontend/src/api/hub-client.ts`. The chat build runs first and the standalone Hub build then emits `hub.js`/`hub.css` into the same ignored embedded directory. Prettier is the source formatter, ESLint checks TypeScript/TSX and hooks, and Stylelint checks CSS. After changing frontend files, format them before reviewing the diff and run every frontend quality check:

```sh
npm --prefix frontend run format
npm --prefix frontend run format:check
npm --prefix frontend run lint
npm --prefix frontend run typecheck
npm --prefix frontend test
```

CI runs `format:check` and `lint`; do not rely on CI to rewrite files. Keep lint suppressions narrow and documented rather than disabling rules inline to bypass a finding.

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

## Package Documentation
User-facing documentation lives in `docs-site/content/`; there is no separate `docs/` tree. Contributor rules live beside the code they govern:

- `internal/sqliteutil/README.md` – adding and testing SQLite migrations
- `internal/doctor/README.md` – adding a `term-llm doctor` check
- `internal/restart/README.md` – reload ownership, mode adapters, and SIGUSR2 verification coverage
- `internal/session/README.md` – steering persistence, ownership, and schema upgrades
- `internal/terminal/README.md` – owned terminal architecture and verification
- `frontend/README.md` – embedded frontends, build contract, and accessibility checks

## Common Change Paths
### Providers and configuration
- For provider changes, follow [`internal/providerspec/README.md`](internal/providerspec/README.md).
- Config lives under `$XDG_CONFIG_HOME/term-llm/config.yaml` (falling back to `~/.config/term-llm/config.yaml`). Credential names and supported providers are defined in code; do not maintain a second static list here.

### Tools
See `internal/tools/README.md` for adding a built-in tool and permission-denial coverage.

### Web UI (`frontend/` and `internal/serveui/`)
- Markdown configuration and sanitization live in `frontend/src/domain/markdown.ts`, with shared renderer hooks in `markdown-hooks.ts`. Add rendering cases to `frontend/src/domain/rendering.test.ts`.
- Author frontend assets under `frontend/` and rebuild with `make frontend`; never edit generated `internal/serveui/static/dist` files. The directory is embedded recursively and asset versions are computed from embedded contents. Follow `frontend/README.md` for the separate chat/Hub build and cache contracts; Hub assets must stay out of the chat service worker.
- For static shell assets outside `dist`, check the explicit `//go:embed` inputs in `internal/serveui/embed.go`, references in `static/index.html` and `static/sw.js`, and `embed_test.go` coverage.

## Go Style and Commits
- Use standard `gofmt`; exported names are CamelCase and unexported names mixedCaps.
- Keep functions focused and errors explicit. Wrap propagated errors with context: `fmt.Errorf("operation: %w", err)`.
- If asked to commit, use a short imperative, unprefixed subject and keep unrelated changes out of the commit.
- always run make complexity at the end of your work and ensure it passes
