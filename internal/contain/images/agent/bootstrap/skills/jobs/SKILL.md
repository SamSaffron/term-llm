---
name: jobs
description: "Work with scheduled jobs and container services. Use when creating, updating, pausing, debugging, or inspecting jobs; checking runs/events; or changing runit services like webui, jobs, and bootstrap-jobs."
---

# Jobs and Services

The container uses two related systems:

- **Services** are long-running runit processes. Definitions live in `/home/agent/.config/term-llm/services/<name>/run` and are reinstalled into `/etc/sv` on container start by `/home/agent/.config/term-llm/init.sh`.
- **Jobs** are scheduled or manual units managed by `term-llm serve jobs`, exposed on `http://127.0.0.1:8080`, and controlled through `term-llm jobs`.

Default services:

| Service | Purpose |
|---|---|
| `webui` | Runs `term-llm serve web` on port 8081. |
| `jobs` | Runs `term-llm serve jobs` on port 8080. |
| `bootstrap-jobs` | One-shot bootstrapper that creates default jobs, then sleeps. |

Default jobs:

| Job | Schedule | Purpose |
|---|---|---|
| `mine-sessions` | every 30 min | Mine session transcripts into memory fragments. |
| `update-recent` | every 10 min | Promote fragments into `recent.md`. |
| `memory-gc` | daily 04:00 UTC | Garbage-collect stale or duplicate memory fragments. |
| `pacman-upgrade` | daily 05:00 UTC | Run `sudo pacman -Syu --noconfirm`. |

## Inspecting jobs

```bash
term-llm jobs list
term-llm jobs get <job-id-or-name>
term-llm jobs runs <job-id-or-name> --limit 20
term-llm jobs active
term-llm jobs run get <run-id>
term-llm jobs run events <run-id> --limit 200
```

Use `--json` when scripting or when you need exact IDs and payloads:

```bash
term-llm jobs --json list
term-llm jobs --json runs mine-sessions --limit 5
```

## Creating a program job

Program jobs execute a command directly from the jobs worker:

```bash
term-llm jobs create --data '{
  "name": "example-program",
  "enabled": true,
  "runner_type": "program",
  "runner_config": {
    "command": "/usr/local/bin/term-llm",
    "args": ["memory", "update-recent", "--agent", "'$AGENT_NAME'"]
  },
  "trigger_type": "cron",
  "trigger_config": {"expression": "*/30 * * * *", "timezone": "UTC"},
  "concurrency_policy": "forbid",
  "timeout_seconds": 300,
  "misfire_policy": "skip"
}'
```

## Creating an LLM job

LLM jobs run agent instructions in the background. Always set `runner_config.agent_name` explicitly.

```bash
term-llm jobs create --data '{
  "name": "daily-review",
  "enabled": true,
  "runner_type": "llm",
  "runner_config": {
    "agent_name": "'$AGENT_NAME'",
    "instructions": "Review recent memory and produce a short status note."
  },
  "trigger_type": "cron",
  "trigger_config": {"expression": "0 9 * * *", "timezone": "UTC"},
  "concurrency_policy": "forbid",
  "timeout_seconds": 1800,
  "misfire_policy": "skip"
}'
```

## Updating and operating jobs

```bash
term-llm jobs update <job-id-or-name> --data '{"enabled": false}'
term-llm jobs pause <job-id-or-name>
term-llm jobs resume <job-id-or-name>
term-llm jobs trigger <job-id-or-name>
term-llm jobs delete <job-id-or-name> --cancel-active
term-llm jobs run cancel <run-id>
```

Prefer `update` over delete/recreate when preserving run history matters.

## Service changes

To add or change a long-running service, edit files under `/home/agent/.config/term-llm/services`. Each service directory needs an executable `run` script. Then restart the container or run the init hook:

```bash
bash /home/agent/.config/term-llm/init.sh
```

Debug service logs through Docker output for the workspace, or enter the container and inspect `/etc/sv`, `/etc/runit/runsvdir`, and the service `run` script.
