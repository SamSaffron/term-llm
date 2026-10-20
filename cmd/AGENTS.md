# Command layer

Keep extracted commands as Cobra binding, flag/input capture and application calls. Serve, ask and chat orchestration live in `internal/app/serve`, `internal/app/ask` and `internal/app/chat`. Other commands still have implementation here; this is not a claim that all cmd logic has moved.
Keep help, flag names/defaults, completion, and `ReferenceCommand()` stable. Move focused tests with implementation; retain real Cobra contracts here. Application packages must not import `cmd`.
