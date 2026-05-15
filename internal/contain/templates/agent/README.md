# term-llm agent contain workspace: {{name}}

This workspace runs the managed term-llm agent image.

## Before first start

The generated `.env` contains your Web UI port/token and any provider
credentials collected during setup. Keep it private; it is written with `0600`
permissions.

If you skipped provider setup, edit `.env` later and add the credentials you
want to use.

## Commands

Start:

```sh
term-llm contain start {{name}}
```

Open shell:

```sh
term-llm contain shell {{name}}
```

Chat with the agent in your terminal using the workspace exec recipe:

```sh
term-llm contain exec {{name}} agent
```

`contain exec` first checks `x-term-llm.exec_recipes` in `compose.yaml`; if
there is no matching recipe it runs the command directly in the container. Use
`--` after the workspace name to force a raw command when a recipe name collides.

```sh
term-llm contain exec {{name}} -- term-llm --version
```

Rebuild after changing `TERM_LLM_VERSION`, `AGENT_DISTRO`, `AGENT_PLATFORM`,
`AGENT_BASE_IMAGE`, or when you want a fresh managed image:

```sh
term-llm contain rebuild {{name}}
```

The managed image supports `AGENT_DISTRO=arch` and `AGENT_DISTRO=fedora`.
Arch keeps the original pacman/AUR-based environment and defaults to
`linux/amd64` because Docker's official `archlinux:latest` image is amd64.
Fedora uses official multi-arch images and defaults to native `linux/arm64`,
which is the recommended Apple Silicon path. `AGENT_BASE_IMAGE` can be changed
to a compatible base image for the selected distro.

Stop without deleting state:

```sh
term-llm contain stop {{name}}
```

## Widgets

The Web UI starts with term-llm widgets enabled and uses
`/home/agent/.config/term-llm/widgets/` as the widget directory. Add widget apps
there and open them from `/chat/widgets/` (or your configured `WEB_BASE_PATH`
plus `/widgets/`). The bundled `widgets` skill contains the operational workflow
for creating, inspecting, restarting, and smoke-testing widget apps.

## Bootstrap model

On first boot, the image renders/copies its built-in bootstrap files into the
persistent `/home/agent` volume for agent `{{name}}`. Future boots ignore bootstrap
files and run `/home/agent/.config/term-llm/init.sh` from the volume. The runit
supervisor stays root, while shells, Web UI, jobs, bootstrap jobs, and normal
agent work run as the non-root `agent` user; use explicit passwordless `sudo`
inside the container when root privileges are needed. Interactive shells open as
`agent` in zsh, with `tl` available as a shorthand alias for `term-llm`.

To customize the first-boot bootstrap, add a static `/seed` mount containing a
`bootstrap.yaml`; otherwise use the defaults baked into the managed image.
