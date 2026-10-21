# Tools package

- `descriptors.go` is the built-in tool registry: constructors, policy, modes, aliases, and CLI labels belong there.
- `registry.go` wires shared tool state; `permissions*` handles approval and denial; tool implementations stay beside their tests.
- Follow [README.md](README.md) for the full add-a-tool checklist, path/guardian selectors, and permission-denial coverage; do not duplicate it here.
- Focused check: `make test PKG=./internal/tools`.
