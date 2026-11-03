# Multi-user attribution in the web UI (trusted-proxy identity), phase one

Status: proposed

Note: implemented in the working tree, 2026-11-03; not yet committed or deployed.
All three implementation stages are complete. Live proxy-deployment validation
remains outstanding (the documentation explicitly labels its proxy examples as
deployment templates). Implementation notes and deviations are below.
The UI direction was picked from three
rendered mocks: transcript bylines from mock A plus the sidebar from mock C. Revised the
same day with Sam's answers:
- a request without the header stays anonymous, as today
- linked devices are attributed to the person who approved their pairing
- the shared secret, Hub identity and other APIs are left for later

Line numbers are as of `eea254c1`.

## Problem

People already share one `term-llm serve web` instance, by sharing its bearer token or by
sitting behind a single sign-on proxy, but the UI treats them as one person. Every human
message is an anonymous right-aligned bubble and the sidebar cannot say whose chats are
whose. When three people drive one session you cannot tell who asked for the revert and who
said "don't restart web-03".

## Decisions

1. **Layered on.** `--identity-header NAME` tells the server which request header to read.
   A request that carries a valid value is attributed to that person. A request without it
   is anonymous and behaves exactly as today. Without the flag, nothing is read or stored.
2. **Linked devices.** When the flag is set, a request from a linked device is attributed to
   the person who approved that device's pairing. Devices paired before the feature, or
   approved by an anonymous request, stay anonymous until they are paired again.
3. **Display only.** The author is stored beside a message, never inside its content, and
   never reaches a provider. All humans remain one "user" to the model.
4. **UI.** A byline above each human bubble, participant avatars on sidebar rows, an
   "Everyone / Started by me" filter, and a signed-in identity card at the bottom of the
   sidebar.
5. **Minimal.**
   - No permissions, no new authentication, and no change to other APIs.
   - The shared secret and Hub identity come later.
   - Setup needs a proxy and is fiddly, so documentation is part of phase one (see
     [Documentation](#documentation)).

## User experience

### Transcript (mock A)

```
                                         Priya Natarajan (PN)
                  ╭──────────────────────────────────────────╮
                  │ That matches Grafana: worker RSS jumped… │
                  ╰──────────────────────────────────────────╯
                                                     11m  ⧉
```

- Each human turn keeps today's right-aligned bubble. Above it sits a right-aligned byline:
  display name plus a 20px avatar (initials on a colour derived from the ID). The viewer's
  own turns say "You". A message sent from a linked device shows the person who approved
  that device.
- Back-to-back user rows by the same author share one byline. A byline appears again
  after any non-user row (assistant text, tool group, notice), after a transcript
  virtualization gap, and whenever the author changes.
- Unattributed user rows show no byline. These are history from before the feature,
  anonymous requests, CLI/TUI turns without `--user` / `/user`, jobs and external API callers. The one exception: when
  the row directly follows an attributed user row, it shows a muted "Unknown" so it is not
  read as that person's next message.
- The byline's `title` is `Priya Natarajan <priya@example.com>` (email only when the proxy
  provides one). The meta row under the bubble (time, copy, interrupt badge) is unchanged.
- Live updates: a steering bubble projected from `response.steering` shows its byline at
  once, and the sender's optimistic bubble shows "You" before the server echoes it.
- Pending steering rows above the composer show the author's avatar and first name
  ("Priya: also check web-05 (pending)").
- Assistant, tool and notice rows do not change, and the agent gets no name row.

### Sidebar (mock C)

```
 [ Everyone ][ Started by me ]
 Uploads 502 after 14:10 deploy
 8 messages · 1m ago               (SS)(PN)(DA)
 ─────────────────────────────────────────────
 (SS) Sam Saffron
      sam@example.com · via proxy
```

- **Participants:** the distinct authors of a session's user messages, in first-message
  order. A row shows up to three 18px avatars plus "+N" on its meta line. The tooltip and
  the row's accessible name list them: "Sam Saffron (you), Priya Natarajan, Diego Alvarez".
- **Filter:** "Everyone / Started by me", placed under New chat. It applies to the flat
  list, project groups, pinned sessions and search results, and is remembered per
  browser. Empty state: "No chats started by you."
- **Started by:** the author of the session's first user message. For a branch, it is the
  person who created the branch. An anonymous first user row keeps the starter unknown;
  undoing that row permits a new attributed first row to establish the starter.
  Transcript replacement and compaction never infer a starter from a later row.
- **Identity card:** at the bottom of the sidebar, below the Recent/Projects switch when
  projects are enabled. It shows avatar, display name, email if provided, and a muted
  "via proxy", or "via linked device" on a linked device. There is no sign-out, because
  sign-in lives at the proxy.
- The filter and the card appear only when the viewer is identified.
- **Mobile:** the same row avatars, with the card at the bottom of the drawer.

### Anonymous viewers and servers without the flag

- **Flag not set:** nothing new is read or stored. The UI shows only authors that are
  already stored, and a server that never set the flag has none, so it looks exactly as
  today.
- **Flag set, request without the header:** everything works as today, and the viewer's
  messages are unattributed. The viewer still sees other people's bylines and participant
  avatars, but gets no identity card and no filter.

## Configuration

### Flags (`term-llm serve web`)

| Flag | Meaning |
|---|---|
| `--identity-header NAME` | Read the user's stable ID from this header. Enables attribution. |
| `--identity-name-header NAME` | Optional display-name header. Defaults to the ID. |
| `--identity-email-header NAME` | Optional email header. |

These are flags only, like `--auth` and `--token`; there are no `config.yaml` keys in phase
one. Options go through `Options` → `serveServerConfig`
(`internal/app/serve/serve_server_config.go:15`), and binding stays in `cmd/serve.go`. At
startup the server logs `identity: reading Remote-User (name: Remote-Name, email:
Remote-Email)`, so a misconfigured flag is visible.

### Identity resolution (per request)

1. **Linked device:** a request admitted as a linked device (`linkeddevices.DeviceFromRequest`)
   is attributed to the approver recorded on the device's grant (see
   [Recording who approved a pairing](#recording-who-approved-a-pairing)). Identity headers
   on device requests are ignored. A grant with no approver makes the request anonymous.
2. **Header:** otherwise, if the configured header has a valid value, the request is
   attributed to that person.
3. **Anonymous:** otherwise the request is anonymous, exactly as today. The server never
   rejects a request because the header is missing or invalid.

Validation:
- Trim the value. Decode RFC 2047 encoded-words in the name and email (Go
  `mime.WordDecoder`); Tailscale uses them for non-ASCII names.
- Require valid UTF-8 with no control characters. Limits: ID ≤ 256 bytes, name ≤ 128 runes,
  email ≤ 256 bytes.
- An invalid ID counts as missing. An invalid name or email is ignored.
- IDs are compared exactly (case-sensitive), so the proxy must send a stable value.

Where it runs:
- Inside the existing `s.auth` wrapper (`internal/app/serve/serve_handlers.go:2745`),
  after authentication succeeds, including the no-auth path, where `s.auth` currently
  returns `next` unchanged.
- It works the same with bearer, passkey and no-auth.
- Route registrations and `testdata/routes.golden` do not change.
- The identity is stored on the request context and read through one helper in a new
  `serve_identity.go`.

### Recording who approved a pairing

- **The grant:** `pairing.Grant` (`internal/pairing/grants.go:21`) gains
  `ApprovedBy *Approver` with `` `json:"approved_by,omitempty"` ``, where `Approver` is
  `{id, name, email}`. Grants live in a JSON file that is decoded without
  `DisallowUnknownFields` (`grants.go:74`), so existing grants load unchanged and have no
  approver.
- **Approval:**
  - The approve endpoint (`internal/linkeddevices/api.go:98-106`) calls `Runtime.Approve`
    (`signal.go:102`) with the request context.
  - That route is already wrapped in `s.auth` (`internal/app/serve/serve_http.go:113`), so
    the approver's identity is on the context.
  - `linkeddevices` cannot import the serve package. Instead, the serve layer passes a
    small callback in the runtime options that reads the identity from a context.
  - `Approve` calls it and passes the result to `host.CompletePairing` (`signal.go:130`).
    That function builds and stores the grant (`internal/pairing/host.go:284`, `:310-311`),
    so the grant is written once, with its approver.
- **Device requests:** `DevicePrincipal` (`internal/linkeddevices/ingress.go:15`) gains
  `ApprovedBy`. It is copied from the grant when the peer session is authorized
  (`signal.go:402-411`) and is constant for the session.
- **Mixed binaries:** an older binary ignores the field. It also drops the field when it
  rewrites the grants file, so those devices fall back to anonymous until they are paired
  again.

### Trust

The server believes the header. Anyone who reaches the node without going through the
proxy can send any name. That includes requests through the Hub, because
`hubRewriteProxyRequest` (`internal/app/serve/serve_hub_proxy.go:242`) forwards client
headers other than credentials and `X-Forwarded-*`. For a display feature this is
acceptable, and the docs say it plainly. A shared-secret header and identity verified by the
Hub are later work.

## Data model

### Message author

- `core.Message.AuthorID string` with `` `json:",omitempty"` ``
  (`internal/llm/core/contracts.go:342`), next to `ClientMessageID`. Its doc comment: the
  stable ID of the person who sent a user message, directly or through a linked device they
  approved, used for persistence and UI only, and ignored by providers. Because it is part
  of the message, it travels everywhere the message already goes:
  - the engine steering queue (`engine.QueuedSteering.Message`, `engine.go:291`)
  - durable pending steering (`session_pending_steering.message` is core.Message JSON)
  - rush payloads (`session_rush_entries.payload`)
  - runtime history

  None of these need a new column.
- `session.Message.AuthorID` with `` `json:"author_id,omitempty"` ``. `NewMessage`
  (`internal/session/types.go:267`) copies it from core to session, and `ToLLMMessage`
  (`types.go:371`) copies it back. That way rows rebuilt from in-memory history keep their
  author: snapshot replacement (`serve_runtime.go:1262`), compaction tails and the redo
  stack (`session_redo.suffix` is JSON).
- New column `messages.author_id TEXT NOT NULL DEFAULT ''`.
  - Every insert writes it: `sqlite_messages.go:100`, `:683`, `:964`, `:1444` (redo
    restore), `sqlite_message_insert.go:11` and `branch.go:263` (the copy `SELECT` includes
    `author_id`).
  - The path-note insert (`branch.go:292`) writes `''`.
  - Every `SELECT` that scans message rows reads it (start at `sqlite_messages.go:862`).
- Only user-role rows ever carry an author.

### Session starter

- `sessions.created_by TEXT NOT NULL DEFAULT ''`, plus `Session.CreatedBy` and
  `SessionSummary.CreatedBy`.
- The store sets it in the transaction that writes a session's first user-role row, when
  that row has an author, with no other user row present and excluding compaction
  tails. This does not depend on the `user_turns` counter or increment ordering.
  Existing sessions stay `''` (unknown); removing an anonymous first turn by undo
  allows the next first user row to establish a starter.
- Raw store JSON (`Session`, `SessionSummary`, `SearchResult`, including CLI JSON
  and store-based exports) uses `created_by_id`; web author objects use `created_by`.
  Frontend conversion ignores raw IDs and malformed author objects.
- Branches: `CreateBranchOptions` (`internal/session/store.go:1070`) gains `CreatedBy`,
  set to the requester. Copied rows keep their original authors.

### Author directory

- New table: `authors(id TEXT PRIMARY KEY, display_name TEXT NOT NULL, email TEXT NOT NULL
  DEFAULT '', updated_at TIMESTAMP NOT NULL)`.
- **Header stamps** upsert it: the write happens when the server stamps a message, not on
  every request, and is skipped when the cached values are unchanged. The latest name wins
  everywhere, so a rename at the identity provider also shows on old messages.
- **Linked-device stamps** only insert a missing row. The approver's details are a snapshot
  from pairing time and must not overwrite a newer name.
- It is read through a small serve-side cache (new `serve_authors.go`) that every
  projection uses. Store access goes through an `AuthorStore` interface with an
  `AsAuthorStore` helper, following `AsPendingSteeringStore`.

### Participants

- Derived, not stored: the distinct `author_id` values of a session's user rows, ordered by
  first sequence.
- One batched query per page of sessions:
  `WHERE session_id IN (…) AND author_id <> '' GROUP BY session_id, author_id`.
- Backed by a partial index:
  `idx_messages_session_author ON messages(session_id, author_id) WHERE author_id <> ''`.
  On a server that never sets the flag the index stays empty, so the query is free.
- Because there is no participants table, undo, compaction and branching have nothing to
  keep in sync.

### Migration

- One new session migration: the next version, v69 today
  (`internal/session/sqlite_migrations.go:14`). It adds the two columns with
  `sqliteutil.ColumnExists` guards, the `authors` table and the partial index, following
  `internal/sqliteutil/README.md`. There is no backfill.
- Tests: fresh versus migrated `SchemaSignature`, data preservation, rollback and retry.
- Mixed binaries: as with v64 and v67, the previous binary rejects the new marker for
  read-write use; read-only, it ignores the new columns. Add a rollout note to the README.

## Where authors are stamped

Stamping means setting `AuthorID` on the `core.Message` from the request's identity (the
header, or a linked device's approver) before the message enters the runtime. Anonymous
requests leave it empty. A helper in `serve_identity.go` does this, so each call site
changes by about one line.

| Path | Where | Rule |
|---|---|---|
| `POST /v1/responses` | `serve_handlers_responses.go`, after `validateResponsesAdmissionRequest` (`:178`) | After `prepareResponseClientMessageIDs` (`:73`) normalizes IDs, stamp every user-role input that has a `client_message_id`: the new message of a first-party send, or each message of a batch. Replayed history has no IDs and is not stamped. |
| Steering / interrupt | `handleSessionInterrupt`, `serve_handlers.go:2104-2118` | Stamp `msg` once it is built. |
| Skill invocation | `serve_skills_admission.go:50-51` | Stamp `userMessage`. |
| Branch | `/v1/responses` with `branch=true` | The new message is stamped as above; the fork's `created_by` is the requester. |

These keep working the old way, with no attribution:
- `/v1/chat/completions` and `/v1/messages`
- `/v1/responses` without `client_message_id`
- live-voice delegation (`serve_live.go:1743`, `:1802`)
- jobs and job notifications
- Telegram and CLI/TUI turns without `--user` / `/user`

## Keeping identity away from the model

This holds by construction. Providers build requests from `Role` and `Parts` and already
ignore the persistence-only fields `ClientMessageID`, `ResponseID` and `DisplayText`;
`AuthorID` joins that set. It is never written into `Parts`, `TextContent`, `DisplayText` or
any prompt text.

- No author or participant text goes into anything the model reads: compaction, handover,
  side questions and fork context, branch path notes, title generation, memory mining, and
  the session directory tool. `serve_session_directory.go` builds its own entry struct;
  keep it free of `created_by` and participants.
- `agyTranscriptHash` (`internal/llm/providers/clibin/agy_bin.go:395`) hashes whole
  `core.Message` values to detect a diverged conversation. Clear `AuthorID` before hashing,
  so a dropped or added author never resets an agy-bin conversation.

Tests:
- **Canary test.** Use authors whose ID, name and email contain a unique canary string.
  Run a normal send, a steering injection, a compaction and a side question through
  `internal/llm/mockprovider`, and assert that no recorded provider request contains the
  canary.
- **Request-builder test.** For each provider adapter family, a request built from
  messages with `AuthorID` set must be byte-identical to one built without it.

## API changes

A new wire type, Go `webAuthor`, emitted to TypeScript by `make api-types`
(`internal/tsgen/generate.go:21`):

```ts
type Author = { id: string; name: string; email?: string };
```

Projections return whatever is stored; nothing is gated on the serve identity flag.
A database with no stored attribution emits no new fields; CLI-authored rows also
appear on the web even when proxy identity is disabled.

| Where | Change |
|---|---|
| `sessionMessageEntry` (`serve_handlers.go:956`) | `author?` on user rows, filled by `sessionMessageEntries` (`:1683`) with one directory lookup per response. Covers `/transcript`, `/transcript/bodies`, `/messages` and the selected-session sideload. |
| `webSessionEntry` (`serve_handlers.go:1205`) | `created_by?`, `participants?` (at most 5, in first-message order) and `participant_count?`. Filled for the list, the sidebar, search and the selected session. If absent, the value is unknown; it does not mean "nobody". |
| `GET /v1/sessions` (`:1453`), `/v1/sidebar` (`serve_projects.go:770`), `/v1/sessions/search` (`serve_session_search.go:12`) | A `created_by=me` filter, resolved to the viewer on the server; only `me` is accepted. `ListOptions`, `SidebarOptions` and `SearchOptions` gain `CreatedBy`. Returns 400 when the request is anonymous. Applied personal filters use `Cache-Control: private, no-store`. |
| `/v1/capabilities` (`serve_capabilities.go:15`) | `viewer?: Author`, present only when this request is identified. The viewer ID goes into a valid quoted ETag (`:41`); identified responses are `Cache-Control: private, no-store`. |
| `response.steering` (`serve_response_run_event_projection.go:290`) and `response.steering.queued` (`serve_runtime.go:1017`) | `author?`, taken from `Message.AuthorID`. |

`/v1/events` needs no change: other browsers refetch the transcript on
`session.transcript_changed` and get authors from the transcript API.

## Frontend changes

- **Types:**
  - `Author` in `domain/types.ts`
  - `Message.author?` (`types.ts:132`)
  - `Session.createdBy?`, `participants?` and `participantCount?` (`types.ts:275`)
  - `PendingSteering.author?` (`stores/store-types.ts:81`)
- **Conversion:** the user branch of the transcript converter
  (`domain/transcript.ts:574-607`) and `withSource` (`:188`) carry `author`.
  `sanitizeSession` (`:859`) maps the session fields using the existing conditional-spread
  pattern, so a snapshot without them does not wipe cached values.
- **Viewer:** `applyCapabilities` (`stores/app-store.ts:1352`) stores `viewer` in a signal.
  - The card, the filter and "You" need a viewer.
  - Bylines and participant avatars render whenever the data carries authors.
- **Optimistic sends:** when there is a viewer, these local user messages get
  `author = viewer`:
  - the optimistic send (`stores/run-engine.ts:560`)
  - restored intents (`:192`)
  - skill invocations (`stores/skill-store.ts:207`)
  - local steering entries
- **Live:** the `response.steering` projection (`domain/response.ts:736`) copies `author`.
  Queued receipts (`run-engine.ts:1446`) store it on the pending entry, and
  `SteeringQueuePreview` renders avatar and first name.
- **Byline:** a new `AuthorByline` component. `MessageRow` (`Transcript.tsx:1169`) renders
  it inside the user article, before `.message-body`.
  - The grouping rule lives in `createTranscriptIndexes` (`domain/transcript-indexes.ts:9`)
    as a per-row `bylineAuthor`, with unit tests.
  - `MessageRow` stays memoized on a stable prop.
- **Sidebar:**
  - a `Facepile` in `SessionRow`'s meta line (`Sidebar.tsx:381-391`) and in its
    accessible name
  - the filter under `.sidebar-actions` (`:1275`), stored with `store.storage` and passed
    to the session-store list, sidebar and search requests
  - the identity card after `.sidebar-view-footer` (`:1364`)
- **Avatars** (`domain/authors.ts`):
  - Initials are the first letters of the name's first and last words, or else the first
    two characters of the ID.
  - The colour is a stable hash of the ID into a fixed palette of eight muted OKLCH hues
    (rose, orange, gold, green, teal, blue, violet, plum; C 0.055, with green and teal at
    0.042 because they read stronger; L about 0.55). A person keeps the same colour in both
    themes, with white initials at 4.5:1 or better.
  - No network images.
- **Styles:** a new `styles/features/authors.css`, imported in `app.css` after
  `transcript.css` and before `integration/preact-overrides.css`.
- **Accessibility:**
  - Avatars are `aria-hidden`; the byline name is real text read before the message.
  - Sidebar rows include participant names in their accessible name.
  - The filter is a two-option toggle group with arrow-key support, like
    `SidebarViewSwitch`.
- Keep new code in new files. `Transcript.tsx` and `Sidebar.tsx` change only at call sites.

## Documentation

Setup needs a proxy and is easy to get subtly wrong, so phase one ships with docs. Add a
section "Several people on one server" to `docs-site/content/guides/web-ui-and-api.md`,
after "Optional passkey authentication", and run `make docs` so the CLI reference picks up
the flags. It must cover:

1. **What it does:** attribution in the web UI only. The model never sees it, and there
   are no permissions.
2. **The flags,** the startup log line, and the rule that a request without the header is
   anonymous.
3. **Quick start with Tailscale Serve,** the simplest safe setup. Serve removes
   client-supplied `Tailscale-User-*` headers and adds its own for tailnet users. Tagged
   devices and Funnel traffic arrive anonymous, and a `--no-auth` server must never be
   exposed through Funnel.
   ```sh
   term-llm serve web --no-auth \
     --identity-header Tailscale-User-Login --identity-name-header Tailscale-User-Name
   tailscale serve --https=443 http://127.0.0.1:8080
   ```
4. **Authelia with Caddy `forward_auth`:** `copy_headers Remote-User Remote-Name
   Remote-Email`, then `--identity-header Remote-User --identity-name-header Remote-Name
   --identity-email-header Remote-Email`. Caddy's docs do not promise that a client-supplied
   `Remote-User` is removed when the auth response lacks it, so the example strips it
   before auth.
5. **oauth2-proxy:**
   - With `--pass-user-headers`, it sends `X-Forwarded-User`,
     `X-Forwarded-Preferred-Username` and `X-Forwarded-Email`.
   - Behind nginx `auth_request` with `--set-xauthrequest`, use `auth_request_set` plus
     `proxy_set_header X-Auth-Request-User` and `X-Auth-Request-Email`. These overwrite
     client values.
6. **Cloudflare Access:** `Cf-Access-Authenticated-User-Email` as both ID and email. Safe
   only when the origin is reachable solely through Cloudflare (Tunnel). Cloudflare
   recommends validating `Cf-Access-Jwt-Assertion`, which term-llm does not do.
7. **Checking it works:**
   - Your name appears at the bottom of the sidebar.
   - `curl -s -H 'Remote-User: alice' http://127.0.0.1:8080/ui/v1/capabilities` shows a
     `viewer`.
8. **The trust warning:** whoever reaches the server without the proxy can claim any name,
   including through the Hub. The proxy must be the only path and must overwrite client
   copies of the header.
9. **Linked devices** post as the person who approved their pairing. Devices paired
   before the feature, or approved anonymously, stay anonymous until they are paired again.
10. **CLI/TUI attribution:** `ask --user NAME` and `chat --user NAME` attribute new
    human turns; terminal `/user NAME` switches subsequent turns and `/user off`
    clears attribution. Other APIs, jobs, generated continuations and voice stay
    unattributed.

Every example is run end to end before it is published. That includes checking that a
header the client sent is overwritten or dropped.

## Testing

Go (`make test PKG='./internal/app/serve/... ./internal/session ./internal/llm/... ./internal/linkeddevices/... ./internal/pairing'`):
- **Identity resolution:**
  - Without the flag, headers are ignored.
  - With the flag, a valid header is attributed, and a missing, empty or invalid one is
    anonymous with an unchanged response.
  - Encoded-word names are decoded, and name and email fall back correctly.
  - Bearer, passkey and no-auth all keep working.
- **Linked devices:**
  - An identified approval records the approver on the grant; an anonymous approval
    records none.
  - Grants with and without `approved_by` load and round-trip.
  - A device request is attributed to its approver and ignores identity headers.
  - A device without an approver is anonymous.
  - A device stamp never overwrites a newer name in the author directory.
- **Stamping:**
  - `/v1/responses` single and batch sends are stamped; replayed history is not.
  - Steering is stamped, including through durable pending steering and a restart.
  - Rush commits and skill invocations are stamped.
  - Anonymous sends stay unstamped.
- **Store:**
  - Every insert path persists `author_id`; undo/redo keeps it.
  - Branch copies keep authors and set the fork's `created_by`.
  - The `created_by` rule and the participants query.
  - Migration tests as listed above.
- **API:**
  - Fields appear exactly when authors are stored.
  - `created_by=me` works for identified requests and returns 400 for anonymous ones.
  - `viewer` is absent for anonymous requests, and the capabilities ETag differs per viewer.
- **Model isolation:** the canary and request-builder tests above.
- `testdata/routes.golden` and `testdata/hub_auth.golden` are unchanged.

Frontend (`npm --prefix frontend test`, `lint`, `typecheck`):
- conversion of author and session fields
- grouping rules: same author, author change, non-user row, gap, "Unknown"
- "You" for optimistic sends
- author from the steering event, and on pending rows
- facepile overflow and accessible names
- the filter request parameter and its persistence
- no card or filter without a viewer
- nothing new when no authors exist

E2E: extend `mockAPI` in `frontend/e2e/chat.spec.ts` with authors, participants and a
viewer, and cover bylines and the filter on the desktop and mobile projects.

Finish with `make check-changed ARGS=--all`, `make complexity`, `make plans-lint` and `make docs`.

Several touched Go files are recorded as oversize in `ledger/go-size-baseline.json`:
- `serve_handlers.go`
- `serve_handlers_responses.go`
- `serve_runtime.go`
- `sqlite_messages.go`
- `sqlite_schema.go`
- `sqlite_sessions.go`

New logic goes in new files (`serve_identity.go`, `serve_authors.go`,
`session/sqlite_authors.go`, …). Edits to the files above are limited to call sites and
column lists.

## Stages

Each stage leaves a server without the flag unchanged.

1. **Identity:** flags, header resolution, recording pairing approvers and resolving
   linked devices to them, `capabilities.viewer`, and the docs draft.
2. **Attribution:** migration, `AuthorID` plumbing, stamping, the author directory, the
   API fields and filter, and the model-isolation tests.
3. **UI:** bylines, live and pending authors, sidebar avatars, the filter, the identity
   card, e2e coverage and the final docs.

## Known limitations of phase one

- **Names are trusted as sent.** They are only as trustworthy as the path to the server;
  see [Trust](#trust).
- **No permissions.** Anyone can read, continue, rename, archive, delete or undo in any
  session, and can remove anyone's pending steering.
- **Per-user state is still shared:** pins, archive, unread and attention "seen" state,
  and web push (every subscribed device is notified of every completion).
- **No presence:** nobody can see who is viewing.
- **The model does not know who is talking.**

## Later

- A shared-secret header, so identity is trusted only when the proxy proves itself.
- Identity verified by the Hub and forwarded to nodes.
- Attribution for other APIs, live voice and jobs.
- Per-user pins, unread state and notifications.
- Permissions.
- Telling the model who is speaking.

## Implementation notes (2026-11-03)

- `pairing.Approver` already names the approval-policy interface; the persisted
  display snapshot is `pairing.Person` instead. The JSON contract is unchanged.
- Reusable identity/directory projections live in
  `internal/app/serve/authorship`, behind the request helpers in
  `serve_identity.go`. SQL author helpers live in `internal/session/authordb`. The whole search endpoint
  lives in `internal/app/serve/sessionsearch`, and the transcript prefix algorithm
  lives in `internal/session/transcriptprefix` (pure moves, no behavior change). These boundaries keep both large packages
  within their existing size allowances; no baseline was raised.
- The store sets the first-user starter through a transactional insert trigger.
  Branch creation overrides it with the requester after copying original rows.
- Model isolation additionally strips `AuthorID` at the engine and side-question
  provider boundaries. The side-question canary exposed a separate direct call.
  Bedrock delegates to the tested Anthropic request builder.
- The existing project sidebar emits session summaries, not `webSessionEntry`;
  the author projection preserves that summary wire shape and adds the same
  attribution fields. No route registrations or route goldens changed.
- Viewer metadata is not cached as authentication state. Session author fields
  are cached as display hints; the workspace-cache schema was bumped.
- Proxy templates were not run against real Tailscale/Authelia/oauth2-proxy/
  Cloudflare deployments in this environment. The guide requires an explicit
  forged-header overwrite test before deployment and does not claim validation.

- The author filter uses native radio controls, which supply browser-owned
  arrow-key navigation and roving focus. Avatar and byline views remain eager.
- Initial production entry measurement was 538.1 kB raw / 157.85 kB Go-level-6 gzip.
  The raw entry cap and historical payload baseline are unchanged; the active
  gzip test allowance increases by 300 bytes (157700 to 158000), with the
  measured feature cost documented beside the assertion.

- Once at least one author directory row exists, a successful participants query
  emits an explicit zero count when empty. A cached EXISTS bit is checked at startup
  and set on the first successful stamp; servers that never stored an author emit
  no new fields, preserving their flag-off wire projection. Absence remains unknown. The converter clears cached facepiles only on this
  known-zero marker, so undoing the last attributed row cannot leave stale
  avatars while older snapshots without these fields still preserve the cache.

- Full verification exposed an existing pairing-delivery lifecycle race: a
  peer-disconnect frame can clear an invitation while approval still awaits its
  delivery receipt. Explicit cancellation now settles retained deliveries even
  when the invitation is already absent, preventing a blocked approval and
  ensuring its captured approver grant is rolled back. A deterministic
  regression covers this ordering.

- The directory cache is bounded to 512 identities. Successful projections use
  one fresh batched directory read rather than a TTL, so renames from another
  writer appear immediately; cached names remain a display fallback on read
  failure. Linked-device viewer cards also prefer current directory metadata
  over the pairing-time name without overwriting the approver snapshot.

### Accepted review fixes and deviations (2026-11-03)

- **A:** Added production HTTP/auth-wrapper tests with file-backed SQLite and
  `internal/llm/mockprovider` for identified, anonymous, flag-off, external,
  batched and replayed-history sends; live interrupt consumption and rush commit;
  and main-context skill invocation. Assertions cover stored message/starter IDs,
  author-bearing queued/consumed steering events, API presence/absence and provider
  isolation. Restart coverage freezes an active runtime before consumption, queues
  through `handleSessionInterrupt`, closes/reopens SQLite and recreates the host.
  This exposed a replay bug: first-party follow-ups now restore authoritative
  pending authors by client ID, including anonymous originals, instead of assigning
  the resuming viewer. Rush-owned rows remain owned by the rush path. Frontend
  regressions cover optimistic “You”, receipt authors and local starter visibility.
- **B–D:** Viewer capabilities and applied personal-filter responses are private,
  no-store (including conditional responses); the shared conditional JSON writer
  preserves explicit cache policy. Viewer ETags are quoted even without a base
  ETag. Duplicate ID header values are anonymous; duplicate optional name/email
  values are ignored. Empty linked-approver names default to the ID.
- **E–F:** Single-word initials use that name's first two characters, falling back
  to ID only for an empty name. Directory writes release the mutex for SQLite I/O.
  Linked-device existence stamps are cached separately from header metadata so
  stale pairing snapshots cannot shadow current names or cause repeated inserts.
  Concurrent overlapping header writes invalidate unsafe unchanged-value shortcuts.
- **G–I:** Zero participant markers require a known author directory, as described
  above. Message author IDs are immutable after insertion, including UpdateMessage.
  Raw store starter IDs use `created_by_id`, avoiding collision with web objects;
  CLI JSON and exports were checked for consumers of the old raw key. No production
  consumer depends on the old raw string. Invalid incoming author objects no longer
  erase valid cached starter hints.
- **J–M:** New optimistic/local sessions carry the current viewer as a provisional
  starter until authoritative metadata arrives, so the personal filter retains
  them. Pending-steering configuration returns on nil state before assigning fields.
  Neutral package moves and the first-current-user-row starter rule are described
  above. Store regressions cover anonymous undo, all replacement/compaction paths,
  immutable authors and counter-independent first insertion.
- The three rejected findings (fresh bootstrap, anonymous branch starter reset and
  capabilities-before-sidebar ordering) remain unchanged. No commits, route golden
  changes or ratchet/baseline increases were made for these review fixes.

### Review-fix verification

- Focused full-package tests passed for serve (including its new helper packages),
  session, linked devices, pairing, LLM/provider isolation and side questions.
- Attribution/cache/starter tests passed with `-race -count=3` through `make test`.
  Bundle/wire-generation/CLI-session contract tests also passed.
- `make api-types`, `make pkgmap` and `make docs` completed. Frontend
  `format:check`, `lint`, `typecheck`, `test` and `make frontend` passed;
  133 frontend files / 2,115 tests passed.
- `make check-changed` and `make check-changed ARGS=--all` passed, including builds,
  vet, root-module tests, nested modules, frontend checks, complexity and plan lint.
- Targeted `chat.spec.ts --grep 'multi-user attribution'` Playwright smoke passed
  on desktop and mobile (2 tests), using the `browserfixture` build.
- Current entry measurement is **538,467 bytes raw / 157,933 bytes Go-level-6 gzip**,
  within the unchanged active caps of 538,500 / 158,000. This review pass did not
  increase any budget or baseline. Real trusted-proxy deployments and the complete
  Playwright suite remain outside this verification; targeted attribution coverage
  was run instead.

### CLI/TUI follow-up (2026-11-03)

- CLI/TUI attribution is now opt-in through `ask --user NAME`, `chat --user NAME`
  and terminal chat `/user NAME`. `/user` reports the current user; `/user off`
  clears it. There is no shorthand flag, config default or implicit adoption of
  the author of a resumed session. Existing turns are never relabeled.
- The literal, trimmed value is both ID and display name: valid UTF-8, no control
  characters, at most 256 bytes and 128 runes. Explicit empty/invalid flags fail
  before startup. Unlike optional proxy display headers, CLI names are not RFC
  2047 decoded. Header behavior is otherwise identical.
- Reusable validation and insert-if-missing CLI stamping live in the neutral
  `internal/authoridentity` package. Neither ask nor TUI code imports serve
  attribution helpers. Existing pre-feature app/runtime integration imports are
  unchanged; no new app-to-app dependency was added.
- Ask stamps the prepared human input before conversation/persistence setup,
  including agent and resumed runs. Chat stamps normal sends and steering at
  admission, not consumption, preserving the author if `/user` changes while
  guidance waits. Directory writes fill missing IDs only, never replacing proxy
  names/email. Existing starter/participants/store paths need no schema changes.
- Invocation-scoped selection is shared across replacement chat models and
  included in process reload state; session switches, handovers and reloads keep
  the current user. A new invocation without `--user` starts unattributed. TUI
  transcript rendering is unchanged; only short footer status/confirmation is new.
- Scope decision: human prompts and steering are attributed. Internal/generated
  continuation prompts, jobs and private side-question exchanges keep their
  existing semantics; no human identity is injected into model text or provider
  requests. Frontend code, route goldens and ledger baselines remain untouched by
  this follow-up, and all changes remain uncommitted.
- Regressions cover actual Cobra flag capture/validation, byte/rune/control limits,
  SQLite ask persistence with/without agents and continuation, multiple chat users,
  anonymous resumption, steering admission identity, reload/shared selection,
  insert-if-missing directory metadata, starter/participants and mock-provider
  request isolation. Documentation includes CLI flags, terminal slash commands
  and matching CLI IDs to trusted-proxy identities.

#### CLI/TUI follow-up verification

- Complete owning-package tests passed through `make test` for `cmd`,
  `authoridentity`, `app/ask`, `app/chat`, `app/serve/authorship` and `tui/chat`.
- CLI/ask/TUI attribution and provider-isolation regressions passed with
  `-race -count=3`; final chat runtime-binding, resume, steering and reload tests
  also passed three race-enabled runs.
- `make docs` regenerated both ask/chat flag tables and `PACKAGES.md`.
- `make check-changed` and `make check-changed ARGS=--all` passed, including
  build, vet, root and nested-module tests, frontend checks, complexity and plan
  lint. Frontend source was not changed by this follow-up.
- Final diff inspection preserves route goldens and ledger baselines. No commits
  were made. No live-provider or additional browser tests were needed: execution
  uses mock providers and the TUI, with no web behavior or frontend changes.
