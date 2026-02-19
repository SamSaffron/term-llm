# runit services

This directory is copied into `/etc/sv/` in the container image.
Place service directories here and they will be available to runit at runtime.

## Enabling a service

Services are **not** auto-enabled by the image. To enable one, symlink it into
the runsvdir (this persists on the `/root` volume across restarts):

```bash
ln -sf /etc/sv/<name> /etc/runit/runsvdir/<name>
```

Or manage it from outside the container:

```bash
docker exec jarvis ln -sf /etc/sv/<name> /etc/runit/runsvdir/<name>
```

## Service structure

A minimal runit service needs a `run` script:

```
sv/<name>/
├── run       # required: exec your process here (must not fork)
└── finish    # optional: called on exit with $1=exit_code
```

`run` and `finish` must be executable. The Dockerfile handles this automatically
for anything under `docker/sv/`.

## Useful commands

```bash
sv status <name>   # check if running
sv restart <name>  # restart
sv stop <name>     # stop (runit won't restart until you sv start it)
sv start <name>    # start
```

## Notes

- `update.sh` automatically restarts all running services after a binary upgrade.
- Personal/instance-specific services (e.g. telegram bot) should not be committed
  here — configure them on the volume after deploy.
