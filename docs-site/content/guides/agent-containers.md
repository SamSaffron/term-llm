---
title: "Agent Containers"
weight: 15
description: "Run independent term-llm agents in Docker — one container per agent, fully isolated, no image rebuild needed."
kicker: "Deploy agents"
---

Each agent gets its own Docker container, its own compose file, and its own state volume. Adding a new agent never touches an existing one.

## Prerequisites

- Docker with Compose v2
- A clone of the term-llm repo
- At least one LLM API key (Anthropic, OpenAI, Venice, etc.)

## Scaffold a new agent

From the term-llm repo root:

```bash
docker/init.sh myagent
```

This creates a standalone project directory:

```
myagent/
├── docker-compose.yml
├── .env                ← API keys, web token
├── init.sh             ← boot hook (installs runit services)
├── agents/
│   └── myagent/
│       ├── agent.yaml
│       ├── soul.md      ← voice, values, personality
│       └── system.md    ← operational context
└── services/
    ├── webui/run           ← web UI on port 8081
    ├── jobs/run            ← job scheduler
    └── bootstrap-jobs/run  ← creates default jobs on first boot
```

You can also specify a custom output directory:

```bash
docker/init.sh myagent ~/agents/myagent
```

## Configure

### API keys

Edit `.env` and add at least one API key:

```bash
ANTHROPIC_API_KEY=sk-ant-...
```

The `.env` file also has a pre-generated `WEB_TOKEN` for authenticating the web UI and a `WEB_PORT` you can change if running multiple agents.

### Personality

The scaffold separates personality from operations:

- **`soul.md`** — voice, values, and boundaries. This is who the agent *is*. Edit it to change tone, principles, or guardrails.
- **`system.md`** — operational context. Who the user is, what tools are available, domain-specific instructions. Edit it to change what the agent *knows about its environment*.

Both files are bind-mounted into the container as seed files. On first boot they are copied into the agent's config directory on the state volume. After that, the container owns them — your local copies are the seed, not the live version.

### Agent settings

`agent.yaml` controls model, tools, and behavior:

```yaml
name: myagent
description: "AI assistant"
include: ["soul.md"]
tools:
  enabled: [read_file, write_file, edit_file, glob, grep, shell, ...]
shell:
  auto_run: true
  allow: ["*"]
max_turns: 200
search: true
```

The `include` field loads additional markdown files (like `soul.md`) after the system prompt. See [Agents](/guides/agents/) for the full configuration reference.

## Start

```bash
cd myagent
docker compose up -d
```

The web UI will be at `http://localhost:8081/chat` (or whatever `WEB_PORT` you set).

Authenticate with the bearer token from `.env`:

```
Authorization: Bearer <WEB_TOKEN>
```

## How it works

The container uses the same term-llm Docker image for every agent. No agent-specific files are baked into the image.

On first boot, the entrypoint:

1. Copies seed files from `/seed/` into the state volume (only if they don't already exist)
2. Runs `/seed/init.sh` which installs runit services (web UI, job scheduler, job bootstrapper)
3. Starts runit as PID 1

After the first boot, the state volume owns all agent config. Your local `agents/` directory remains the seed — edit it and delete the volume to re-seed, or edit the live files inside the container directly.

### Services

| Service | What it does |
|---|---|
| `webui` | Web UI and HTTP API on port 8081 |
| `jobs` | Background job scheduler |
| `bootstrap-jobs` | Creates default scheduled jobs on first boot, then sleeps |

On first boot, the `bootstrap-jobs` service waits for the jobs API, then creates four default jobs:

| Job | Schedule | What it does |
|---|---|---|
| `mine-sessions` | Every 30 min | Extracts memory fragments from session transcripts |
| `update-recent` | Every 10 min | Promotes recent fragments into the agent's context |
| `memory-gc` | Daily at 4am UTC | Garbage-collects stale or duplicate memory fragments |
| `pacman-upgrade` | Daily at 5am | Keeps the container's Arch packages current |

These jobs are yours after creation — edit or delete them with `term-llm jobs list` and `term-llm jobs update`.

All services are managed by runit. Check status:

```bash
docker exec myagent sv status /etc/runit/runsvdir/webui/
```

## Running multiple agents

Each scaffolded directory is fully independent. Run as many as you like:

```bash
docker/init.sh agent-a
docker/init.sh agent-b ~/agents/agent-b
```

Set different `WEB_PORT` values in each `.env` to avoid port conflicts:

```bash
# agent-a/.env
WEB_PORT=8081

# agent-b/.env
WEB_PORT=8082
```

Each agent gets its own Docker volume for state, its own compose project, and its own container. They share only the term-llm image.

## Updating

When you pull a new version of term-llm, rebuild the image and restart:

```bash
cd myagent
docker compose up -d --build
```

The state volume persists across rebuilds — agent config, memory, and session history are preserved. The `init.sh` boot hook re-installs runit services on every boot, so new services added to your `services/` directory will be picked up automatically.

## Transcript access

Session transcripts are stored in the state volume. To access them from the host:

```bash
docker exec myagent term-llm sessions list --agent myagent
docker exec myagent term-llm sessions show <session-id> --agent myagent
```

The `mine-sessions` job automatically extracts key facts from transcripts into searchable memory fragments. See the bootstrapped jobs above for the full list of default scheduled tasks.
