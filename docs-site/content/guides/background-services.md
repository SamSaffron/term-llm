---
title: "Background services"
weight: 17
description: "Install Web and Hub as per-user systemd or launchd services, with passkeys or custom server arguments."
kicker: "Deploy agents"
---

`term-llm service` keeps a personal Web UI, a Hub, or both running without an
open terminal. Linux uses **systemd user services**; macOS uses **LaunchAgents**.
The OS supervises the ordinary `serve` process—there is no separate daemon or
internal watchdog. Run these commands as your normal user, **not with sudo**.

On macOS 14+, [term-llm for Mac](/getting-started/installation/#macos-app-recommended)
already manages the local web service and start-at-login; you do not need a
separate `term-llm service install web` for the app-managed workspace. The
instructions below cover CLI-managed services.

## Quick start

Configure term-llm normally first, then:

```bash
term-llm service install web
# Optional: install a separate Hub alongside it.
term-llm service install hub
```

Omitting the kind on `install` defaults to `web`. New installs use:

| | Web | Hub |
|---|---|---|
| Browser URL | `http://localhost:8080/ui/` | `http://localhost:8090/hub/` |
| Bind address | `127.0.0.1` | `127.0.0.1` |
| Authentication | Passkey | Passkey |
| State | Existing account/Web state | Existing account/Hub state |

The installer finds the executable, records the account environment and working
directory, installs the native definition, enables autostart, starts the service,
and checks the local HTTP backend. It offers to open the browser when interactive.
No model request is made: **provider credentials and public HTTPS reachability
are not established by the local HTTP check**.

If no passkey has been enrolled, installation prints a one-time setup code in an
interactive terminal. Open the printed setup URL, enter the code, and create a
passkey. Existing compatible Web/Hub passkeys and sessions are reused.

```bash
term-llm service open web
term-llm service open hub
```

Subsequent browser visits use the existing session or the normal passkey sign-in
page. The service CLI does not introduce another login protocol.

### Unfinished enrollment and recovery

```bash
# Generate a fresh first-enrollment capability and restart the service.
term-llm service setup web

# If all existing passkeys are inaccessible, explicitly authorize recovery.
term-llm service recover web
```

`setup` never deletes existing passkeys; when already enrolled, it directs you to
sign in. `recover` requires an existing credential store and uses the existing
recovery flow. Both operations can restart the server and interrupt active work.

The temporary capability is stored in a private service file, **not in the
unit/plist or service logs**. Its enrollment window lasts ten minutes after the
server reads it. Restarting an uncompleted enrollment renews that window with the
same supplied secret; `setup`/`recover` explicitly replace it. After successful
enrollment, the next managed server startup detects the added credential and
removes the consumed temporary file rather than renewing its authority.

Codes are not printed to redirected output by default. Automation can explicitly
use `--print-setup-code` on `install`, `setup`, or `recover`; treat that output as
a temporary enrollment credential. `--yes` only suppresses the browser prompt;
it does **not** permit secret disclosure.

## Custom server arguments

Arguments after `--` are additional flags for the corresponding `serve` command:

```bash
term-llm service install web -- --auth bearer --port 8081

term-llm service install hub -- \
  --auth passkey --public-url https://hub.example.com/hub/
```

The installer parses the actual `serve web`/`serve hub` flag definitions. Existing
server compatibility rules still apply: Web passkeys do not support Hub reverse
registration, so use bearer mode for a Hub-connected Web node.

Managed listeners remain loopback-only. For remote access, configure HTTPS and a
reverse proxy separately. A passkey public URL fixes the relying-party identity;
use the same stable hostname after installation. The public URL and mount must
match. The service persists an explicit mount so unrelated shell/config changes
do not redirect its cookies or enrollment paths.

Relative server paths resolve against the recorded working directory (home by
default). Use absolute paths or explicitly choose `--working-directory`:

```bash
term-llm service install hub --working-directory "$HOME/hub" -- \
  --config "$HOME/hub/nodes.yaml"
```

### Bearer credentials and Hub registration

Bearer-mode installs generate a stable token if one has not been supplied or
stored previously. It is never printed during normal installation or startup.
Explicitly reveal it when needed for the existing browser/API login:

```bash
term-llm service token web
```

Do not place secrets after `--`. Flags such as `--token` and
`--hub-registration-token` are rejected in persisted launch arguments. Use masked
input instead:

```bash
term-llm service install web \
  --secret TERM_LLM_SERVE_TOKEN \
  --secret TERM_LLM_HUB_REGISTRATION_TOKEN -- \
  --auth bearer \
  --hub-url https://hub.example.com/hub/ \
  --hub-node-id my-laptop \
  --hub-connect reverse \
  --hub-register
```

To enable incoming registration on a managed Hub, provision
`TERM_LLM_HUB_REGISTRATION_TOKEN` with `--secret` or `--secrets-file`. A passkey Hub
can also accept an explicitly provisioned `TERM_LLM_HUB_TOKEN` for its existing API
compatibility mode; the installer does not create that second credential by default.

For automation, use a mode-0600 file:

```text
TERM_LLM_SERVE_TOKEN=your-existing-web-token
TERM_LLM_HUB_REGISTRATION_TOKEN=your-hub-registration-token
OPENAI_API_KEY=your-provider-key
```

```bash
term-llm service install web --secrets-file ./service-secrets.env -- \
  --auth bearer --port 8081
```

This is **literal `KEY=value`**, not a shell script: no `export`, interpolation,
or quote removal. Duplicate keys are errors. Only credential-like variable names
are allowed (such as `*_API_KEY`, `*_TOKEN`, `*_SECRET`, `*_PASSWORD`); loader hooks,
PATH/HOME overrides, and bootstrap/recovery variables are not accepted here.
Enrollment secrets are owned by the setup/recovery flow.

### Where credentials live

- **macOS:** per-service generic-password items in the user's Keychain. The
  installer uses `/usr/bin/security` with encoded input over stdin, never a
  password in subprocess arguments. It verifies readback and never silently
  falls back to plaintext. Unlock/access approval may be required. The native
  tool controls item access; this is not an application-signing isolation claim.
- **Linux:** mode-0600 files inside the private service directory. A desktop
  keyring is not required, allowing operation without an unlocked desktop.

The launch specification contains only credential names. Values are resolved by
the internal runner and passed to the server, not written to the unit/plist.
Uninstall deliberately preserves these credentials, but removes any temporary
setup/recovery capability.

Keychain input is bounded by Apple's command-line protocol; unusually large
credentials are rejected rather than truncated or placed in arguments. Validate
unattended access on the intended macOS account.

## Environment and existing state

The default working directory is home, not an implicit agent workspace grant.
The specification records HOME, PATH, XDG config/data/cache locations and selected
locale/shell settings. It does **not** source shell profiles or capture the entire
environment. Dynamic desktop/session endpoints come from the native supervisor.

Persistent provider configuration and secret references continue to work when
available to that user session. Terminal-only credentials are not imported
silently: the installer warns about their names. If needed, import them with
`--secret NAME` or `--secrets-file`. Changing a credential used by a running server
requires a restart.

## Lifecycle and maintenance

```bash
term-llm service status             # list Web and Hub
term-llm service logs web
term-llm service logs hub --follow
term-llm service stop web           # stop AND disable autostart
term-llm service start web          # enable autostart and start
term-llm service restart web        # enabled service only; interrupts active work
term-llm service start web --no-enable    # start without changing autostart
term-llm service restart web --no-enable  # restart even when autostart is disabled
term-llm service stop web --keep-enabled  # stop without changing autostart
term-llm service autostart web on         # enable login autostart only
term-llm service autostart web off        # disable login autostart only
term-llm service autostart web status     # prints enabled or disabled
term-llm service uninstall hub      # preserve user data and saved setup
```

Running and login autostart are independent controls. The default `start` still
**enables and starts**, and the default `stop` still **disables and stops**.
`start --no-enable` and `restart --no-enable` leave autostart unchanged and do not
require it to be enabled. Ordinary `restart` requires autostart to be enabled,
but does not change it. All restarts interrupt active work.

`stop --keep-enabled` leaves the autostart setting unchanged (including when it
was already disabled). On macOS it unloads the current LaunchAgent without
changing its persistent login setting. Starting an unloaded macOS service with
`--no-enable` uses a forced, one-session `launchctl load -F`, without `-w`, so a
disabled login override remains disabled.

`autostart on|off` changes only the login setting: turning it on does not start a
stopped service, and turning it off does not stop a running service. `autostart
status` prints `enabled` or `disabled` and exits successfully on a successful
query. These operations require an installed managed specification and an owned
native definition; they never adopt an unmanaged service.

For startup failures, enable diagnostics:

```bash
term-llm service start web --debug
term-llm service restart hub --debug
```

`--debug` writes startup checks, executable and working-directory paths, and
native service-manager commands and their outcomes to stderr. On failure it also
collects native lifecycle status and the last 60 service log lines. Readiness
errors include the last HTTP health-check result. Debug mode does not change the
saved service configuration or enable server debug logging. Credentials and the
native environment are not dumped, but service logs may contain sensitive data;
review the output before sharing it.

If the health check times out while the server process is not running, the error
includes the supervisor's lifecycle state (for example `state = spawn scheduled`
and `last exit code = 1`). That means the server is exiting during startup and
being restarted; the reason is in `term-llm service logs`.

A Hub-registered Web node keeps serving locally when the Hub is unreachable at
startup. It retries registration in the background (backing off to once a
minute) and deregisters on shutdown only if registration succeeded. A Hub that
rejects the registration, for example because of a wrong registration token,
still stops startup.

If exactly one native service is installed, commands can infer its kind. When
both exist, specify `web` or `hub` for individual operations.

A bare reinstall preserves the existing custom arguments, account environment,
credentials, and recorded executable, even when run from a different binary:

```bash
term-llm service install web
```

Supplying arguments after `--` explicitly replaces the launch arguments. An
unchanged reinstall does not force a restart; changed settings, credential input,
or enrollment provisioning take effect through a restart. `--binary` selects a
different executable (the first install uses the running one), and `--working-directory` changes the working directory. Saved PATH
and XDG settings can be inspected in the private specification; reinstall does
not silently replace a custom account environment. After changing your shell's
PATH (for example adding a tool with mise or asdf), run the reinstall from a
fresh terminal with `--refresh-env` to record the current PATH, SHELL, LANG and
XDG locations; the service restarts when they changed:

```bash
term-llm service install web --refresh-env
```

Run it from a normal terminal, not from a shell inside the service, which would
record the service's existing PATH again.

No updates are downloaded or scheduled. After replacing the binary, restart when
it is safe to interrupt active work:

```bash
term-llm upgrade
term-llm service restart web
```

### Machine-readable status

```bash
term-llm service status --json       # web, then hub (even when not installed)
term-llm service status web --json   # only web
term-llm service status hub --json   # only hub
```

`--json` is available only on `status`. It writes one JSON object, with service
failures represented as data rather than a failing exit status. Flag/usage errors
still fail normally. The default, human-readable status output is unchanged.
Inspection does not create files, change autostart, or load/start/stop services.

The envelope has `schema_version: 1`, `generated_at` (UTC RFC 3339 timestamp),
`uid` (the inspected user ID), `platform` (`darwin` or `linux`), and `services`
(an array in the order above). Consumers should check the schema version and
ignore unknown fields for forward compatibility. For example:

```json
{
  "schema_version": 1,
  "generated_at": "2026-11-07T09:00:00Z",
  "uid": 501,
  "platform": "darwin",
  "services": [
    {
      "kind": "web",
      "installed": true,
      "managed": true,
      "registered": true,
      "state": "ready",
      "running": true,
      "pid": 1234,
      "autostart": false,
      "binary": "/opt/term-llm/bin/term-llm",
      "url": "http://127.0.0.1:8765/ui/",
      "host": "127.0.0.1",
      "port": 8765,
      "base_path": "/ui",
      "auth": "bearer",
      "enrollment": "not_required",
      "healthy": true,
      "health_error": "",
      "spec_path": "/Users/example/.config/term-llm/services/web/service.json",
      "native_path": "/Users/example/Library/LaunchAgents/com.term-llm.web.plist",
      "last_exit_code": 0,
      "error": ""
    }
  ]
}
```

Per-service fields:

| Field | Meaning |
|---|---|
| `kind` | `web` or `hub`. |
| `installed` | The managed specification exists and loads successfully. |
| `managed` | A valid matching specification is present and any native definition passes the installer ownership-marker check. True for a retained specification without a native definition. |
| `registered` | The native definition exists on disk; this does not mean the job is currently loaded or running. |
| `state` | One of the states below. |
| `running`, `pid` | The native manager reports a positive PID; otherwise false and zero. |
| `autostart` | Native login-enabled setting (`launchctl print-disabled` or `systemctl --user is-enabled`); false when unknown or not managed/registered. Independent of `running`. |
| `binary` | Recorded executable path, not the executable invoking status. No binary is executed to discover its version. `version` is currently omitted (it is not the specification version). |
| `url`, `host`, `port`, `base_path` | Browser URL and local bind/mount metadata from the specification. URL user information, query and fragment are removed. Empty strings/zero when unavailable. For an installed service `base_path` is the normalized mount prefix the server actually uses (for example `/ui`); the serve base-path parser rejects an empty value or `/`, so those appear only when the specification is unavailable. |
| `auth` | `bearer`, `passkey`, or `none`; empty when unavailable. |
| `enrollment` | `not_required` except for passkey auth: `pending` for zero credentials, `enrolled` otherwise. Empty with `state: "error"` if the count cannot be inspected. Only the count is inspected, never credential contents emitted. |
| `healthy`, `health_error` | True only for a running process whose local health endpoint returns HTTP 200. A failed probe supplies a sanitized diagnostic; otherwise the error string is empty. |
| `spec_path`, `native_path` | Expected managed specification and native definition paths, including when absent. Empty if the environment cannot be resolved. |
| `last_exit_code` | Integer when reported by the native manager; omitted when unknown. |
| `error` | Sanitized inspection/manager error, otherwise empty. Never native command output. |

All listed boolean and string fields, and `pid`/`port`, remain present even when
false, empty or zero. No arguments, environment values, tokens, secrets,
credential contents, or log output are included.

States are:

- `not_installed`: no specification or native definition.
- `unmanaged`: a native definition exists without a loadable managed
  specification, or fails the ownership check. It is left untouched. Without a
  managed specification, `installed` and `managed` are false while `registered`
  is true.
- `not_registered`: the specification is retained but the native definition is
  absent (for example after uninstall).
- `stopped`: registered and managed, but no native running PID. An unloaded
  macOS job with a retained plist is also stopped.
- `ready`: a native running PID **and** a local `/healthz` HTTP 200 response.
- `unhealthy`: a native running PID, but the health probe failed. Inspect
  `health_error`.
- `error`: inspection or manager status query failed. Inspect `error`.

Health probes use the local host, port and mount, just like installation
readiness checks, not the public browser URL. They use a bounded two-second
request, with no proxy, authentication or redirects. Manager queries are also
bounded. A foreign HTTP responder without a managed running PID cannot make a
service ready. Unknown/unavailable status is not an instruction to reinstall or
adopt the service.

### Preview without touching the OS

```bash
term-llm service install hub --dry-run
term-llm service install web --no-start -- --auth bearer --port 8081
```

`--dry-run` prints the plan and native definition without creating files or
credentials. `--no-start` writes files but does not enable/start the service.

Existing manually managed definitions—including the older systemd example—are
**not overwritten or adopted automatically**. Migrate them explicitly after
backing up their configuration and stopping them yourself. A port conflict also
fails rather than killing another process or silently selecting a different port.

### macOS Local Network permission

A LaunchAgent, unlike a command run from Terminal, needs macOS Local Network
permission to connect to a Hub on a local-network address. When access is blocked,
the Hub connection fails with `connect: no route to host` even though the same URL
works from a terminal; the service log then includes recovery steps.

macOS authorizes each new executable build separately, so the first connection
after an upgrade or rebuild is blocked. macOS only completes that authorization
while the blocked process is still running. Because the node keeps serving and
retrying, the connection normally succeeds a few seconds later without any
action.

If it stays blocked, enable **Term LLM** under System Settings > Privacy &
Security > Local Network. Alternatively, exempt the Hub's address from Local
Network privacy for every program, then restart the Mac. This is Apple's
documented administrator setting; use `AllowedEthernetLocalNetworkAddresses` for
wired networks:

```bash
sudo defaults write com.apple.network.local-network \
  AllowedWiFiLocalNetworkAddresses -array 10.0.0.51/32
```

## Platform behavior and files

| | Linux | macOS |
|---|---|---|
| Supervisor | systemd user manager | launchd GUI user domain |
| Native identity | `term-llm-web.service`, `term-llm-hub.service` | `com.term-llm.web`, `com.term-llm.hub` |
| Native files | `$XDG_CONFIG_HOME/systemd/user/` | `~/Library/LaunchAgents/` |
| Logs | user journal | private `service.log` beside the specification |

Specifications live under `$XDG_CONFIG_HOME/term-llm/services/{web,hub}/service.json`
(falling back to `~/.config`). LaunchAgent log rotation is not provisioned by this
initial implementation; monitor log size or arrange rotation separately.

Closing a terminal does not stop either service. Linux operation before login or
after logout requires user lingering:

```bash
sudo loginctl enable-linger "$USER"
```

This account-wide policy is never changed automatically or undone by uninstall.
A macOS LaunchAgent starts at login and requires that user session; it is not a
pre-login system daemon. Neither platform makes a sleeping machine continuously
available. There are no root installs, arbitrary instance profiles, automatic
local-node registration, or automatic updates.
