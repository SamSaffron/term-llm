# Ask application

`Run(ctx, args, Options, Dependencies)` owns ask orchestration and cleanup; `cmd` owns Cobra binding and capture.

- No Cobra/pflag in production or tests. Ask may import the serve host, never the reverse. Input/output/persistence/MCP leaves stay here or under `internal/app/runtime`; do not duplicate process state or caches.
- Callers capture writers/stdin, explicit Changed state (including the historical `system` lookup), memory/session inputs, and deferred config/approval/provider/diagnostics hooks.
- Preserve: validation → config → approval → model/input setup order; JSON terminal flushes; plain output channels; child drain before store cleanup; resumed input selection; exact structured attachment grants.
- Test real `Run` behavior with scripted providers, not replacement execution hooks. CLI-only tests stay in `cmd`; MCP subprocess dispatch belongs in the binary that launches it.
- Focused check: `make test PKG='./internal/app/ask ./cmd'`.
