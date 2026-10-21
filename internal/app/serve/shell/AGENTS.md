# Shared shell

Per-session PTY shell generations for `term-llm serve`, independent of the serve host.

- `shell.go`: `Shell` output replay, exit handling, resize/write; `Process` interface.
- `manager.go`: `Manager` creation, lookup, TTL eviction, and serialized teardown per session.
- `collaboration.go`, `activity.go`: agent/browser collaboration state, protocol capture, activity reservations.
- `controller.go`: `Controller`, the `tools.CollaborativeShellController` implementation.
- `unix.go`, `session_*.go`, `unsupported.go`: platform PTY process and session-group signalling.
- `inspect.go`: accessors/overrides for embedding tests (fake `Process`).

HTTP handlers, workspace resolution and runtime availability checks live in `internal/app/serve/serve_shell*.go`. Focused check: `make test PKG=./internal/app/serve/shell`.
