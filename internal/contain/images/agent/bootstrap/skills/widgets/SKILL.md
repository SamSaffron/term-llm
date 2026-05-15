---
name: widgets
description: "Build, install, inspect, and debug term-llm Web UI widgets. Use when adding local widget apps, editing widget manifests, checking widget routes, or explaining how /chat/widgets works."
---

# Widgets

Widgets are local web apps proxied by `term-llm serve web` under the Web UI base path.
In this container the `webui` service starts with widgets enabled:

```bash
term-llm serve web \
  --base-path "${WEB_BASE_PATH:-/chat}" \
  --enable-widgets \
  --widgets-dir /home/agent/.config/term-llm/widgets
```

Default widget directory:

```bash
/home/agent/.config/term-llm/widgets
```

Public route shape:

```text
/chat/widgets/<widget-name>/
```

Adjust `/chat` if `WEB_BASE_PATH` is set differently.

## Widget layout

Create one subdirectory per widget:

```text
/home/agent/.config/term-llm/widgets/
└── example/
    ├── widget.yaml
    └── ... app files ...
```

`widget.yaml` is the manifest. Inspect existing widgets or the term-llm source for the exact supported fields before inventing a shape.

Useful source paths:

```bash
/home/agent/source/term-llm/internal/widgets
/home/agent/source/term-llm/cmd/serve.go
```

## Workflow

1. Inspect existing widget support first:

```bash
rg "widget.yaml|enable-widgets|widgets-dir" /home/agent/source/term-llm
```

2. Add or edit files under the widgets directory.
3. Restart the Web UI service if the manager does not pick up the change:

```bash
sudo sv restart /etc/runit/runsvdir/webui
```

4. Smoke the route:

```bash
curl -i http://127.0.0.1:8081/chat/widgets/<widget-name>/
```

For authenticated browser access use the Web UI token from the workspace `.env`.

## Rules

- Keep widget state and source in the persistent `/home/agent/.config/term-llm/widgets` directory unless the user asks for a project-local mount.
- Prefer small, inspectable apps over framework sprawl.
- Do not expose secrets through static assets or client-side JavaScript.
- After changing widgets or service flags, restart `webui` and smoke test the widget route.
