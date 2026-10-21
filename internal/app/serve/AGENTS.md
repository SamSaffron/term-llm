# Serve application host

Owns serve startup/lifecycle, HTTP and Hub routes, the shared runner and prepared runs, session inputs/settings, nested child execution, root-checkout coordination, jobs composition/notifications, live voice and media. `Run`, `RunMCP` and `RunHub` take plain options and captured invocation values; `ResolveApproval` is required and runs only after validation and configuration succeed.

- No Cobra/pflag anywhere under `internal/app/serve/**`, including tests and inactive build tags, and no transitive path through another application package (`internal/depguard` enforces this). The host must not import ask/chat. Shared title-message selection belongs in `internal/app/runtime`, not the Cobra-bearing sessions application. Flag binding, Changed interpretation, MCP parent/child arbitration and completion stay in `cmd`.
- Keep runtime, listener and platform seams private. Do not export fields for callers; use the prepared-run/input APIs.
- One process owner each for session-input coordination, root-checkout leases, registration/reload secrets and child lifecycle. Pass memory/session overrides explicitly to every descendant runner. Never restore root flags here.
- Preserve validation → config → approval → startup order and secret scrubbing. Shut down HTTP before closing resources it uses; failed startup must release ownership the same way as shutdown.
- `testdata/routes.golden` and `testdata/hub_auth.golden` pin real route registrations and auth wrappers; regenerate them only for intended route changes. `internal/tsgen` roots name real host declarations, not aliases.
- Tests: `TestMain` provides the git template; process isolation is `internal/testutil/procenv`; both CLI and host test binaries call `mcptest.RunIfChild` before setup.

Subpackages `hub/`, `jobs/`, `server/`, `shell/`, `protocol/`, `wire/` and `liveprogress/` must not import this package. Platform/service primitives and Telegram live in `internal/serve/`; shared run runtime, approval and bootstrap live in `internal/app/runtime/`.

Focused checks: `make test PKG='./internal/app/serve/... ./cmd' ARGS=-count=1`, the same with `ARGS=-race`, and `make test PKG='./cmd/gentsapi ./internal/depguard'`.
