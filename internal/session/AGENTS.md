# Session package

- Durable sessions, messages, steering operations and SQLite storage live here; web projection and HTTP handlers live in `internal/app/serve/serve_session*.go`.
- Follow [README.md](README.md) for steering persistence, fenced ownership and migration/downgrade rules; do not duplicate those rules here.
- Coordinate schema changes with `internal/sqliteutil/README.md`; add fresh/upgrade equivalence and error-path tests.
- Tests use temporary databases, never a user's sessions.
- Focused check: `make test PKG=./internal/session`.
