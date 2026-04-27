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

Rebuild after changing `TERM_LLM_VERSION` or when you want a fresh managed image:

```sh
term-llm contain rebuild {{name}}
```

Stop without deleting state:

```sh
term-llm contain stop {{name}}
```

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
