---
title: "Hub"
weight: 16
description: "Run term-llm Hub: one dashboard over many term-llm web nodes, with server-side token injection and one-click open."
kicker: "Deploy agents"
---

`term-llm serve hub` runs the **term-llm Hub**: a launcher and control plane that puts every term-llm web node you operate behind one pane of glass. The dashboard shows each node with live reachability, latency, and (when detected) its agent name, version, and capabilities, and opens any node's full web UI through the hub with a single click.

```bash
term-llm serve hub
# term-llm Hub listening on http://127.0.0.1:8090
#   auth: bearer required
#   generated Hub bearer token: ...
```

Hub auth is deliberately simple: by default the dashboard, `/api/nodes`, and
`/node/<id>/...` require one Hub bearer token. `/healthz` stays unauthenticated
for load balancers and uptime probes. Provide a stable token with `--token` or
`TERM_LLM_HUB_TOKEN` when running behind a public reverse proxy. Treat that
bearer as an operator/admin credential: anyone holding it can add nodes and make
the Hub connect to addresses reachable from the Hub host. Use `--auth none` only
for loopback-only local development.

## Nodes

The core object is a **node**: a reachable term-llm serve (web/API endpoint) with an identity, a URL + base path, an optional web bearer token, and a source. Nodes are discovered from three resolvers, re-resolved on every request so changes are picked up without a restart:

1. **Static config** (`--config nodes.yaml`) — YAML or JSON:

   ```yaml
   nodes:
     - name: jarvis
       url: http://127.0.0.1:8081/chat
       token: <web bearer token>
     - id: edge
       url: https://edge.example.com
       base_path: /ui
       token: <token>
   ```

    `id` is derived from `name` when omitted; the base path may be embedded in the URL or given explicitly. Hub v1 requires a non-root base path such as `/chat` because path-based proxying needs a stable prefix to rebase.

    Nodes support two connection modes:

    - `connection: direct` (the default): the Hub dials the node's `url`.
    - `connection: reverse`: the node dials the Hub and keeps a websocket open, so the node can live behind NAT, in Docker, on a laptop, or in a private cloud network with no inbound port.

    A reverse node still needs a stable `id`, `base_path`, and `token`, but it does not need a Hub-reachable `url`:

    ```yaml
    nodes:
      - id: artist
        name: Artist
        connection: reverse
        base_path: /chat
        token: <artist web bearer token>
        delegation:
          enabled: true
          accept_from: [jarvis]
          workdir: /work
    ```

    Start the private node with reverse connect enabled:

    ```bash
    term-llm serve web jobs \
      --base-path /chat \
      --token "$ARTIST_TOKEN" \
      --hub-url https://hub.example.com \
      --hub-node-id artist \
      --hub-connect reverse
    ```

    The Hub still exposes the same `/node/artist/...` proxy and delegation APIs. The only difference is transport: direct nodes use Hub → node HTTP, reverse nodes use the node's outbound websocket.

    Nodes do **not** need to be local. A direct node can be another process on the same machine, a Docker/contain workspace, a VM, a cloud runner, or a remote server reachable over a private network/tunnel. If the Hub cannot reach the node, use `connection: reverse` and let the node maintain the outbound connection instead.

2. **Contain workspaces** (on by default, disable with `--contain=false`) — every local `term-llm contain` workspace with a provisioned `WEB_TOKEN` in its `.env` shows up automatically, using its `WEB_PORT`/`WEB_BASE_PATH`. To wire up [dv (Discourse Vibe)]({{< relref "dv-hub" >}}) containers as reverse nodes, see the step-by-step [Discourse Vibe + Hub]({{< relref "dv-hub" >}}) guide.

3. **Dashboard-added nodes** — the **Add node** form (with a **Test connection** button) persists nodes to a local JSON store (`--nodes-file`, default `<data-dir>/hub/nodes.json`, mode 0600 since it holds tokens).

When two sources produce the same node id, precedence is config → local store → contain.

The reverse connection is intentionally a transport choice, not a second Hub API. Delegation, node opening, token injection, and policy checks all continue to target the same node record; the Hub chooses direct HTTP or the reverse websocket based on `connection`. The socket is kept alive with websocket pings and read deadlines on both sides, so silent network drops are detected and the node reconnects. Reverse mode does not queue work while the node is offline: the dashboard shows it as disconnected and requests fail fast until it reconnects.

## Opening a node

Each node's **Open** action navigates to `/node/<id>/`, a proxy onto that node's serve. For direct nodes the Hub dials the configured URL; for reverse nodes the Hub sends the same request over the node's connected websocket.

- The node's bearer token is injected **server-side**; tokens never reach the browser, and client-supplied `Authorization`, `Cookie`, and `X-Api-Key` headers are stripped before forwarding.
- The node UI's baked-in base path (`<base>` tag and `window.TERM_LLM_UI_PREFIX`) is rebased onto `/node/<id>` so the SPA's API calls, service worker, and subresources all route back through the hub.
- The hub injects `window.TERM_LLM_HUB` into the node's page, so the node's web UI shows a **Back to Hub** link in the sidebar (below Widgets).
- Direct-node SSE and other long-lived streams pass through untouched; only connection and response-header times are bounded. Reverse-node requests are carried over the node's outbound websocket with bounded per-request queues; if one proxied client stops consuming a stream, the Hub cancels that request rather than blocking the whole reverse node tunnel.

A node can also be made hub-aware when opened directly (not through the proxy):

```bash
term-llm serve web --hub-url http://127.0.0.1:8090/ --hub-node-id jarvis --hub-node-name Jarvis
```

## API

```text
GET    /api/nodes        nodes with probe status (never includes tokens)
POST   /api/nodes        add a node to the local store
DELETE /api/nodes/<id>   remove a local-store node
POST   /api/nodes/test   probe a node spec without persisting it
ANY    /node/<id>/...    reverse proxy to the node's serve
GET    /api/connect      reverse-node websocket endpoint (node auth)
GET    /healthz          hub health

POST   /api/delegations              create a cross-node delegation (node auth)
GET    /api/delegations              list delegations (node auth or same-origin)
GET    /api/delegations/<id>         delegation status, refreshed from the target
POST   /api/delegations/<id>/cancel  cancel a delegation (originating node only)
```

Probes hit each node's `{base}/healthz` with the node token. Serves report their agent name, version, and capabilities (`web`, `api`, `jobs`, `widgets`, `voice`) on `healthz` only to callers presenting the valid bearer token (or when the serve runs with auth disabled). Hub dashboard/API/proxy routes require the Hub bearer token when `--auth bearer` is active; `/api/connect` and node-originated delegation calls use node auth instead so reverse nodes and `hub_delegate` do not need a separate Hub user account.

The dashboard also shows lightweight diagnostics on each node card when the Hub can spot a likely configuration problem:

- reverse nodes that have not connected their outbound websocket
- nodes without a configured bearer token
- `delegation.enabled: true` without a delegation `workdir`
- nodes that accept delegation but do not report the `jobs` capability (or whose jobs capability cannot be verified)
- obvious origin/target policy mismatches, such as an origin whose `to` allows a target that does not `accept_from` that origin

These diagnostics are advisory and token-safe; `/api/nodes` still never returns node tokens or full secret-bearing config.

## Security posture

The hub defaults to bearer auth: `--auth bearer` protects the dashboard, Hub APIs, and node proxy with a single Hub token. (`/healthz` is intentionally public and returns only `{"status":"ok","role":"hub"}`.) Set the bearer explicitly with `--token` or `TERM_LLM_HUB_TOKEN` for stable deployments; otherwise the hub prints a generated token at startup. Treat this token as an operator/admin secret: a holder can add or test nodes pointing at any address the Hub can reach and can proxy through those nodes. `--auth none` is available for local development, but it is loopback-only because anyone who can reach an unauthenticated hub can reach every node it fronts. Reverse nodes authenticate their websocket with the node id plus the node's bearer token; the hub accepts that connection only for nodes configured with `connection: reverse`, and the node-side connector forwards only requests under its configured base path.

The backend transport never uses an environment proxy (`HTTP_PROXY` would see injected tokens). The hub still rejects obvious cross-site browser requests and requires JSON content types for mutating node-registry APIs as defense-in-depth around the simple bearer gate.

Routing is path-based (`/node/<id>/...`) in v1; the proxy target is resolved per request, so host-based routing can be layered on later without changing the proxy plumbing. Because path routing puts hub UI and proxied node UI on the same browser origin, Hub v1 treats registered nodes/widgets as trusted. The node web UI namespaces localStorage by hub node id to avoid ordinary state collisions; this is not a security boundary. Untrusted remote nodes/widgets still need the future host-based/widget-grant isolation work before they should be opened through a shared hub origin.

Node self-registration, scheduling, hub-level user auth, and mTLS between hub and nodes are deliberately out of scope for v1.

## Cross-node delegation

An agent on one node can delegate work to another node **through the hub** — nodes never talk to each other directly and never see each other's tokens. The flow:

1. The agent on node A calls the `hub_delegate` tool (`target_node`, `prompt`, optional `agent_name`/`model`/`cwd`/`timeout_seconds`).
2. The tool calls `POST /api/delegations` on the hub, authenticating **as node A** with A's own serve token plus an `X-Term-LLM-Node-ID` header. The hub verifies the token against the node's stored token (constant-time); nodes the hub holds no token for can never authenticate.
3. The hub checks policy, then uses **node B's token** (which only the hub holds) to create and trigger a manual jobs-v2 LLM job on B. The job's instructions carry a provenance preamble (delegation id, origin, depth, chain) and the job is labelled `hub_delegation` for traceability.
4. `hub_delegate` returns a `delegation_id` immediately (or blocks with `wait: true`). `hub_check_delegation` polls the hub, which polls the target run and returns the final response.
5. The Hub dashboard also polls active delegation runs from the list view. If the target returns Markdown links or image links (for example `![result](/chat/files/result.svg)`), the dashboard surfaces the artifact inline while preserving the raw response text.

### Delegation policy

Policy lives on the node entry in the hub config. **Default off**: a node with no `delegation.enabled: true` can neither originate nor accept delegated work. Once enabled, `to` and `accept_from` can narrow which nodes may talk; accepting still requires a `workdir`.

```yaml
nodes:
  - name: jarvis
    url: http://127.0.0.1:8081/chat
    token: <web bearer token>
    delegation:
      enabled: true       # REQUIRED: delegation is otherwise completely off
      to: ["*"]           # node ids this node may delegate to (default: all once enabled)
      accept_from: ["*"]  # node ids accepted from (default: all once enabled + workdir set)
      workdir: /work      # REQUIRED to accept; delegated jobs start here
      max_in_flight: 4    # concurrent delegations targeting this node (default 4)
      allowed_agents: []  # agents origins may request (default: developer only)
      allowed_models: []  # model overrides origins may request (default: none)
```

`allowed_agents` defaults to the default delegation agent only (`developer`); `"*"` allows any plain agent name, but path-like names (containing `/`, `\`, `..`, or leading `.`/`~`) must be listed exactly — agent names can resolve to files on the target node. `allowed_models` defaults to refusing every model override (the target's own default model is used); list models or `"*"` to open it up.

Contain workspaces opt in automatically when their compose file declares an `x-term-llm.workspace` path — the sandbox accepts delegations with that path as the workdir (default agent only, no model overrides). Static/manual nodes must set `delegation.enabled: true` explicitly. An explicit `cwd` on a delegation must resolve inside the target's workdir.

Loop and load protection: chains are capped at depth 3, a target already in the chain is refused, and in-flight caps apply hub-wide, per origin, and per target. Chaining is anchored in hub-written provenance for delegated jobs: a delegated job carries a `hub_delegation` label, the jobs-v2 runner exposes it to the tools, and `hub_delegate` attaches `parent_delegation_id` from it automatically. A manually supplied `parent_delegation_id` is still verified against the ledger (the parent must target the delegating node). Treat depth/loop checks as cooperative guardrails: a compromised node that calls the Hub API directly can start a fresh root delegation by omitting the parent id, so the in-flight caps and node allowlists are the real blast-radius controls.

### What the workdir does — and does not — protect

The delegation workdir scopes where the delegated job **starts** (its `cwd`) and where its file tools are rooted. It is **not an OS sandbox**: a delegated agent whose target-node agent definition enables `shell` (the default `developer` agent does) executes commands with the target serve process's normal privileges and can touch anything that user can. Treat `accept_from` + `allowed_agents` as the real policy boundary, and use [contain workspaces]({{< relref "agent-containers" >}}) when you want delegated work inside an actual container sandbox.

### Artifact-returning delegations

A useful pattern is an origin agent delegating a concrete artifact to a specialist node, then showing the returned link to the user:

```text
User asks Jarvis: "ask Artist to draw a hub-and-spoke robot"
Jarvis calls hub_delegate(target_node="artist", prompt="create /home/agent/Files/hub-artist-demo.svg and return ![Hub artist demo](/chat/files/hub-artist-demo.svg)")
Hub runs a jobs-v2 job on Artist
Artist writes the file and returns the Markdown image link
Jarvis calls hub_check_delegation and displays the returned image/link
Hub dashboard shows the delegation status plus the inline artifact preview
```

The link is the target deployment's normal served file URL. Hub does not copy artifacts between nodes in v1. For user-facing replies, have the origin agent display the returned Markdown link directly when that path is reachable from the user's web surface, or have the target return an absolute `https://...` URL.

### Node-side setup

A node started with `serve web jobs --hub-url ... --hub-node-id ...` configures the delegation tools in-process from its own serve token. Add `--hub-connect reverse` when the node should maintain an outbound websocket to the Hub instead of requiring the Hub to reach its URL directly. The target node must run with `jobs` enabled so the hub can create and trigger the delegated jobs-v2 run. Standalone processes can export `TERM_LLM_HUB_URL`, `TERM_LLM_HUB_NODE_ID`, and `TERM_LLM_HUB_TOKEN` instead; the token is captured at startup and **scrubbed from the process environment**, so subprocesses spawned by tools (shell commands, custom tools, widgets, MCP servers) never inherit it. It is also never injected into browser-facing HTML or config.

`hub_delegate` and `hub_check_delegation` are not enabled in any builtin agent. Enable them explicitly on the agents that should delegate:

```yaml
tools:
  enabled: [read_file, shell, hub_delegate, hub_check_delegation]
```

### Delegation security posture

- **No token movement**: node A authenticates with its own credential; the hub alone holds B's token; delegation records and API responses never contain tokens, and target-node error bodies are redacted before they travel back.
- **Default off**: nodes cannot originate or accept delegations unless `delegation.enabled: true` is set; accepting also requires a workdir. `to`, `accept_from`, `allowed_agents`, and `allowed_models` narrow the enabled surface.
- **Bounded execution**: delegated work runs through the standard jobs-v2 path on the target with a clamped timeout, starting inside the target's declared workdir (a cwd/file-tool scope, not an OS sandbox — see above).
- **Scoped visibility**: a node may read only delegations it originates or targets; the full list is reserved for the hub operator's same-origin dashboard.
- The delegation ledger (`<data-dir>/hub/delegations.json`, mode 0600) holds prompts/response excerpts for audit; terminal records are pruned after 7 days.
- All hub→node and node→hub clients dial directly (no `HTTP_PROXY`), since those requests carry bearer tokens.
