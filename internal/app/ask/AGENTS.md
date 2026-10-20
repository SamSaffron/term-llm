# Ask application

`Run(ctx, args, Options, Dependencies)` owns ask orchestration and cleanup. Cmd owns
Cobra binding/capture. Keep production and tests Cobra/pflag-free; ask may import
the serve host, never the reverse. Existing input/output/persistence/MCP leaves
remain here or below runtime; do not duplicate process state or caches.

Capture writers/stdin, explicit Changed state (including historical `system`),
memory/session inputs and deferred config/approval/provider/diagnostics hooks.
Preserve progressive validation -> config -> approval -> model/input setup order,
JSON terminal flushes, plain output channels, child drain before store cleanup,
resumed input selection and exact structured attachment grants. Test real Run
behavior with scripted providers, not replacement execution hooks. CLI-only tests
stay cmd; MCP subprocess dispatch belongs in the binary that launches it.
