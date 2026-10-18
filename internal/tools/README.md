# Adding a built-in tool

1. Implement `llm.Tool` with a stable `Spec()` (name, description, schema).
2. Add one entry in `descriptors.go` with its constructor, policy kind, modes and default-enabled setting.
3. Set `pathCapable` or `guardianSelector` when the tool needs workspace or path-selector policy.
4. Add `FlagLabel` only if the CLI help should advertise it; use `Aliases` for legacy deny-list names.
5. If registration requires shared state (like spawn controls), wire that state in `registry.go`.
6. Add tests for descriptor completeness, observable execution and permission denial.
7. Run `go test ./internal/tools` and the root build/test/vet/complexity checks.
