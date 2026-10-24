# Static Hub routing (operator proposal; not deployed by this change)

Build with `make frontend && node demo/build.mjs`. The document root is the
contents of `demo/dist/`, **not** the repository or docs site. JSON files are
build outputs; nginx only reads files. There is no proxy, database, serverless
function, agent, inference, or JavaScript requirement for the Hub GET endpoints.

## Exact nginx changes

`nginx-locations.conf` is the complete shared location snippet, also used verbatim
by the local integration tests. For the demo HTTPS server block:

1. Keep `root /var/www/term-llm-demo/current;`, `index index.html;`, TLS/listeners,
   MIME types, logging, compression, and existing security headers unchanged.
   Do **not** touch the docs-owned `/var/www/term-llm` root.
2. Replace the existing server-level method guard and these existing locations:
   `location ~ /\.`, `/demo-api`, `~ \.mjs$`, `/assets/`, and `/` with:

   ```nginx
   include /etc/nginx/snippets/term-llm-demo-locations.conf;
   ```

   That file must contain exactly the tracked `demo/nginx-locations.conf`.
   Do not append duplicate locations. The old `/models/` location is unused by
   this demo and need not be changed for compatibility. Its presence does not
   cause a model download; the demo does not reference it.
3. The separate HTTP-to-HTTPS redirect and ACME locations remain unchanged.

These are proposed operator actions only: this PR does not install the snippet,
change nginx, reload, upload, deploy, change DNS/TLS, or publish anything. Asset
publication **alone** will not enable extensionless GETs or nested reloads under
an older configuration using only `try_files $uri $uri/ =404`.

The snippet explicitly maps each extensionless endpoint to its `.json` file
with `application/json`. Exact locations also accept query strings (such as
`/api/attention?limit=30`). Only published chat routes fall back to `index.html`;
missing API/asset/node routes remain 404. All HTTP methods except GET/HEAD return
405, including mutations to known GET routes. The browser-only `/demo-api` stays
404 on the network. No successful server-side write is simulated.

## Endpoint and navigation manifest

| HTTP GET path | Static file / result |
| --- | --- |
| `/healthz` | `healthz.json`: `status: ok`, `role: hub` |
| `/api/nodes` | `api/nodes.json`: one fixed `config`-source DEMO node |
| `/api/attention` | `api/attention.json`: zero counts, empty panels |
| `/api/delegations` | `api/delegations.json`: empty delegations |
| `/api/registration-info` | `api/registration-info.json`: disabled, no node persistence |
| `/` | `index.html`: existing interactive demo, plus `data-hub-config` discovery |
| `/node/demo/` | same shell, welcome seed selected |
| `/node/demo/?new=1` | same shell, fresh browser-local conversation |
| `/node/demo/chat/1`, `/node/demo/chat/demo-1` | same shell, welcome/device-tool seed |
| `/node/demo/chat/2`, `/node/demo/chat/demo-2` | same shell, static Hub tour seed |

The IDs are `demo`, `demo-1`, and `demo-2`. Session numbers, IDs, titles, timestamps,
and message counts are shared with the in-browser API seed data. The displayed
node URL is `https://demo.term-llm.com` with base path `/node/demo`; it points to
this same static site, not a private/nonexistent backend. Node `reachable`/`ok`
means this scripted static preview is available, **not** a live agent probe.
No attention capability or delegations are advertised. All navigation and asset
URLs stay on the demo origin. Root-relative shell assets and module imports
survive direct nested loads and reloads.

## Native contract and read-only behavior

The ordinary Hub protocol uses `data-hub-config` (`page: dashboard`,
`authMode: none`, `passkeyAuth: false`, `canAddNodes: false`), the core `config`
resolver source for fixed nodes, and disabled registration. There is no general
Hub `readOnly` flag to invent here. These values suppress add/remove/sign-in
mutations in compatible native clients. A single node also has no usable reorder
action; empty attention has nothing to clear/mark seen. This does not alter normal
Hub behavior or claim any shared persistence. Removing a connection from the
phone itself is a local native action, not a remote Hub mutation.

Initial native loading uses root discovery, health role detection and the three
panel GETs above. Management can read registration info. Passkey-only security
GETs (`/api/auth/session`, `/api/auth/credentials`) are intentionally not supplied:
this site has no identity or credentials, and `passkeyAuth: false` disables that
flow. Unsupported operations return 404/405 rather than fake success.

Native navigation accepts `/node/<node-id>/` with optional `?new=1` and
`/node/<node-id>/chat/<number-or-id>` with no query or fragment. The generated
paths follow that existing grammar. Browser-local bootstrap resolves the path
into the stock frontend's ephemeral active-session preference; it does not add a
second protocol. Browser chat and preferences reset on document reload; metadata
never changes. Explicit native device actions still can persist on the device.

No private app code is included or modified. No private app compatibility change
is currently required by this contract. Full on-device acceptance remains
necessary; contract and Chromium checks are not iOS proof.

## Reproduce locally

Requires Node, nginx on PATH, and the normal frontend build dependencies:

```sh
make frontend && node demo/build.mjs
node --test demo/mock-api.test.mjs demo/hub.test.mjs demo/static.test.mjs
CHROMIUM_PATH=/usr/bin/chromium node --test demo/browser.test.mjs
```

`static.test.mjs` starts an owned ephemeral nginx bound to loopback on an
unprivileged random port, with the exact tracked location snippet. Temporary
nginx files live in ignored `tmp/` and are removed at shutdown. It checks real
HTTP status/content type/JSON, discovery, navigation/asset consistency, negative
routes and rejected writes. It is **not** a fixture API server. `hub.test.mjs`
checks seed IDs/numbers/titles/counts and route selectors. The browser test uses
the existing frontend Playwright dependency and system Chromium: root and nested
chat, fresh chat, scripted streaming, reload resets, and the real MCP UI switch
and inline result flow with an explicitly synthetic page tool. Unit tests cover
the sample-playlist tool, errors, cancellation and timeouts without calling a
real device.

For manual HTTP/browser inspection, `node demo/static-server.mjs` prints its
loopback origin; stop with Ctrl-C. It is not a permanent service.

**iOS acceptance is BLOCKED without an Apple environment:** add
`https://demo.term-llm.com/` as a Hub, tap the DEMO node and both sessions, send a
message, reload, inspect management/security controls, and run the native device
tool walkthrough on an authorized device. The public URL will only provide this
contract after a separately authorized static deployment plus routing change.
Do not interpret a Chromium WebMCP double as native device execution or retry
Apple CI billing failures as part of this work.

## Backward-compatibility acceptance matrix

| Surface | Evidence / limitation |
| --- | --- |
| Existing `/` chat and `/?new=1` | Chromium on real static nginx: welcome, help, fresh chat, scripted streaming; root remains chat, not a Hub dashboard |
| Original boot hints | Legacy-entry browser probe verifies `?session=demo-1`, `/demo-api` prefix and shell authorization; current frontend additionally receives an ephemeral active-session preference |
| Existing device-tool walkthrough | Real MCP UI switch plus synthetic ping/sample-playlist tools pass in Chromium; existing unit tests cover results/failures/timeouts/cancellation; real device execution is BLOCKED |
| New Hub connection | HTTP JSON/discovery and route/seed consistency pass; actual iOS add/tap/chat is BLOCKED |
| Existing native Try demo (saved Direct Node) | Source inspection: still opens the same root chat and retains its explicit saved kind; actual iOS verification is BLOCKED |
| Native device-tool context | Existing root URL, prefix, opt-in and WebMCP protocol remain unchanged; no new native bridge/context is injected; actual native permission/context checks are BLOCKED |
| Saved direct-demo voice discovery | Existing demo health was HTTP 404, so voice discovery was already unsupported. New `role: hub` is explicitly rejected by a direct-only discovery flow. This changes the discovery error, not a demonstrated working voice feature; needs native/parent compatibility sign-off, not a claim of passing voice acceptance |
| Legacy role discovery with no saved kind | A health probe now identifies a Hub. This is intentional additive discovery but can change a legacy client's routing choice; root still serves chat. Native migration/voice behavior is not verified here |
| Normal Hubs | No core server, standard protocol, frontend or private app behavior is changed; only `demo/` authoring/build/tests/routing proposal are touched |

The health endpoint cannot simultaneously declare both `hub` and `node` to the
same plain static GET. If preserving a direct-only client's successful role
classification is required (beyond preserving this demo's already unsupported
voice behavior), resolve that contract with the client owner **before release**;
do not invent header-specific APIs, fake voice success, or silently change a
saved connection kind. This proposal has not been deployed.
