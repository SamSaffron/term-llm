---
title: "Discourse Vibe (dv) + Hub"
weight: 17
description: "Connect dv (Discourse Vibe) dev containers to a term-llm Hub as reverse nodes: the reverse-node contract term-llm exposes, the dv-hooks reference implementation, and a troubleshooting guide for the common 'waiting for reverse connection' traps."
kicker: "Deploy agents"
---

[dv (Discourse Vibe)](https://github.com/discourse/dv) manages local Discourse dev containers. Each `dv` container can run a term-llm agent inside it and surface that agent in your [term-llm Hub]({{< relref "hub" >}}) — so you get one dashboard across every dv agent you operate, with one-click open and cross-node delegation.

## Mental model

There are **two processes** and they talk over a websocket:

- **The Hub** runs on your **host** (your Mac/Linux machine): `term-llm serve hub`. It owns the dashboard and a single registry of nodes.
- **Each dv container** runs a **term-llm web node** inside it that dials *out* to the Hub and keeps a websocket open. This is a [**reverse node**]({{< relref "hub" >}}): the Hub never has to reach into the container; the container reaches out.

**You only ever run `serve hub` yourself.** It is a dashboard and proxy — it holds no agent. The agent itself (web UI, HTTP API, tools, the LLM loop) is `serve web`, and a copy runs *inside each container*, launched automatically when the container starts. You never type `serve web` by hand for dv; the container's own startup service runs it for you.

Reverse is the right mode for dv because the agent lives inside Docker, behind Docker's network. The container can always reach the host; the host cannot reliably reach a port inside the container. So the container makes the connection.

## Prerequisites

- `dv` installed and at least one container created (`dv ls` shows it).
- `term-llm` installed on the host.
- Docker Desktop or OrbStack (provides the `host.docker.internal` hostname the container uses to reach the host).

## Step 1: Start the Hub on the host

Run the Hub. The flags that matter for dv:

```bash
term-llm serve hub \
  --host 0.0.0.0 \
  --port 8090 \
  --auth bearer \
  --token hubdevtoken \
  --registration-token devreg
```

- `--host 0.0.0.0` — **critical.** A dv container reaches the host via `host.docker.internal`, which resolves to the host's gateway address, **not** `127.0.0.1`. A Hub bound to loopback (the default) is invisible to the container. `0.0.0.0` makes it reachable.
- `--token hubdevtoken` — the **Hub bearer token**. You type this once in the browser to log into the dashboard. Pick your own value.
- `--registration-token devreg` — a **separate** token that lets a container self-register itself as a node on startup. The container must present the same value (see Step 2). Pick your own value.

**Do not run the bare `term-llm serve hub` with no flags.** It binds `127.0.0.1` only and uses a different auto-generated token. dv containers can't reach it, and you'll end up staring at a Hub that never sees your agent. See [Troubleshooting](#troubleshooting).

The Hub prints where it's listening and confirms `registration: enabled`. Leave it running.

## Step 2: The reverse-node contract

term-llm already ships everything a container needs to become a reverse node. There is nothing dv-specific in term-llm itself — whatever manages the container (dv, a hand-written script, a Compose entry) just has to satisfy one contract: **run `term-llm serve web` inside the container with the reverse + register flags.**

```bash
term-llm serve web \
  --token "<node bearer token>" \                 # this node's own web token
  --hub-url http://host.docker.internal:8090/ \    # the Hub, as seen from the container
  --hub-node-id   my-agent \                       # unique id on the Hub
  --hub-node-name my-agent \                       # display name
  --hub-connect reverse \                          # dial out instead of being dialed
  --hub-register \                                 # self-register on startup
  --hub-registration-token devreg                  # MUST equal the Hub --registration-token
```

| Flag | Purpose | Constraint |
| --- | --- | --- |
| `--hub-url` | Where the container dials. Use `host.docker.internal`, **not** `localhost` (inside the container `localhost` is the container itself). | Host + port of your Hub |
| `--hub-registration-token` | Presented to `/api/register-node` so the node can self-register. Defaults to `$TERM_LLM_HUB_REGISTRATION_TOKEN`. | **Yes** — equals the Hub's `--registration-token` |
| `--token` | This node's own bearer token. The Hub stores it on registration, then uses it to authenticate the reverse websocket and to inject auth when you open the node. | Stable per node |
| `--hub-node-id` | Unique node id; drives the card and the `/node/<id>/` proxy path. | Unique on the Hub |

Two rules term-llm enforces: `--hub-register` requires `--hub-connect reverse`, and it needs `--hub-url`, `--hub-node-id`, `--hub-registration-token`, and a bearer `--token` all set.

**Deregister on removal.** `--hub-register` does a `POST <hub-url>/api/register-node` at startup. When the container is destroyed, delete the node so its card disappears from the Hub:

```text
DELETE <hub-url>/api/register-node/<id>     # auth: the registration token
```

(Only reverse-registered nodes can be removed this way; config/contain nodes are managed by their own source.)

## Step 3: Wire a dv container (reference implementation)

You don't hand-write the above per container. The reference implementation is **[SamSaffron/dotfiles `dv-hooks`](https://github.com/SamSaffron/dotfiles/tree/master/dv-hooks)** — host-side scripts that hang off dv's `postCreate`/`postRemove` hooks and satisfy the contract for you. On `dv new` they:

1. copy a hub-capable `term-llm` binary and your `~/.config/term-llm` into the container,
2. install a small in-container [runit](http://smarden.org/runit/) service that runs the `serve web … --hub-connect reverse --hub-register` command from Step 2, and
3. on `dv remove`, call the `DELETE /api/register-node/<id>` deregister endpoint.

To use them, set your Hub URL and registration token once in the hooks' `.env` (copy `.env.example`), and register the hooks in `~/.config/dv/config.json`. The per-container values map straight onto the contract:

| dv-hooks env var | Contract flag |
| --- | --- |
| `TERM_LLM_HUB_URL` | `--hub-url` |
| `HUB_REGISTRATION_TOKEN` | `--hub-registration-token` (= Hub `--registration-token`) |
| `NODE_TOKEN` | `--token` |
| node id / name (default: container name) | `--hub-node-id` / `--hub-node-name` |

See the [dv-hooks README](https://github.com/SamSaffron/dotfiles/tree/master/dv-hooks) for the exact `config.json` block and token-lookup order. The important thing from term-llm's side is unchanged: the container ends up running the Step 2 command, and `--hub-registration-token` equals the Hub's `--registration-token`.

## Step 4: Connect and verify

Start (or restart) the container so it runs the Step 2 command. With the dv-hooks reference implementation that's the in-container runit service:

```bash
# inside the container, if using the dv-hooks runit service
sv restart term-llm-hub
```

Then open the dashboard on the host and log in with the **Hub bearer token** (`hubdevtoken`):

```text
http://localhost:8090
```

The node card flips from **"waiting for reverse connection"** to **connected** within a couple of seconds. You can confirm from the host without a browser:

```bash
curl -s http://127.0.0.1:8090/api/nodes -H "Authorization: Bearer hubdevtoken" \
  | python3 -c 'import sys,json;[print(n["id"],n["status"]["state"]) for n in json.load(sys.stdin)["nodes"]]'
# my-agent connected
```

## Troubleshooting

Almost every first-time problem shows up as the same card: **status `disconnected`, "waiting for reverse connection"**, with the error *"Reverse node is waiting for its outbound websocket connection."* That message means exactly one thing: **the Hub has the node registered, but no reverse websocket has connected to *this* Hub.** Work through these in order.

### 1. You're running two Hubs (the most common trap)

If you ever ran the bare `term-llm serve hub` (no flags) and also the configured one, you can end up with **two Hub processes on port 8090** — one bound to IPv4 `127.0.0.1`, the other to the wildcard. (That split is *why* they coexist instead of the second one failing with "address already in use".) Both processes read the same on-disk node store (`~/.local/share/term-llm/hub/nodes.json`), so your node shows up on **both** dashboards — but the live reverse websocket exists in only **one** of the processes.

So the trap is: the container is genuinely connected, but to the *other* Hub. The one your browser is pointed at sees the node in the shared store yet has no live socket for it, so it shows "waiting" forever. In our debugging session the bare Hub held IPv4 `127.0.0.1:8090` (what the browser hit) while the configured `--host 0.0.0.0` Hub held the wildcard and the container's actual connection.

**Check** how many Hubs are listening:

```bash
lsof -nP -iTCP:8090 -sTCP:LISTEN
```

If you see **two** `term-llm` rows, that's the bug. Stop the bare one (find it in its terminal and Ctrl-C, or `kill <pid>`). With the squatter gone, the configured Hub's wildcard socket serves the `127.0.0.1` path too, and your browser and container land on the same Hub. Confirm there's exactly **one** row, then refresh.

To avoid this entirely, never start `term-llm serve hub` without flags. A tiny shell function helps (fish shown; adapt for bash/zsh):

```fish
function hub
    term-llm serve hub --host 0.0.0.0 --port 8090 --auth bearer \
        --token hubdevtoken --registration-token devreg
end
```

### 2. Hub bound to loopback

If the Hub was started **without** `--host 0.0.0.0`, it listens only on `127.0.0.1` and the container cannot reach it at all (`host.docker.internal` → host gateway, not loopback). Restart the Hub with `--host 0.0.0.0`. (`address already in use`? A Hub is already running on 8090 — you probably don't need to start another; check with `lsof` as above.)

### 3. Registration token mismatch

If the container's `--hub-registration-token` ≠ the Hub's `--registration-token`, self-registration fails and the node never appears (or an old registration with a stale token rejects the websocket). With the dv-hooks reference implementation this token is `HUB_REGISTRATION_TOKEN` in the hooks' `.env`. Make the two equal and restart the connector (`sv restart term-llm-hub`).

### 4. `host.docker.internal` doesn't resolve

On Docker Desktop (Mac/Windows) it's built in. On plain Linux Docker you may need to add it to the container:

```yaml
# compose
extra_hosts:
  - "host.docker.internal:host-gateway"
```

### 5. Probe from inside the container

The connector retries every 2s and logs `hub reverse connect: …` on failure, but the dv-hooks runit service does **not** persist that output (no `log/` pipeline, no redirect), so you usually can't just tail it. Use two log-free checks instead.

Confirm the service is up:

```bash
sv status term-llm-hub      # inside the container
```

Then probe the Hub's public health endpoint **from inside the container** (no token needed):

```bash
curl -sS -o /dev/null -w '%{http_code}\n' http://host.docker.internal:8090/healthz
```

- `200` → the Hub is reachable, so the fault is auth: a token or registration-token mismatch (cases 1 or 3).
- connection refused / timeout → the container can't reach the Hub: wrong URL, a loopback-only bind, or `host.docker.internal` not resolving (cases 2 or 4).

To actually watch the `hub reverse connect:` retry lines (a successful connect logs `node "<id>" connected to <hub-url>`), stop the service and run its `serve web … --hub-connect reverse` command from `/etc/service/term-llm-hub/run` by hand in the foreground:

```bash
sv stop term-llm-hub
```

## Related pages

- [Hub](/guides/hub/): full Hub reference — nodes, the proxy, delegation, and security posture
- [Agent Containers](/guides/agent-containers/): term-llm's own `contain` workspaces, which auto-register with the Hub the same way
- [SamSaffron/dotfiles `dv-hooks`](https://github.com/SamSaffron/dotfiles/tree/master/dv-hooks): the reference implementation that satisfies the reverse-node contract for dv containers
