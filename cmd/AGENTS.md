# Command layer

Keep extracted commands as Cobra binding, flag/input capture and application calls. Serve and ask orchestration live in `internal/app/serve` and `internal/app/ask`; chat extraction follows. Other commands still have implementation here; this is not a claim that all cmd logic has moved.
Keep help, flag names/defaults, completion, and `ReferenceCommand()` stable. Move focused tests with implementation; retain real Cobra contracts here. Application packages must not import `cmd`.
