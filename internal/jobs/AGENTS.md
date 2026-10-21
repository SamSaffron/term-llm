# Jobs package

- `jobs.go` owns job scheduling and runner contracts; `program_runner_test.go` exercises program execution.
- Serve jobs-v2 HTTP routes and composition live in `internal/app/serve/serve_jobs_v2*.go` and `jobs_v2_*.go`; manager, schema, cron and notification helpers live in `internal/app/serve/jobs/`.
- Keep time-zone and retry behavior explicit; tests must not depend on real clocks, network access, or user jobs.
- Focused checks: `make test PKG='./internal/jobs ./internal/app/serve/jobs'`, `make test PKG=./internal/app/serve RUN=JobsV2`.
