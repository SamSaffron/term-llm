---
title: "Sharing"
weight: 8
description: "Share session transcripts through the built-in GitHub publisher or a custom Share Helper Protocol v1 command."
kicker: "Transcripts"
---

term-llm can publish rendered transcript bundles without coupling the Web UI, TUI, or session store to one hosting service.

## User commands

From the chat TUI:

```text
/share [new] [raw] [public|unlisted|private]
```

From the CLI, for a complete saved session:

```bash
term-llm sessions share 42
term-llm sessions share 42 --visibility private # requires a custom provider advertising private shares
term-llm sessions share 42 --include-raw-reasoning
term-llm sessions share 42 --new --json
```

A compatible whole-session share is updated by default. `new` or `--new` always creates another share. An update is offered only when the stored share has `scope: session`, belongs to the currently configured provider, and that provider advertises `update`. Replacing saved state warns that the old provider link may remain active.

Raw model reasoning is never included implicitly, even when `reasoning.export: raw` is configured. It requires the explicit `/share raw` or `--include-raw-reasoning` opt-in, remains subject to `reasoning.raw` and `reasoning.source`, and is accompanied by a privacy warning because it may contain sensitive information.

The Web UI shares either one response or the visible conversation through that response. These point-in-time Web shares are deliberately **not persisted**. A later whole-session update therefore cannot add broader content to a response-only URL.

The older `agents export/import gist` and `sessions export gist` commands remain GitHub-specific and do not use this protocol.

## Providers

GitHub Gist is the default provider and uses the existing `gh` authentication:

```yaml
share:
  provider: github
  timeout: 120s
```

It supports `unlisted` and `public` shares. Unlisted Gists are accessible to anyone with the URL; they are not private. After creating or updating a Gist, term-llm makes at most five anonymous readiness requests to the primary `https://gisthost.github.io/` transcript URL within ten seconds. A `403`, `429`, network failure, or exhausted probe does not turn an already-created or updated Gist into a failure; the result instead has `ready: false`. Updates query GitHub for the Gist's existing visibility because GitHub cannot change visibility in place.

To use a custom executable:

```yaml
share:
  provider: command
  command: [/usr/local/bin/acme-share, --tenant, engineering]
  timeout: 120s
```

The configured array is an argv vector, not shell text. term-llm resolves `argv[0]` once (including through `PATH`) and stores an absolute executable path before changing to a bundle directory. It appends `capabilities`, `create`, or `update` as the final argument. Prefix arguments are preserved exactly. `share.timeout` controls create/update and must be greater than zero and at most `600s`; its default is `120s`. Capabilities always has a maximum timeout of ten seconds.

## Share Helper Protocol v1

### Process contract

The protocol identifier is `term-llm-share` and the version is JSON number `1`.

Each operation is one process invocation:

```text
COMMAND [PREFIX_ARG ...] capabilities
COMMAND [PREFIX_ARG ...] create
COMMAND [PREFIX_ARG ...] update
```

term-llm executes the argv directly without a shell. The helper reads exactly one JSON object from standard input and writes exactly one JSON object to standard output. Standard error is diagnostic-only: it is captured with a 64 KiB process bound, quoted and truncated again to 4 KiB on the operator log surface, and is never included in Web or TUI errors or their wrapped error chains. Standard output is limited to 1 MiB. Extra non-JSON standard output makes the response invalid.

The process runs in a detached process group. Cancellation and timeout kill the group. After any completed invocation, term-llm also cleans up background descendants; a successful zero-exit helper whose descendant retains inherited pipes is accepted after the bounded wait delay and its completed stdout response is still parsed.

### Capabilities request

The `capabilities` working directory is unspecified. Input:

```json
{
  "protocol": "term-llm-share",
  "version": 1,
  "request_id": "7de40dd4ff5347af93e2187df9c4ef83"
}
```

`request_id` is unique per invocation and should be included in provider-side diagnostics or idempotency records. It is not a persistent share ID.

A successful capabilities response has this schema:

```json
{
  "protocol": "term-llm-share",
  "version": 1,
  "provider": {
    "id": "acme-vault",
    "name": "Acme Vault",
    "help": "Run acme-share login if authentication is required."
  },
  "operations": ["create", "update"],
  "visibilities": ["private", "unlisted"],
  "default_visibility": "private",
  "notes": ["Private links expire after 30 days."],
  "limits": {
    "expires_after_days": 30
  }
}
```

Required fields and semantics:

| Field | Requirement |
|---|---|
| `protocol` | Exactly `term-llm-share`. |
| `version` | Exactly `1`. |
| `provider.id` | Stable identifier containing 1–64 printable ASCII bytes without whitespace. It is persisted with whole-session shares and gates future updates. `github` is reserved for the built-in provider and is rejected from command helpers. |
| `provider.name` | Non-empty human-readable name, at most 128 UTF-8 bytes and without control characters. |
| `provider.help` | Optional user guidance, at most 2048 UTF-8 bytes and without control characters. It may mention provider-specific installation or authentication. |
| `operations` | Must contain `create`; may also contain `update`. No other v1 operation is valid. |
| `visibilities` | Non-empty unique list drawn only from `public`, `unlisted`, and `private`. |
| `default_visibility` | Must be one of `visibilities`. |
| `notes` | At most 16 non-empty entries. Each is at most 1024 UTF-8 bytes and cannot contain control characters. Clients show these notes to users. |
| `limits` | Optional JSON object with at most 32 keys, at most four nested levels, bounded arrays/objects, and an encoded size of at most 16 KiB. |

Advertising `update` means the helper can replace the bundle associated with its own opaque ID. A helper that cannot safely preserve an existing URL must omit `update`.

### Create and update bundle

For `create` and `update`, term-llm creates a fresh private temporary directory with mode `0700`, writes bundle files with mode `0600`, sets that directory as the helper's working directory, and removes it after every success or failure.

Version 1 transcript bundles contain at most 32 files, with a 16 MiB per-file limit and a 32 MiB total-content limit. The standard bundle contains:

- `index.html` — standalone rendered transcript and the entrypoint;
- `session.md` — Markdown source transcript.

The JSON manifest names files relative to the working directory. Helpers must reject absolute paths, traversal, or files not declared by the manifest. File content is read from the working directory and is not duplicated in JSON.

Create input:

```json
{
  "protocol": "term-llm-share",
  "version": 1,
  "request_id": "39b843c39b624315b80e04297c67ef09",
  "title": "Investigate latency",
  "description": "term-llm session: Investigate latency",
  "visibility": "private",
  "entrypoint": "index.html",
  "files": [
    {
      "name": "index.html",
      "media_type": "text/html; charset=utf-8",
      "role": "entrypoint"
    },
    {
      "name": "session.md",
      "media_type": "text/markdown; charset=utf-8",
      "role": "transcript"
    }
  ]
}
```

Update input is identical and adds the helper's previously returned opaque ID:

```json
{
  "protocol": "term-llm-share",
  "version": 1,
  "request_id": "becf0f36c9224cfd8a8976af1037914d",
  "id": "share_Y2hhbmdlLW1l",
  "title": "Investigate latency",
  "description": "term-llm session: Investigate latency",
  "visibility": "private",
  "entrypoint": "index.html",
  "files": [
    {"name": "index.html", "media_type": "text/html; charset=utf-8", "role": "entrypoint"},
    {"name": "session.md", "media_type": "text/markdown; charset=utf-8", "role": "transcript"}
  ]
}
```

`title` and `description` are display metadata. `visibility` has already been checked against capabilities. On update, the helper must return the actual resulting visibility; it must not claim a requested visibility that the remote object could not adopt.

### Success response and readiness

A successful create or update exits zero and returns:

```json
{
  "protocol": "term-llm-share",
  "version": 1,
  "id": "share_Y2hhbmdlLW1l",
  "url": "https://shares.example.test/s/share_Y2hhbmdlLW1l",
  "source_url": "https://shares.example.test/s/share_Y2hhbmdlLW1l/source",
  "visibility": "private"
}
```

`source_url` is optional. All returned URLs must be absolute HTTPS URLs. Plain HTTP is accepted only for loopback hosts such as `localhost`, `127.0.0.1`, or `::1`. A URL is at most 2048 bytes and cannot contain whitespace, control characters, or embedded user information. `id` is 1–256 printable ASCII bytes without whitespace. `visibility` must be one of the three v1 values.

**A zero exit is a readiness promise:** `url` must already be usable when the helper exits. term-llm does not probe custom URLs, because they may require authentication and probing could disclose credentials or create load. Successful custom-provider results therefore have `ready: true`.

### Failure response

A failed operation exits nonzero. It may return this structured object on stdout:

```json
{
  "error": {
    "code": "auth_required",
    "message": "Sign in with acme-share login and try again."
  }
}
```

Stable v1 codes are:

| Code | Meaning |
|---|---|
| `dependency_missing` | The helper or one of its required programs is unavailable. |
| `auth_required` | Provider authentication is missing or expired. |
| `timeout` | The provider operation timed out. term-llm also generates this code when it kills an overdue helper. |
| `provider_error` | The remote provider rejected or could not complete the operation. |
| `protocol_error` | Input/output does not satisfy this protocol. |
| `unsupported_visibility` | The requested visibility cannot be provided. Normally term-llm rejects this before create/update using capabilities. |

The message must be safe for an end user: valid UTF-8, non-empty, no control characters, and at most 1024 bytes. Do not put tokens, raw HTTP bodies, stack traces, or command diagnostics in it. Put diagnostics on stderr; term-llm never forwards arbitrary stderr to Web or TUI users. Unknown error codes are normalized to `provider_error`; an invalid message is replaced with `share helper failed`. A nonzero exit without a valid structured error becomes the same curated message.

## Web API

Authenticated clients can discover the active provider with:

```text
GET /v1/sharing/capabilities
```

The response uses `Cache-Control: no-store` and returns `enabled`, `provider`, `operations`, `visibilities`, `default_visibility`, `help`, `notes`, and optional `limits`. Unlike generic `GET /v1/capabilities`, this endpoint may invoke a configured helper. Generic capabilities deliberately omits sharing details so it never waits for or implies readiness of a subprocess.

Point-in-time creation remains:

```text
POST /v1/sessions/{id}/shares
```

Request fields are `anchor_message_id`, `scope` (`response` or `conversation`), and `visibility`. For one compatibility release, `public: true|false` is accepted only when `visibility` is absent. The generic response fields are `provider`, `id`, `url`, optional `source_url`, `visibility`, `ready`, and `scope`. GitHub responses additionally include legacy `gist_id`, `gist_url`, `preview_url`, and `public` fields for one compatibility release.

Errors use a stable `error.code` and curated `error.message`; helper stderr is never returned.
