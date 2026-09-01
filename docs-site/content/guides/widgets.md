---
title: "Web UI widgets"
weight: 8
description: "Build and run local web applications proxied through the term-llm Web UI."
kicker: "Web runtime"
---
Widgets are local HTTP applications that term-llm starts on demand and reverse-proxies under the Web UI base path. A widget can be a small static dashboard, an interactive tool, or a stateful application. It is a normal web process rather than a term-llm frontend component, and there is no widget SDK.

The built-in agent can create or modify one interactively:

```bash
term-llm chat @widget-builder
```

## Widget discovery

`term-llm serve web` discovers widgets by default. The standard distribution does not include any widget manifests, so a clean installation has no widget launcher; the Web UI only shows widget controls after it discovers at least one valid widget.

The default widget root is:

```text
~/.config/term-llm/widgets/
```

Choose another root with either the CLI or `config.yaml`:

```bash
term-llm serve web \
  --widgets-dir /path/to/widgets
```

```yaml
serve:
  widgets_dir: /path/to/widgets
```

`serve.widgets_dir` only selects the directory; discovery remains enabled for the Web UI. To turn the widget runtime and its routes off entirely, use:

```bash
term-llm serve web --disable-widgets
```

Agent containers use the same opt-out default and scan `/home/agent/.config/term-llm/widgets`.

## Directory and manifest

Each immediate subdirectory with a `widget.yaml` is one widget:

```text
~/.config/term-llm/widgets/
└── example/
    ├── widget.yaml
    ├── server.py
    └── static/
        └── app.css
```

A manifest contains:

```yaml
title: "Example widget"
description: "A small local dashboard"
mount: example
command: ["python3", "server.py", "--port", "$PORT"]
```

| Field | Required | Meaning |
|---|---:|---|
| `title` | Yes | Human-readable name shown in the Web UI. |
| `description` | No | Short description shown alongside widget status. |
| `mount` | No | Public route segment. Defaults to the directory name. |
| `command` | Yes | Argument array used to start the local HTTP process. |

Mounts must match `^[a-z0-9][a-z0-9-]{0,63}$` and must be unique. If two widgets claim the same mount, the lexicographically first directory wins and the other is reported as a load error.

`command` is executed directly in the widget directory, not through a shell. It must contain one transport placeholder mode:

- `$PORT` for a free loopback TCP port
- `$SOCKET` for a Unix-domain socket

Do not include both. `$PORT` is the simplest portable default. A port-based server should listen on `127.0.0.1`, not `0.0.0.0`.

## Minimal example

This dependency-free Python server is enough to demonstrate the contract:

```python
from argparse import ArgumentParser
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer

parser = ArgumentParser()
parser.add_argument("--port", type=int, required=True)
args = parser.parse_args()

ThreadingHTTPServer(("127.0.0.1", args.port), SimpleHTTPRequestHandler).serve_forever()
```

Add an `index.html` in the same directory and use the manifest above. The root request must receive an HTTP response within ten seconds or startup is marked as failed.

For real widgets, prefer a small, inspectable application. Add a framework only when its interaction or maintenance benefits justify the dependencies, and keep the dependency lockfile with the widget.

## Public paths and proxy behavior

With the default Web UI base path `/ui`, the routes are:

```text
/ui/widgets
/ui/widgets/<mount>/
/ui/admin/widgets/reload
/ui/admin/widgets/status
/ui/admin/widgets/stop
/ui/admin/widgets/<mount>/stop
```

If the server uses `--base-path /chat`, every route starts with `/chat` instead. Do not hard-code either base path in a widget.

term-llm removes `/widgets/<mount>` before forwarding the request. Use relative browser URLs so assets and application routes remain under the widget mount:

```html
<link rel="stylesheet" href="static/app.css">
<script type="module" src="static/app.js"></script>
```

```js
const response = await fetch("api/state");
```

Avoid root-relative URLs such as `/static/app.css` or `/api/state`; those target the term-llm server root instead of the widget. Relative URLs are also the only portable default when a widget is opened through a Hub or another path-rewriting proxy.

When an absolute public path is genuinely unavoidable, derive it **for each request** from the `X-Forwarded-Prefix` request header. term-llm normalizes this header before proxying to the widget, so the widget receives its complete browser-visible mount, for example `/chat/widgets/example` directly or `/hub/node/jarvis/widgets/example` through a Hub. `X-Forwarded-Host` and `X-Forwarded-Proto` likewise describe the browser-visible origin when a complete absolute URL is required. These headers are routing hints from the current request's proxy chain, not authenticated facts; do not use them for authorization or other security decisions. Do not use a process-global value for browser URLs.

`BASE_PATH` and `TERM_LLM_WIDGET_BASE_PATH` remain available as the node-local startup path for compatibility with non-request work, but they cannot represent every public route when one widget process serves both direct and Hub traffic.

The widget process receives these environment variables:

```text
TERM_LLM_WIDGET_ID
TERM_LLM_WIDGET_MOUNT
TERM_LLM_WIDGET_BASE_PATH
BASE_PATH
TERM_LLM_WIDGET_HOST
TERM_LLM_WIDGET_PORT
HOST
PORT
TERM_LLM_WIDGET_SOCKET
SOCKET
```

Only the variables for the selected port or socket mode are set.

## Lifecycle and persistence

A widget starts lazily when its route is first requested. term-llm waits for the widget root to respond, then begins proxying the request. An idle widget is stopped after ten minutes by default and will start again on a later request. Running widgets can also be stopped from the Web UI widget launcher; this does not remove the widget, and opening it again starts a fresh process.

Consequences for application design:

- Keep startup below ten seconds.
- Do not download dependencies or compile production assets on every start.
- Do not keep important state only in memory.
- Store durable state in the persistent widget directory or another deliberately configured location.
- Flush state safely and tolerate process termination or restart.

Widget stdout and stderr are written to the term-llm server logs.

## Reload and inspect

After adding, removing, or changing a manifest, reload the registry instead of restarting the whole Web UI. These examples assume a `/ui` base path and a bearer token already stored in `TOKEN`:

```bash
curl -fsS -X POST \
  -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/ui/admin/widgets/reload

curl -fsS \
  -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/ui/admin/widgets/status

curl -fsS -X POST \
  -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/ui/admin/widgets/example/stop

# Stop every running widget without shutting down term-llm.
curl -fsS -X POST \
  -H "Authorization: Bearer ${TOKEN}" \
  http://127.0.0.1:8080/ui/admin/widgets/stop
```

On POSIX systems, sending `SIGUSR1` to the widgets-enabled `term-llm serve web` process also stops all widget subprocesses while leaving the Web UI running:

```bash
kill -USR1 <serve-web-pid>
```

Target the Web UI process specifically; other term-llm commands do not install this widget signal handler. The authenticated admin endpoint is preferable for scripts and agents because it does not require discovering the server PID.

Open the proxied widget with its trailing slash so relative browser URLs resolve correctly:

```text
http://127.0.0.1:8080/ui/widgets/example/
```

A manifest reload is enough for registry changes. Reloading does not restart an already running process; after changing a running widget's application code, stop that mount and let the next request start it again. Restart the Web UI only when changing service flags, changing the widget root, upgrading the binary, or recovering from an unavailable admin endpoint.

## Security model

A widget command is trusted local code. It runs as the same OS user as term-llm and inherits the server process environment; widgets are not sandboxed. Install only code and dependencies you trust, and never enumerate or expose inherited environment variables.

The Web UI authentication layer protects widget routes, but the proxy strips `Authorization` and `Cookie` before forwarding requests. A widget therefore does not receive the Web UI token, login cookie, authenticated username, or per-user identity. Do not implement behavior that assumes those values are present.

Keep secrets out of HTML, JavaScript, CSS, generated images, manifests, and client-visible API responses. Validate mutating input and write persistent data safely even though the outer route is authenticated.

## Troubleshooting

- **Widget is missing:** confirm the effective widgets directory, make sure `--disable-widgets` is not set, and check that `widget.yaml` is in an immediate child directory.
- **Load error:** inspect `/admin/widgets/status` for malformed YAML, a missing title or command, an invalid mount, duplicate mounts, or missing `$PORT`/`$SOCKET`.
- **Startup timeout:** run the manifest command manually with a test port, verify loopback binding, and make sure `/` responds quickly.
- **HTML loads but assets fail:** replace root-relative browser URLs with relative URLs.
- **State disappears:** move it from process memory into persistent storage under the widget directory.
- **Container route differs:** agent containers normally use `/chat`, while a standard host defaults to `/ui`; follow the server's configured base path.
