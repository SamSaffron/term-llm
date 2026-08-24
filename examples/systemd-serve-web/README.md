# term-llm `serve web` with systemd

This example runs `term-llm serve web` as a systemd **user service** and checks
at 04:00 each day whether the binary on disk differs from the binary currently
running. The service is restarted only when the binary changed.

A user service is intentional: term-llm normally uses the invoking user's
configuration, sessions, agents, skills, credentials, and project files. Do not
run it as root just to keep it running after logout; enable user lingering
instead.

## Files

- `install.sh` interactively installs, configures, validates when
  `systemd-analyze` is available, enables, and starts the user service and timer.
- `run-term-llm-web.sh` safely turns optional Hub environment settings into CLI
  arguments before replacing itself with term-llm.
- `term-llm-web.service` is the default unit for manual installation.
- `term-llm-web-update-check.timer` schedules a daily check at 04:00.
- `term-llm-web-update-check.service` invokes the comparison script.
- `restart-if-binary-changed.sh` compares the installed binary with
  `/proc/<MainPID>/exe` and conditionally restarts the Web UI.
- `.env.example` documents persistent secrets and provider credentials.
- `.gitignore` protects local configuration, credentials, and runtime data if
  this directory is used as a deployment directory.

The `/proc` comparison makes this a Linux-specific example, as systemd itself
normally is. Comparing bytes is more reliable than comparing modification
times: it detects an atomically replaced executable and avoids a restart when
only file metadata changed.

## Assumed paths

The supplied units assume:

- binary: `~/.local/bin/term-llm`
- deployment files: `~/.config/term-llm/systemd-web/`
- systemd user units: `~/.config/systemd/user/`
- working directory: your home directory

The default files used for manual installation keep term-llm config, data, and
cache isolated under the deployment directory. The installer instead asks and
defaults to the account's normal term-llm state so an existing setup works
immediately. If isolated state is selected, the layout is:

```text
~/.config/term-llm/systemd-web/
├── .env
├── install.conf
├── run-term-llm-web.sh
├── restart-if-binary-changed.sh
├── config/term-llm/config.yaml
├── data/term-llm/
└── cache/term-llm/
```

For manual installation, edit both `term-llm-web.service` and
`term-llm-web-update-check.service` first if your binary is elsewhere, and edit
`WorkingDirectory=` if term-llm should start in a particular project or
workspace. The installer discovers and writes these values automatically.

## Quick install

Run this from the example directory:

```bash
./install.sh
```

The installer finds the `term-llm` binary, asks where the service should start,
creates a stable bearer token, optionally records a provider API key, and can
optionally configure reverse Hub registration. It writes and validates the
systemd user units, enables the service and 04:00 timer, starts them, and offers
to enable user lingering so the Web UI survives logout.

It also asks whether to use the account's normal term-llm config and session
store (the default) or keep isolated config, data, and cache under the
deployment directory. Existing `.env` secrets and non-secret choices in
`install.conf` are preserved when the installer is run again. Explicit flags or
interactive answers can change those choices; changed units, helper scripts, or
environment settings cause an immediate, reported service restart rather than
taking effect later at reboot.

When it finishes, it prints the exact status, log, restart, update-check,
configuration, and reinstallation commands needed for maintenance.

See all automation options with:

```bash
./install.sh --help
```

For example, an unattended installation that uses normal account state is:

```bash
./install.sh --non-interactive --normal-state
```

Interactive installation is recommended when adding secrets. Provisioning
systems can pass the Hub registration token through
`--hub-registration-token-file`; the installer does not accept that secret as a
command-line value.

## Manual installation

If you prefer to inspect and copy every file yourself, run these commands from
this example directory:

```bash
install -d "$HOME/.config/term-llm/systemd-web"
install -d "$HOME/.config/systemd/user"

install -m 755 restart-if-binary-changed.sh run-term-llm-web.sh \
  "$HOME/.config/term-llm/systemd-web/"
install -m 644 term-llm-web.service term-llm-web-update-check.service \
  term-llm-web-update-check.timer "$HOME/.config/systemd/user/"

# Do not overwrite an existing secrets file.
if [ ! -e "$HOME/.config/term-llm/systemd-web/.env" ]; then
  install -m 600 .env.example "$HOME/.config/term-llm/systemd-web/.env"
fi
```

Generate a long-lived bearer token and edit the environment file:

```bash
openssl rand -hex 32
$EDITOR "$HOME/.config/term-llm/systemd-web/.env"
```

At minimum, replace `TERM_LLM_SERVE_TOKEN`. Add the credential for your
provider, unless authentication is already configured in the isolated
`config.yaml` described below. The token must remain stable across restarts or
saved browser and API credentials will stop working.

Reload systemd, then start the Web UI and timer:

```bash
systemctl --user daemon-reload
systemctl --user enable --now term-llm-web.service
systemctl --user enable --now term-llm-web-update-check.timer
```

Open <http://127.0.0.1:8080/ui/> and enter the bearer token from `.env`.

### Keep it running after logout

On a server, allow this user's systemd manager to run without an active login
session:

```bash
sudo loginctl enable-linger "$USER"
```

This is generally preferable to turning term-llm into a root-owned system
service.

## Configuration in this directory

When isolated state is selected—or when the supplied unit is installed
manually—the service sets these XDG roots:

```text
XDG_CONFIG_HOME=~/.config/term-llm/systemd-web/config
XDG_DATA_HOME=~/.config/term-llm/systemd-web/data
XDG_CACHE_HOME=~/.config/term-llm/systemd-web/cache
```

Consequently, term-llm reads its configuration from:

```text
~/.config/term-llm/systemd-web/config/term-llm/config.yaml
```

To edit that configuration with the CLI, use the same XDG root as the service:

```bash
XDG_CONFIG_HOME="$HOME/.config/term-llm/systemd-web/config" \
  term-llm config edit
```

The local `.gitignore` excludes `.env` and all three XDG state directories.
That makes it safer to copy this example into a separate Git repository, but
always inspect `git status` before committing.

If you prefer your normal `~/.config/term-llm/config.yaml` and normal session
store, remove the three `Environment=XDG_...` lines from
`term-llm-web.service`.

### Optional reverse Hub registration

The environment template can configure this Web UI as a reverse-connected Hub
node. This is useful when the Hub cannot directly reach the node, but the node
can make an outbound connection to the Hub.

The installer prompts for these settings and writes safely quoted values. For a
manual installation, uncomment and edit:

```text
TERM_LLM_SERVE_HUB_URL=https://hub.example.com
TERM_LLM_SERVE_HUB_NODE_ID=my-node
TERM_LLM_SERVE_HUB_NODE_NAME=My node
TERM_LLM_SERVE_HUB_REGISTER=1
TERM_LLM_HUB_REGISTRATION_TOKEN=replace-with-the-hub-registration-token
```

The launch wrapper converts those individual settings into CLI arguments, so a
display name may contain spaces without relying on a whitespace-split argument
bundle.

The settings satisfy the reverse-registration contract as follows:

- `TERM_LLM_SERVE_HUB_URL` is the Hub URL as reachable from this machine. The
  installer adds `https://` when the entered URL has no scheme.
- `TERM_LLM_SERVE_HUB_NODE_ID` is unique on that Hub.
- `TERM_LLM_SERVE_HUB_REGISTER=1` enables reverse connection and startup
  registration.
- `TERM_LLM_HUB_REGISTRATION_TOKEN` must equal the registration token
  configured on the Hub.
- `TERM_LLM_SERVE_TOKEN` is this node's stable bearer token. The Hub records it
  during registration and uses it when proxying authenticated requests.

For unattended setup, put the registration token in a mode-0600 file and use
`--hub-registration-token-file`; do not put it on the command line:

```bash
./install.sh --non-interactive \
  --hub-url https://hub.example.com \
  --hub-node-id my-node \
  --hub-node-name "My node" \
  --hub-registration-token-file /run/user/$(id -u)/hub-registration-token
```

The Hub must have reverse registration enabled. Restart the service after
manually changing `.env`:

```bash
systemctl --user restart term-llm-web.service
journalctl --user -u term-llm-web.service -n 50 --no-pager
```

A successful startup logs both the registration and reverse connection. Treat
both tokens in `.env` as secrets.

## Daily update check

The timer runs at 04:00 local time. `Persistent=true` means a check missed while
the machine was asleep runs after it next starts.

The checker:

1. Does nothing if `term-llm-web.service` is inactive.
2. Obtains the service's current main PID.
3. Compares the configured installed binary with `/proc/<MainPID>/exe`.
4. Restarts the service only if those executable contents differ.

The timer **does not download or install updates**. Use your normal update
mechanism, for example:

```bash
term-llm upgrade
```

If that replaces the binary, the running server continues using the old image
until 04:00, unless you restart it manually.

Change the schedule by editing `OnCalendar=` in the timer. Confirm the parsed
schedule with:

```bash
systemd-analyze calendar '*-*-* 04:00:00'
systemctl --user list-timers term-llm-web-update-check.timer
```

Run the check immediately without waiting for the timer:

```bash
systemctl --user start term-llm-web-update-check.service
journalctl --user -u term-llm-web-update-check.service -n 20 --no-pager
```

A manual unconditional restart remains available:

```bash
systemctl --user restart term-llm-web.service
```

## Status and logs

```bash
systemctl --user status term-llm-web.service
systemctl --user status term-llm-web-update-check.timer
journalctl --user -u term-llm-web.service -f
curl http://127.0.0.1:8080/ui/healthz
```

After changing a unit file, reinstall it and run
`systemctl --user daemon-reload`. Restart the Web UI if its service definition
changed; restart the timer if its schedule changed.

## Network and security notes

The example binds only to loopback. For remote access, prefer an SSH tunnel,
VPN, or a TLS reverse proxy. For example:

```bash
ssh -L 8080:127.0.0.1:8080 your-server
```

Then browse to <http://127.0.0.1:8080/ui/> locally.

Do not disable authentication on a remotely reachable listener. If you change
the host to `0.0.0.0`, protect the service with a firewall and TLS-capable
reverse proxy, and keep `TERM_LLM_SERVE_TOKEN` secret.

The example deliberately avoids aggressive systemd filesystem sandboxing:
term-llm agents and tools may need access to project files and subprocesses.
Use term-llm approval and tool permission settings appropriate for your threat
model.

## Uninstall

```bash
systemctl --user disable --now term-llm-web-update-check.timer
systemctl --user disable --now term-llm-web.service
rm -f "$HOME/.config/systemd/user/term-llm-web.service" \
  "$HOME/.config/systemd/user/term-llm-web-update-check.service" \
  "$HOME/.config/systemd/user/term-llm-web-update-check.timer"
systemctl --user daemon-reload
```

The commands above intentionally preserve
`~/.config/term-llm/systemd-web/`, which may contain credentials, configuration,
sessions, and other state. Back it up or remove it separately when appropriate.
