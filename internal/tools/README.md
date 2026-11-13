# Adding a built-in tool

1. Implement `core.Tool` from `github.com/samsaffron/term-llm/internal/llm/core` with a stable `Spec()` (name, description, schema).
2. Add one entry in `descriptors.go` with its constructor, policy kind, default-enabled setting and explicit code-mode exposure/reason. Add the concrete `CodeModeExposure` method in `code_mode_exposure.go`; permission kind does not determine script exposure.
3. Set `pathCapable` or `guardianSelector` when the tool needs workspace or path-selector policy.
4. Add `FlagLabel` only if the CLI help should advertise it; use `Aliases` for legacy deny-list names.
5. If registration requires shared state (like spawn controls), wire that state in `registry.go`.
6. Add tests for descriptor completeness, observable execution and permission denial.
7. Run `go test ./internal/tools` and the root build/test/vet/complexity checks.

Script-callable tools also need an audited cell binding. A configured service must
retain its real runner/controller, while local effects retain captured permissions
and source scope. Add tests for direct and nested denial, removal/replacement,
identity, cancellation and observable effects. Undeclared new tools remain callable
directly but are excluded from scripts; finishing and script-lifecycle tools cannot
opt into nested calls.
