# Session package

- Durable sessions, messages, steering operations and SQLite storage live here; web projection and HTTP handlers stay in `cmd/serve*.go`.
- Follow [README.md](README.md) for steering persistence, fenced ownership and migration/downgrade rules; do not duplicate those rules here.
- Coordinate schema changes with `internal/sqliteutil/README.md`; add fresh/upgrade equivalence and error-path tests.
- Tests must use isolated HOME/XDG and temporary databases, never a user's sessions.
- Focused checks: `go test ./internal/session`, `make check-changed`.
