# TUI package

- `chat/` owns interactive chat model and state; presentation helpers live under `internal/ui/` and `internal/render/`.
- Use `internal/testutil/harness.go` for engine/TUI behavior rather than live providers; preserve keyboard, accessibility and terminal output behavior.
- Terminal runtime and renderer are separate nested modules under `internal/terminal/`; follow their README and `scripts/verify_nested_modules.sh` when changing them.
- Focused check: `make test PKG=./internal/tui/...`.
