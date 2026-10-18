# Jobs package

- `jobs.go` owns job scheduling and runner contracts; `program_runner_test.go` exercises program execution.
- Serve jobs-v2 HTTP routes, persistence, and scheduler live in `cmd/serve_jobs_v2*.go`; schema and cron helpers live in `internal/app/serve/jobs/`.
- Keep time-zone and retry behavior explicit; tests should not depend on real clocks, network access, or user jobs.
- Focused checks: `go test ./internal/jobs ./internal/app/serve/jobs`, `go test ./cmd -run 'JobsV2'` with isolated HOME/XDG, then `make check-changed`.
