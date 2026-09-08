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
term-llm service uninstall hub      # preserve user data and saved setup
```

If exactly one native service is installed, commands can infer its kind. When
both exist, specify `web` or `hub` for individual operations.

A bare reinstall preserves the existing custom arguments, account environment,
and credentials:

```bash
term-llm service install web
```

Supplying arguments after `--` explicitly replaces the launch arguments. An
unchanged reinstall does not force a restart; changed settings, credential input,
or enrollment provisioning take effect through a restart. `--binary` selects the
executable, and `--working-directory` changes the working directory. Saved PATH
and XDG settings can be inspected in the private specification; reinstall does
not silently replace a custom account environment.

No updates are downloaded or scheduled. After replacing the binary, restart when
it is safe to interrupt active work:

```bash
term-llm upgrade
term-llm service restart web
```

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
