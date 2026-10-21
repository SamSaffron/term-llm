# Chat application

`Run(ctx, args, Options, Dependencies)` owns orchestration, runtime construction, UI wiring, handover/session switches and terminal cleanup. `cmd` binds/captures flags and supplies `ExecReload`; process re-exec stays in `cmd`.

- No Cobra/pflag/`cmd` imports in production or tests. Chat may import the serve host, never the reverse. Keep runtime/session fields private; do not export internals for fixtures or add replacement-run hooks.
- Pointer-receiver callbacks must observe the current invocation/model/agent policy.
- Keep terminal/reload authority separate from renderer output authority; preserve TTY cleanup, handover approval carry, and late session-runtime lookup.
- Preserve: terminal gate → restore → config → session/settings → approval. Reloading config on later relaunches is intentional.
- Pass session/memory overrides to all child runners and prompt/skills refreshes.
- Nil-command helpers use an explicit zero Changed state and discard writers; never infer explicitness from bound flag values. Do not fix the historical `system` lookup here.
- Source-output guards scan this directory, including forced-exit paths.
- Focused checks: `make test PKG='./internal/app/chat ./cmd'`, then the same with `ARGS=-race`.
