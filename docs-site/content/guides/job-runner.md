---
title: "Job runner"
weight: 7
description: "Run the jobs server, define scheduled work, and manage job definitions and runs from the CLI or API."
featured: true
kicker: "Operations"
source_readme_heading: "Job Runner (Serve)"
next:
  label: MCP servers
  url: /guides/mcp-servers/
---
Use the jobs runtime when you want scheduled, delayed, or manually triggered background work:

```bash
term-llm serve --platform jobs --port 8080
```

### Jobs V2 API

Definitions:

- `POST /v2/jobs` - create job definition
- `GET /v2/jobs` - list job definitions
- `GET /v2/jobs/:id` - get definition
- `PATCH /v2/jobs/:id` - update definition
- `DELETE /v2/jobs/:id` - delete definition
- `POST /v2/jobs/:id/trigger` - trigger manual run
- `POST /v2/jobs/:id/pause` - disable schedule
- `POST /v2/jobs/:id/resume` - re-enable schedule

Runs:

- `GET /v2/runs` - list runs (optional `job_id`)
- `GET /v2/runs/:id` - get run details
- `GET /v2/runs/:id/events` - get run event timeline
- `POST /v2/runs/:id/cancel` - cancel run

### Jobs CLI

Use the first-class CLI for interrogation and queue control:

```bash
# Point to a server (or set TERM_LLM_JOBS_SERVER / TERM_LLM_JOBS_TOKEN)
term-llm jobs --server http://127.0.0.1:8080 --token "$TOKEN" list

# Create/update from JSON or YAML
term-llm jobs create --file job.yaml
term-llm jobs update nightly-summary --file update.yaml

# Queue and control execution
term-llm jobs trigger nightly-summary
term-llm jobs pause nightly-summary
term-llm jobs resume nightly-summary
term-llm jobs delete nightly-summary --cancel-active

# Interrogate runs/events
term-llm jobs runs nightly-summary --limit 100
term-llm jobs run get run_abc123
term-llm jobs run events run_abc123
term-llm jobs run cancel run_abc123
```

### Trigger Types

- `manual`: run only when manually triggered
- `once`: delayed one-off run via `trigger_config.run_at` (RFC3339)
- `cron`: recurring schedule via `trigger_config.expression` + `trigger_config.timezone`

### LLM job persistence and progressive state

LLM jobs now persist a session trail to the normal sessions SQLite store **by default**.

That means each LLM run gets a stable `session_id`, and long-running progressive jobs no longer keep their best-so-far state only in memory.

Practical effects:

- `GET /v2/runs/:id` returns `session_id` for LLM runs
- progressive LLM jobs update `response` during execution with the latest progressive envelope
- you can inspect the full message and tool history in the sessions DB using that `session_id`

Default behavior is persistence **on**. If you explicitly do not want that, set:

```json
{
  "runner_type": "llm",
  "runner_config": {
    "agent_name": "developer",
    "instructions": "Do the thing",
    "persist_session": false
  }
}
```

You can also provide your own `session_id` when an integration wants a stable external key:

```json
{
  "runner_type": "llm",
  "runner_config": {
    "agent_name": "developer",
    "instructions": "Investigate the failing deploy and summarize it.",
    "session_id": "deploy-investigation-2026-03-14"
  }
}
```

### Inspecting partial progressive output

For progressive LLM jobs, the latest `update_progress` / `finalize_progress` envelope is written into the run record while the job is still running.

That gives you two inspection paths:

- `term-llm jobs run get <run-id>` for the latest envelope and run metadata
- `term-llm sessions show <session>` or direct SQLite inspection for the full persisted transcript

### Retention

Jobs v2 automatically prunes historical data to avoid unbounded disk growth:

- terminal runs older than 30 days are deleted
- event rows older than 30 days are deleted
- terminal runs are capped to 1000 per job (oldest dropped first)

### Example: Daily Midnight (Cron)

```json
{
  "name": "nightly-summary",
  "enabled": true,
  "runner_type": "llm",
  "runner_config": {
    "agent_name": "developer",
    "instructions": "Summarize today's changes and write a short report."
  },
  "trigger_type": "cron",
  "trigger_config": {
    "expression": "0 0 * * *",
    "timezone": "America/Los_Angeles"
  },
  "timeout_seconds": 900
}
```

### Example: One-Off Delayed Job

```json
{
  "name": "run-once-at-midnight",
  "enabled": true,
  "runner_type": "program",
  "runner_config": {
    "command": "echo",
    "args": ["hello from delayed run"]
  },
  "trigger_type": "once",
  "trigger_config": {
    "run_at": "2026-02-22T00:00:00-08:00"
  },
  "timeout_seconds": 60
}
```
