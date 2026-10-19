# Command layer

`cmd` is Cobra glue only: define commands, bind flags into per-command Options, and call logic in `internal/app/<command>`.
Keep command help, flag names/defaults, completion, and `ReferenceCommand()` stable. Move implementation and its focused tests into application packages as boundaries permit; avoid importing `cmd` from them.
