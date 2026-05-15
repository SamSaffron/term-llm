---
title: "Agent Containers"
weight: 15
description: "Create isolated, persistent term-llm agents with the built-in Docker Compose workspace manager."
kicker: "Deploy agents"
---

`term-llm contain` creates named Docker Compose workspaces for long-running agents. Each workspace has its own compose file, `.env`, Docker volume, Web UI, jobs service, memory database, sessions, skills, and agent configuration.

The current happy path is `term-llm contain new` followed by `term-llm contain start`. You do not need to clone the term-llm repo or hand-copy seed directories.

## Prerequisites

- Docker with Compose v2
- `term-llm` installed on the host
- At least one provider credential, or a plan to configure credentials inside the container later

## Create an agent workspace

```bash
term-llm contain new myagent
```

By default this uses the managed `agent` template. It creates a global workspace under your term-llm config directory:

```text
~/.config/term-llm/containers/myagent/
├── compose.yaml       # source of truth for Docker Compose
├── .env               # provider credentials, Web UI token/port, image settings
└── README.md          # workspace-specific commands and notes
```

`contain new` prompts for provider credentials and useful settings unless you pass `--no-input` and `--set` values:

```bash
term-llm contain new myagent \
  --no-input \
  --set provider=anthropic \
  --set web_port=8081
```

The generated `.env` is private (`0600`). It contains the selected provider, optional API keys/OAuth bootstrap data, `WEB_PORT`, `WEB_BASE_PATH`, and a generated `WEB_TOKEN` for bearer-authenticated Web UI access.

## Start and open it

If you did not start it from the creation prompt:

```bash
term-llm contain start myagent
```

The Web UI is exposed at:

```text
http://localhost:<WEB_PORT>/chat
```

Use the bearer token printed by `contain new` or stored in the workspace `.env`.

You can also chat from the terminal using the built-in exec recipe:

```bash
term-llm contain exec myagent agent
```

Open a shell inside the workspace:

```bash
term-llm contain shell myagent
```

Shells open as the non-root `agent` user in `/home/agent`. Use explicit passwordless `sudo` when root privileges are needed.

## What gets bootstrapped

The Compose service builds the managed agent image and mounts one persistent Docker volume at `/home/agent`. On first boot, the image copies bootstrap files from `/opt/term-llm/bootstrap` into that volume. The image also seeds the term-llm source checkout at `/home/agent/source/term-llm`, with documentation under `/home/agent/source/term-llm/docs-site/content`.

Use `/home/agent/source/<project>` for source checkouts and code projects you want to persist. Use `/home/agent/Files` for downloadable files served by the Web UI. Avoid putting durable work in `/tmp`, `/root`, or other image-only paths.

```text
/home/agent/.config/term-llm/
├── agents/myagent/
│   ├── agent.yaml
│   ├── system.md
│   ├── soul.md
│   ├── memory/recent.md
│   └── scripts/
├── services/
│   ├── webui/run
│   ├── jobs/run
│   └── bootstrap-jobs/run
├── skills/
│   ├── jobs/SKILL.md
│   ├── memory/SKILL.md
│   ├── self/SKILL.md
│   └── widgets/SKILL.md
├── source/
│   └── term-llm/        # runtime source and docs-site/content documentation
└── init.sh
```

After first boot, the volume is the source of truth. Future boots ignore image bootstrap files and run `/home/agent/.config/term-llm/init.sh`, which reinstalls persisted runit service definitions into `/etc/sv` and links them into `/etc/runit/runsvdir`.

The service supervisor runs as root. The Web UI, jobs server, first-boot `bootstrap-jobs` one-shot, shells, and normal agent work run as the `agent` Linux user.

## Services

| Service | What it does |
|---|---|
| `webui` | Runs `term-llm serve web` on port `8081` inside the container. |
| `jobs` | Runs `term-llm serve jobs` on port `8080` inside the container. |
| `bootstrap-jobs` | Creates default scheduled jobs on first boot, then removes its persisted service definition and exits. |

The Web UI service starts with file serving and widgets enabled:

```bash
term-llm serve web \
  --agent myagent \
  --base-path /chat \
  --host 0.0.0.0 \
  --port 8081 \
  --auth bearer \
  --yolo \
  --files-dir /home/agent/Files \
  --enable-widgets \
  --widgets-dir /home/agent/.config/term-llm/widgets
```

Check service status from the host:

```bash
term-llm contain exec myagent -- sv status /etc/runit/runsvdir/webui/
term-llm contain exec myagent -- sv status /etc/runit/runsvdir/jobs/
```

Restart a service:

```bash
term-llm contain exec myagent -- sudo sv restart /etc/runit/runsvdir/webui/
```

## Default jobs

On first boot, `bootstrap-jobs` waits for the jobs API, creates the defaults, and then removes itself from the persisted service set:

| Job | Schedule | What it does |
|---|---|---|
| `mine-sessions` | Every 30 min | Mines session transcripts into memory fragments. |
| `update-recent` | Every 10 min | Promotes recent fragments into `memory/recent.md`. |
| `memory-gc` | Daily at 04:00 UTC | Garbage-collects stale or duplicate memory fragments. |
| `system-upgrade` | Daily at 05:00 UTC | Upgrades distro packages (`pacman` on Arch, `dnf` on Fedora). |

Inspect and operate them inside the workspace:

```bash
term-llm contain exec myagent -- term-llm jobs list
term-llm contain exec myagent -- term-llm jobs runs mine-sessions --limit 10
term-llm contain exec myagent -- term-llm jobs update mine-sessions --data '{"enabled": false}'
```

## Skills and memory

Fresh agent containers include a small default skill set:

| Skill | Purpose |
|---|---|
| `jobs` | Inspect and manage jobs and runit services. |
| `memory` | Search and update persistent memory fragments. |
| `self` | Safely modify the agent's own prompt/config/scripts. |
| `widgets` | Build, install, inspect, and debug Web UI widgets. |

Skills are copied to `/home/agent/.config/term-llm/skills/` and the first-boot `config.yaml` enables the skills system with auto-invocation.

Memory fragments live in term-llm's database on the persistent volume. `memory/recent.md` is seeded empty and then maintained by the default jobs; do not edit it directly.

## Widgets

Add widget apps under:

```text
/home/agent/.config/term-llm/widgets/
```

They are served under:

```text
/chat/widgets/<widget-name>/
```

The bundled `widgets` skill documents the operational workflow: inspect widget support, create manifests, reload the widget registry through `/chat/admin/widgets/reload`, and smoke-test routes. You do not need to restart the Web UI just to add or update a widget; restart it only for service flag changes, binary upgrades, or if the admin reload endpoint is unavailable.

## Updating

Update the host `term-llm` first, then rebuild/recreate the workspace image:

```bash
term-llm contain rebuild myagent
```

State is preserved in the Docker volume. Rebuilds update the managed image and binary but do not overwrite the live agent config already copied into `/home/agent`.

## Removing a workspace

Stopping preserves state:

```bash
term-llm contain stop myagent
```

Removal is destructive and asks for confirmation:

```bash
term-llm contain rm myagent
```

This runs Docker Compose down with volumes and deletes the workspace config directory.

## Running multiple agents

Each workspace is independent:

```bash
term-llm contain new agent-a --set web_port=8081
term-llm contain new agent-b --set web_port=8082
term-llm contain start agent-a
term-llm contain start agent-b
```

Each gets its own Compose project, persistent volume, Web UI token, jobs, memory, sessions, and live agent files.

## Advanced: custom bootstrap seed

The managed image supports an optional static `/seed` mount containing `bootstrap.yaml`. If present, `/seed` replaces the image bootstrap source for first boot only. After the bootstrap sentinel exists on the volume, `/seed` is ignored. For normal use, prefer the built-in `contain new` template and edit the live files through the agent's self-modification workflow.
