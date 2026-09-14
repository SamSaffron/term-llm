# term-llm doctor

`term-llm doctor` inspects a local install and reports what is broken, ignored, or left over. Every check is offline and read-only by default; repairs run only with `--fix`, prompt individually unless `--yes` is given, and back up whatever they rewrite.

```bash
term-llm doctor                 # report problems
term-llm doctor -v              # include checks that passed
term-llm doctor --json          # machine-readable findings
term-llm doctor --only db,stale # run a subset
term-llm doctor --fix           # repair, prompting for each finding
term-llm doctor --fix --yes     # repair without prompting
```

Findings carry a severity of `ok`, `info`, `warn`, or `error`. The command exits non-zero only when something reached `error`.

## Output

Each check prints one status line — `✓` healthy, `!` degraded, `✗` broken — followed by its findings, with healthy findings hidden unless `-v` is given. On a terminal the status line is preceded by an animated `checking …` line that the result overwrites, so a slow check (a large `sessions.db` integrity scan, a `$()` config value shelling out) shows progress instead of a frozen prompt. Piped output, `--json`, `CI=1`, and `TERM=dumb` skip the animation and the colors; the body is identical either way.

Progress is driven by `doctor.Options.Observer`, which the runner calls once before each check and once after its confirmed repairs have been applied.

## Checks

### `db` — databases

For every SQLite database term-llm owns (`sessions.db`, `memory.db`, `jobs_v2.db`, `file_history.db`, `file_observations.db`, `hub/attention.db`) the check:

- runs `PRAGMA integrity_check` and `PRAGMA foreign_key_check`;
- migrates a **fresh** database in a temporary directory and diffs its schema signature against the user's file. This is the golden-schema comparison: it catches a change that edited the canonical `CREATE TABLE` without adding a migration, which reaches new installs but never runs on existing ones. See `docs/sqlite-migrations.md`;
- compares each external-content FTS5 index against its source table using the index's `_docsize` shadow table. Drift makes search silently return incomplete results, and is repairable with `--fix` (`INSERT INTO <fts>(<fts>) VALUES('rebuild')`).

Missing objects are errors; leftover objects are warnings. Columns, indexes, triggers, and foreign keys of a table that is missing entirely are folded into the table entry so the report stays readable.

### `config` — configuration

- Keys the loader ignores, reported per key with a Levenshtein suggestion. `--fix` comments the whole block out in place, preserving every other byte including comments, after copying the file to `config.yaml.doctor-bak-<timestamp>`.
- The deprecated `provider` alias for `default_provider`.
- Per declared provider: legacy field combinations rejected by `config.ValidateProviderCompatibility`, deferred values (`op://`, `srv://`, `file://`, `$()`) that fail or resolve empty, `${VAR}` references to unset variables, and providers with no resolvable credential.
- A `default_provider` that is neither configured nor built in.

Only providers the config *file* declares are examined, plus the default provider. Loaded config always contains every built-in provider because defaults populate the map, so iterating it would lecture about providers the user never set up.

Deferred value resolution executes the same code the loader runs lazily, including `$()` commands. Use `--skip-deferred` to suppress it.

### `credentials` — stored OAuth credentials

Offline checks over `chatgpt_oauth.json`, `grok_oauth.json`, and `copilot_oauth.json`: file permissions (`--fix` restores `0600`), parse failures, missing access tokens, and expiry. An expired token with a refresh token is informational because the next request renews it; an expired token without one needs `term-llm auth login`.

### `mcp` — MCP servers

Parses `mcp.json` without starting anything: invalid JSON, invalid server definitions, stdio commands that are not on `PATH`, non-HTTP URLs, and duplicate server names. Duplicates matter because the loader decodes into a map, so a shadowed definition disappears silently.

### `stale` — leftover files and rows

- `-wal`/`-shm` files whose database is gone (`--fix` deletes them).
- Write-ahead logs past `DefaultWALWarnBytes` (`--fix` runs `PRAGMA wal_checkpoint(TRUNCATE)`).
- Service `install.lock` files that no process holds.
- Orphaned rows: messages, workspace grants, and queued push notifications in `sessions.db`; runs and run events in `jobs_v2.db`. `--fix` deletes them.

### `binaries` — external commands

Reports `git`, `sh`, `rg`, `ffprobe`, and `node`, plus the CLI required by each configured subprocess provider (`claude`, `grok`, `cursor-agent`, `agy`). Each missing command explains what stops working.

## Adding a check

Implement `doctor.Check` in `internal/doctor`:

```go
type Check interface {
    ID() string
    Title() string
    Run(ctx context.Context) []Finding
}
```

Rules:

- `Run` must not mutate user state. Attach repairs to the finding's `Fix` callback instead; the runner decides whether to call it.
- A repair must be safe to skip and must back up anything it rewrites.
- Give every non-`ok` finding a concrete `Remedy`. A report full of vague warnings gets ignored.
- Register the check in `doctorChecks` in `cmd/doctor.go`, which owns path resolution so checks stay testable with injected paths, and extend `TestDoctorChecksCoverEveryDocumentedArea`.
