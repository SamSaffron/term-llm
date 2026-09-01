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

## Passkey authentication

Bearer authentication remains the default for compatibility. Public human-facing
Hubs can opt into phishing-resistant WebAuthn/passkey login and short-lived,
server-side browser sessions:

```bash
term-llm serve hub \
  --auth passkey \
  --public-url https://hub.example.com/hub/ \
  --base-path /hub \
  --host 127.0.0.1 \
  --port 8090
```

Passkey mode requires a stable domain and HTTPS. The only HTTP exception is
`http://localhost[:port]` for local development; loopback IP literals are not
accepted. `--public-url` (or `TERM_LLM_HUB_PUBLIC_URL`) is the authoritative
browser origin and relying-party ID. Hub never trusts `Host`, `Forwarded`, or
`X-Forwarded-*` to derive WebAuthn security settings. When `--base-path` is not
explicit, it is derived from the public URL. If both are given, their normalized
paths must match.

On a fresh interactive start, Hub prints a random one-time setup code valid for
ten minutes. Open the printed `/auth/setup` URL, enter the code, and enroll the
first passkey. The code is never put in a URL or page. Redirected output,
systemd, and containers must use a private secret file:

```bash
openssl rand -base64 32 | sudo install -m 600 /dev/stdin /run/secrets/term-llm-hub-bootstrap
term-llm serve hub --auth passkey \
  --public-url https://hub.example.com/hub/ \
  --passkey-bootstrap-token-file /run/secrets/term-llm-hub-bootstrap
```

`TERM_LLM_HUB_BOOTSTRAP_TOKEN` is also supported and is scrubbed from the
process environment after capture. A file takes precedence. The explicit
`--print-passkey-bootstrap-token` escape hatch prints a generated code to
non-interactive output, but makes service logs temporary enrollment credentials.
Remove the bootstrap secret after enrollment. Credentials persist in
`<data-dir>/hub/auth.json` (or `--passkey-auth-file`). Browser sessions persist
in a private `sessions.json` beside that credential store, so valid sessions
survive Hub restarts. Hub takes an exclusive lock on the session store to prevent
overlapping processes from clobbering revocations. Corrupt disposable session
state is quarantined and reset, requiring browser reauthentication instead of
preventing Hub startup. A custom auth file's immediate parent must already be
private (mode `0700` on Unix); Hub rejects an unsafe parent rather than changing
a shared directory's permissions.

The Security panel can add/name multiple passkeys, remove any non-final
credential, show the active session count, revoke other sessions, and sign out.
Adding or deleting a passkey requires a fresh passkey assertion. Sessions have a
12-hour idle and seven-day absolute lifetime. The browser receives a host-only,
HttpOnly, SameSite=Strict cookie scoped to the Hub mount; only its SHA-256 hash
is stored. Session activity checkpoints are rate-limited to five-minute
intervals and written atomically; security changes such as logout and revocation
are written immediately. Recent-auth grants for sensitive credential changes
remain process-local and must be renewed after a restart. Hub retains at most 1,024
active browser sessions; if that defensive limit is ever reached, stop Hub and
remove `sessions.json` to sign out every browser before restarting.

### Reverse proxy example

Terminate TLS at the proxy while keeping the Hub backend on loopback. The
browser must always use the exact configured public URL:

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    http2 on;
    server_name hub.example.com;

    ssl_certificate     /etc/letsencrypt/live/hub.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hub.example.com/privkey.pem;

    location = /hub { return 308 /hub/; }
    location /hub/ {
        # No trailing slash: preserve /hub for Hub's own base-path router.
        proxy_pass http://127.0.0.1:8090;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        # Overwrite, rather than append, client identity at this trust boundary.
        proxy_set_header X-Forwarded-For $remote_addr;
    }
}
```

Start Hub with `--public-url https://hub.example.com/hub/ --base-path /hub`
and explicitly trust only the TLS proxy's direct address for authentication rate
limits:

```text
--passkey-trusted-proxy 127.0.0.1/32
```

Without that flag Hub ignores `X-Forwarded-For`; this prevents clients from
spoofing addresses to evade authentication limits. When the direct peer matches
a configured trusted IP/CIDR, Hub walks `X-Forwarded-For` from right to left and
uses the first untrusted address. Configure every proxy hop deliberately and
have the edge proxy overwrite client-supplied forwarding headers. If the header
is absent or malformed, Hub fails closed to the direct proxy address, which
means all clients share one rate-limit bucket until the proxy configuration is
fixed. Forwarding headers never override the configured WebAuthn origin or RP
ID. Do not browse the loopback backend URL: it is not the WebAuthn origin.

### Host-controlled recovery

Recovery adds one replacement passkey without deleting existing credentials or
granting access to Hub data. With at least one credential already enrolled:

1. Stop Hub and create a cryptographically random private file, for example
   `/run/secrets/term-llm-hub-recovery`, mode `0600`.
2. Restart with `--passkey-recovery-token-file` (or temporarily set
   `TERM_LLM_HUB_RECOVERY_TOKEN`).
3. Within ten minutes, open `<public-url>/auth/recover`, enter the secret, and
   enroll one replacement passkey.
4. Remove/unmount the recovery secret and restart without the recovery option.
5. Sign in normally, revoke other sessions if compromise is suspected, then
   remove obsolete passkeys from Security.

Deleting `auth.json` is a destructive identity reset, not recovery. A deliberate
identity reset must also remove the adjacent `sessions.json`; session stores are
bound to the passkey identity and Hub refuses to reuse one with a replacement
identity.

For an interactive process, the complete recovery run is:

```bash
umask 077
openssl rand -base64 32 > /tmp/term-llm-hub-recovery
term-llm serve hub \
  --auth passkey --public-url https://hub.example.com/hub/ --base-path /hub \
  --host 127.0.0.1 --port 8090 \
  --passkey-recovery-token-file /tmp/term-llm-hub-recovery
# Enroll at https://hub.example.com/hub/auth/recover, then Ctrl-C.
rm -f /tmp/term-llm-hub-recovery
# Restart the original command without --passkey-recovery-token-file.
```

For systemd, use a credential rather than an environment variable or journal
output. Adapt the full `ExecStart` to match the installed unit:

```bash
sudo sh -c 'umask 077; openssl rand -base64 32 > /etc/term-llm-hub-recovery'
sudo systemctl edit term-llm-hub
# [Service]
# LoadCredential=hub-recovery:/etc/term-llm-hub-recovery
# ExecStart=
# ExecStart=/usr/local/bin/term-llm serve hub --auth passkey \
#   --public-url https://hub.example.com/hub/ --base-path /hub \
#   --host 127.0.0.1 --port 8090 \
#   --passkey-recovery-token-file %d/hub-recovery
sudo systemctl daemon-reload
sudo systemctl restart term-llm-hub
# Enroll at /hub/auth/recover, then run `sudo systemctl edit term-llm-hub`
# again and remove the temporary LoadCredential/ExecStart override.
sudo rm -f /etc/term-llm-hub-recovery
sudo systemctl daemon-reload && sudo systemctl restart term-llm-hub
```

For a container, mount a one-use host file read-only, then recreate the
container without the mount and recovery flag immediately after enrollment:

```bash
umask 077; openssl rand -base64 32 > ./hub-recovery
# Add temporarily to the container invocation:
#   -v "$PWD/hub-recovery:/run/secrets/hub-recovery:ro"
#   --passkey-recovery-token-file /run/secrets/hub-recovery
# Enroll at /hub/auth/recover, stop and recreate without both lines, then:
rm -f ./hub-recovery
```

### Browser compatibility checklist

Before promoting a public deployment, exercise the passkey flow with Safari and
iCloud Keychain/Touch ID, Chrome or Edge with a platform/synced passkey,
Firefox with a roaming security key, phone-to-desktop hybrid/QR, and a hardware
security key as a second credential. Also test root and non-root mounts, direct
HTTPS and TLS-terminating reverse proxy deployments, `http://localhost`, login
after a Hub restart, and recovery enrollment while existing credentials remain.
Record authenticator/browser combinations that are not supported rather than
weakening exact-origin or user-verification checks.

The repository's automated Chromium virtual-authenticator smoke covers an
internal/platform authenticator, a second USB/roaming authenticator, an HTTPS
reverse proxy with a non-root mount, logout/relogin, restart, and recovery while
an existing credential remains. Unit and HTTP integration tests cover the
`http://localhost` configuration exception and root mount. No unsupported
combination is currently known.
Safari/iCloud Keychain, Firefox with physical roaming keys, hybrid QR, real
hardware keys, and direct/reverse-proxied production HTTPS still require the
release-time physical-device matrix above; do not infer support by weakening
policy when one of those combinations fails.

An explicitly configured `--token`/`TERM_LLM_HUB_TOKEN` remains accepted in the
`Authorization` header for automation or emergency API access, but passkey mode
never auto-generates one and never accepts query-token login or the legacy
browser token cookie. The startup banner reports when this compatibility
credential is enabled and whether it came from the flag or environment. When
converting an existing bearer deployment, remove `TERM_LLM_HUB_TOKEN` if the
intended result is passkey-only access; inherited systemd/container environment
still counts as explicit bearer configuration.


## Run as a systemd service

An example unit lives at
[`ops/systemd/term-llm-hub.service`](https://github.com/samsaffron/term-llm/blob/main/ops/systemd/term-llm-hub.service)
so the Hub survives reboots and restarts on crashes. Copy it, adjust the
`ExecStart` flags, give it the tokens, and enable it:

```bash
sudo cp ops/systemd/term-llm-hub.service /etc/systemd/system/
sudo install -m 600 /dev/null /etc/term-llm-hub.env
echo "TERM_LLM_HUB_TOKEN=$(openssl rand -base64 32 | tr -d '=+/')" | sudo tee -a /etc/term-llm-hub.env >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --now term-llm-hub
journalctl -u term-llm-hub -f
```

Tokens live in `/etc/term-llm-hub.env` (mode 0600) rather than the unit, so
they never appear in `systemctl show` or process listings; the Hub reads them
via `TERM_LLM_HUB_TOKEN` and `TERM_LLM_HUB_REGISTRATION_TOKEN`.

The example defaults to a sandboxed service (`DynamicUser=`, no home access):
nodes are added via the dashboard, a `--config` file, or reverse
self-registration. Auto-discovery of local `term-llm contain` workspaces needs
access to their `.env` files and loopback ports, so the unit includes a
commented root variant for that setup — or run the unit as a [systemd user
service](https://www.freedesktop.org/software/systemd/man/latest/systemd.unit.html)
under the account that owns the workspaces (`loginctl enable-linger <user>`)
to keep discovery without root. To stop and remove the service, run
`sudo systemctl disable --now term-llm-hub`.

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

For a long-running Linux node, see the repository's
[`serve web` systemd example](https://github.com/samsaffron/term-llm/tree/main/examples/systemd-serve-web).
It includes an interactive installer with optional reverse connection and
startup registration, including `TERM_LLM_HUB_REGISTRATION_TOKEN`, plus a daily
check that restarts the node only after its installed binary changes.

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

The hub defaults to bearer auth: `--auth bearer` protects the dashboard, Hub APIs, and node proxy with a single Hub token. (`/healthz` is intentionally public and returns only `{"status":"ok","role":"hub"}`.) Set the bearer explicitly with `--token` or `TERM_LLM_HUB_TOKEN` for stable deployments; otherwise the hub prints a generated token at startup. Treat this token as an operator/admin secret: a holder can add or test nodes pointing at any address the Hub can reach and can proxy through those nodes. `--auth passkey` replaces browser bearer copies with WebAuthn-backed, revocable, expiring in-memory sessions while leaving node, registration, reverse-WebSocket, and delegation credentials unchanged. `--auth none` is available for local development, but it is loopback-only because anyone who can reach an unauthenticated hub can reach every node it fronts. Reverse nodes authenticate their websocket with the node id plus the node's bearer token; the hub accepts that connection only for nodes configured with `connection: reverse`, and the node-side connector forwards only requests under its configured base path.

The backend transport never uses an environment proxy (`HTTP_PROXY` would see injected tokens). The hub still rejects obvious cross-site browser requests and requires JSON content types for mutating node-registry APIs as defense-in-depth around the simple bearer gate.

Routing is path-based (`/node/<id>/...`) in v1; the proxy target is resolved per request, so host-based routing can be layered on later without changing the proxy plumbing. Because path routing puts hub UI and proxied node UI on the same browser origin, Hub v1 treats every registered node/widget as fully trusted with the operator browser's Hub authority. HttpOnly/SameSite session cookies and passkeys do not isolate malicious JavaScript already executing on that origin. The node web UI namespaces localStorage by hub node id to avoid ordinary state collisions; this is not a security boundary. Untrusted remote nodes/widgets still need the future host-based/widget-grant isolation work before they should be opened through a shared hub origin.

Node scheduling and mTLS between hub and nodes remain out of scope for v1; passkey mode intentionally supports one logical Hub administrator rather than multiple users or roles.

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

## Hub-wide terminal attention

Nodes that advertise the `attention` capability publish complete paginated snapshots of durable running runs and terminal conversations that have not yet been visited. Hub collects these snapshots in the background over the same direct or reverse transport used for node requests and stores a private projection in `<data-dir>/hub/attention.db` (mode 0600). Collection is faster while cached work is active or ready to review, slower while idle, and uses bounded jitter/backoff after failures. The projection contains titles and lifecycle metadata only—never prompts, transcripts, response bodies, backend URLs, or node tokens.

The dashboard's **Ready to review** section combines unseen completed, failed, reviewable-cancelled, and orphan-recovery conversations from every node. `GET /api/attention` exposes the same privacy-safe global/per-node counts and a bounded newest-first inbox; its `limit` is 1–500 (default 200) and `has_more` reports truncation. Conversation links navigate through the normal node proxy. Clicking is not an acknowledgement: the proxied node web app clears the marker only after the route is selected, the page is visible, and transcript bodies are installed through the marker's final revision. Hub learns that authoritative change on a later collection pass.

Hub retains its last successful rows when a node is offline, times out, returns malformed pagination, or loses a previously advertised capability. It replaces one node's projection only after every `unseen` and `running` page has the same store instance and snapshot version. If a node database is reset, the new stable store instance ID prevents unrelated session IDs from being merged. Nodes that never supported the API report attention as unavailable rather than falsely reporting zero.

The Hub dashboard's solid green node indicator means at least one child conversation is running or terminal-unseen. The web agents sidebar reserves its solid green dot for terminal-unseen work on other agents that needs attention; running-only agents stay quiet there. Hub aggregate indicators never pulse, while conversation rows retain the pulse (running) versus solid (ready to review) distinction.

Terminal attention currently follows the Hub's existing single-logical-operator model. Every Hub passkey is a credential for that same operator, not a separate principal, and all clients share each node's seen watermark.
