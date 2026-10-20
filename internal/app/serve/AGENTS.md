# Serve application host

This package owns the residual serve host: startup/lifecycle, HTTP and Hub routes,
shared runner and prepared runs, session inputs/settings, nested child execution,
root-checkout coordination, jobs composition/notifications, live voice, and media.
`Run`, `RunMCP`, and `RunHub` accept plain options and captured invocation values.
`ResolveApproval` is required, deferred until validation and configuration succeed.

- Keep **all** `internal/app/serve/**` production and test code free of Cobra/pflag.
  The transitive dependency check also forbids a Cobra/pflag path through a
  different application package. Host production/tests must not import ask/chat.
  Shared title-message selection belongs to
  `internal/app/runtime`, not the Cobra-bearing sessions application.
  Flag binding, Changed interpretation, MCP parent/child arbitration, and CLI
  completion stay in `cmd`. The depguard scans tests and inactive build tags too.
- Runtime/private listener and platform seams stay private. Do not expose fields
  merely to support callers; use the concrete prepared-run/input APIs.
- Keep one process owner for session-input coordination, root-checkout leases,
  registration/reload secrets, and child lifecycle. Pass memory/session overrides
  explicitly into all descendant runners. Never restore root flags in this host.
- Preserve validation/config/approval/startup order, secret scrubbing, and shutdown
  before closing resources used by HTTP. Failed startup has the same ownership
  requirements as successful shutdown.
- Host tests, `TestMain`'s git template, browserfixture routes, and route/auth goldens
  live here. Common process isolation is `internal/testutil/procenv`; both CLI and
  host test binaries dispatch `mcptest.RunIfChild` before setup.
- `testdata/routes.golden` and `testdata/hub_auth.golden` pin real registrations and
  authentication wrappers. Never regenerate to conceal a migration mismatch.
  `internal/tsgen` roots name actual host declarations, not compatibility aliases;
  generated TypeScript must remain byte-identical for an ownership-only move.

Leaves remain below the host: `hub/`, `jobs/`, `server/`, `shell/`, `protocol/`,
`wire/`, and `liveprogress/`. They must not import this root. `internal/serve/`
continues to own platform/service primitives and Telegram wiring. The shared run
runtime/approval/bootstrap/progressive code lives in `internal/app/runtime/`.

Focused checks (isolated HOME/XDG/TMPDIR, preserve Go caches):
`go test ./internal/app/serve/... ./cmd -count=1`; `go test -race ./internal/app/serve/... ./cmd`;
`go test ./cmd/gentsapi ./internal/depguard`; then `make check-changed ARGS=--all`.
The large host package retains measured migration debt in `ledger/`; do not
regenerate the baselines to hide complexity growth. Ask/chat own their orchestration above this host and use its concrete public APIs.
