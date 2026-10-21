# Command layer

- New command logic goes in `internal/app/<command>/`; `cmd` keeps Cobra binding, flag/input capture and the application call. Serve, ask and chat are fully extracted; other commands still carry implementation here.
- Keep help, flag names/defaults, completion and `ReferenceCommand()` stable. Real Cobra contract tests stay here; move focused tests with the implementation.
- Application packages must not import `cmd`.
- Focused check: `make test PKG=./cmd`.
