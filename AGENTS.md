# Repository Guidelines

## Working Approach
- Work from the repository root. Find the owning package, nearby tests, and an existing pattern with `rg` before editing.
- Use `./tmp` for scratch space; it is gitignored.
- Make the smallest coherent change; remove superseded paths unless compatibility is required and documented.
- Never change user config, credentials, caches, or generated artifacts to make tests pass.
- Before finishing, inspect `git diff`, run checks for every touched area, and report commands and omissions.

## Project Map
`PACKAGES.md` lists every root-module package with non-test code and a one-line purpose, generated from package doc comments; search it to find the owning package. Every such package needs a doc comment (usually `doc.go`); run `make pkgmap` after adding, removing or rewording one. The main areas:

- `main.go`, `cmd/` – CLI entry point and Cobra binding; new command logic goes in `internal/app/<command>/` (see `cmd/AGENTS.md`)
- `internal/app/serve/` – serve host: startup/HTTP/Hub, shared runner, session inputs, child lifecycle, jobs, live runtime
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
make test PKG=./internal/llm RUN='^TestName$'   # isolated HOME/XDG/TMPDIR, real Go caches
make check-changed
make check-changed ARGS=--all # full suite before finishing
```

`make check-changed ARGS='--base <ref> --dry-run'` previews a custom diff; without `--base` it uses the first existing of `upstream/main`, `origin/main`, `main`. The checker includes staged, unstaged, and untracked files; `--all` runs the full root-module, nested-module, frontend, complexity/size, and plan checks. Format changed Go files with `gofmt -w <changed-go-files>` before running it. Run Go tests through `make test` (or `scripts/gotest.sh <go test args>`) rather than bare `go test`, which may read the user's global config and skills; `PKG` accepts several packages and `ARGS` passes extra flags such as `-count=1` or `-race`. `make build` builds the frontend and CLI; JavaScript tests run through Go tests only when `node` is on `PATH`. Match extra checks in `.github/workflows/ci.yml` for browser lifecycle, release, or cross-platform code.

Size and function ratchets live in `ledger/`. Run `make complexity` after Go edits. New non-test Go files must stay at or under 1,000 lines; recorded oversize files may not grow (put new code in a new file) and packages over 15,000 lines may grow by at most 2%. Shrinks, moves and renames only print notes; run `make complexity-baseline` to lock them in and review the ledger diff. Never regenerate baselines to hide growth. Plan statuses are checked by `make plans-lint`.

### Frontend formatting and linting

Follow `frontend/AGENTS.md` and `frontend/README.md` for ownership, formatting, lint, typecheck, tests and build contracts.

## Nested Terminal and Reflow Modules
`internal/reflow`, `internal/terminal/runtime`, and `internal/terminal/renderer` have their own `go.mod` files. Root `go test ./...` does not enter them.

- Changes there require `scripts/verify_nested_modules.sh`; set `VERIFY_RACE=1` for CI race coverage.
- For terminal changes, follow `internal/terminal/README.md` for architecture, compatibility, benchmarks, checkptr, fuzzing, and upstream sync. When relevant, run `scripts/fuzz_owned_renderer.sh` and `scripts/cross_build_owned_terminal.sh`.
- Runtime and renderer keep upstream module identities only for Bubbles/Huh compatibility; never replace either directory wholesale.
- Because local replacements break `go install ...@latest`, build a checkout or use a release artifact.

## Testing Conventions
- Add a regression test that fails before the fix when practical. Keep tests beside code as `*_test.go`; use table-driven cases for meaningful variants.
- Use `internal/llm/mockprovider/` for scripted provider turns and recorded requests. Use `internal/testutil/harness.go` for engine-level behavior rather than invoking a live model.
- Test observable behavior and failure paths, not implementation trivia. Avoid network access, real API keys, timing sleeps, and dependence on the user's home directory.

## Package Documentation
User-facing documentation lives in `docs-site/content/`; there is no separate `docs/` tree. Run `make docs` after changing reference sources to regenerate marked tables, CLI flags and `PACKAGES.md`. Contributor rules live beside the code they govern:

- `internal/tools/AGENTS.md`, `internal/tools/README.md` – tool registration and permission-denial coverage.
- `internal/config/AGENTS.md`, `internal/providerspec/README.md` – configuration paths and provider/credential definitions.
- `internal/jobs/AGENTS.md`, `internal/session/AGENTS.md`, `internal/session/README.md` – job ownership, steering persistence and schema upgrades.
- `internal/tui/AGENTS.md`, `internal/terminal/README.md` – TUI and owned terminal architecture/verification.
- `frontend/AGENTS.md`, `frontend/README.md` – frontend ownership, web transport, embed/build/cache contracts and accessibility.
- `internal/sqliteutil/README.md` – adding and testing SQLite migrations.
- `internal/doctor/README.md` – adding a `term-llm doctor` check.
- `internal/restart/README.md` – reload ownership, mode adapters, and SIGUSR2 verification coverage.

## Go Style and Commits
- Use standard `gofmt`; exported names are CamelCase and unexported names mixedCaps.
- Keep functions focused and errors explicit. Wrap propagated errors with context: `fmt.Errorf("operation: %w", err)`.
- If asked to commit, use a short imperative, unprefixed subject and keep unrelated changes out of the commit.
- always run make complexity at the end of your work and ensure it passes
