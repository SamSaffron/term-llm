# Browser demo deployment

This directory owns the daily GitHub Actions deployment of
<https://demo.term-llm.com>. The demo is a static browser application: there is
no application backend, and the host only serves immutable release files plus a
pinned model.

The workflow is
[`.github/workflows/deploy-browser-demo.yml`](../../.github/workflows/deploy-browser-demo.yml).
It is not active until this change merges to `SamSaffron/term-llm:main`.
Activation is automatic after merge because the workflow is committed to
`main`; it needs no external job registration.

## Triggers and trust boundary

- Daily at `17:23 UTC` (`workflow_dispatch` is available for manual runs).
- Pushes to `main` that touch the demo, frontend, Go source, Makefile, build
  scripts, or this directory.
- Pull requests touching the same paths run the build and tests only.

The build/test job never receives secrets. The deploy job runs only when
`github.repository == 'SamSaffron/term-llm'` and the ref is
`refs/heads/main` for a push, schedule, or manual dispatch. Fork pull requests
and PRs therefore cannot reach the deployment host.

## Build inputs and reproducibility

The build runs on the current `main` checkout and deliberately rebuilds the real
production frontend before the demo bundle:

1. Go comes from `go.mod`; Node is 24, matching the docs deployment.
2. `make frontend-deps` installs `frontend/` and `npm ci` installs
   `experiments/browser-bonsai/`.
3. `make frontend` rebuilds the embedded frontend assets, then
   `experiments/browser-bonsai/build.sh` builds the real Go/WASM entrypoint,
   v86 bundle, web shell, and CPython overlay.
4. JavaScript tests (including the small-stack WASM startup check) and the demo
   Go tests run before a release is packaged. The real Linux guest test skips
   unless `LINUX_TEST_ASSETS` is supplied locally; CI never downloads or
   redistributes the upstream kernel/BIOS. CI also makes no GPU-inference claim.

### Pinned CPython runtime

`experiments/browser-bonsai/web/python-runtime.mjs` is the authoritative pin.
`ops/browser-demo/ensure-python-runtime.sh` resolves the ignored archive in this
order:

1. An existing archive, usually restored by `actions/cache` keyed by the pin.
2. The same-origin public copy at
   <https://demo.term-llm.com/runtime/python-root.tar.gz>, downloaded to a
   temporary directory and accepted only if its exact SHA-256 and byte count
   match the repository pin. The public metadata is corroborating only; it is
   never trusted on its own.
3. The deterministic pinned-Alpine APK recipe in
   `experiments/browser-bonsai/scripts/fetch-python-runtime.sh`.

The recipe result must also match the existing pin. If an APK URL disappears,
the cache or the verified public archive keeps scheduled builds working; a pin
change must be made deliberately.

### Model pin and range behavior

`experiments/browser-bonsai/web/inference.mjs` pins `MODEL_URL` and
`MODEL_BYTES`. `prepare-release.sh` copies those exact values into
`release.json`; the remote preflight requires the same path and size on the
origin before switching the document root. It never guesses from an arbitrary
`*.bin` file.

The origin smoke validates HTTP `206` responses with an exact `Content-Range`
for both a release asset and the pinned model. A valid 206 may omit
`Accept-Ranges` (Cloudflare does); `Accept-Ranges` on `HEAD` is logged as
advisory only. A `200` response to a range request fails and rolls back.

The upstream kernel/BIOS remain direct browser fetches from
`raw.githubusercontent.com/copy/v86` and `i.copy.sh`; the release preflight
rejects those bytes if they ever appear in a build.

## Release layout and atomic switch

The workflow packages one release under
`/var/www/term-llm-demo/releases/<UTC timestamp>-<source SHA>` with:

- deterministic gzip siblings for nginx `gzip_static`,
- `manifest.sha256` for every file,
- `release.json` with `gitSha`, `builtAt`, repository pin hashes, and the exact
  model path/size. This makes the deployed source identifiable.

`remote-deploy.sh` runs under `flock` and validates the tarball before
extraction: safe `./` paths only, regular files/directories only (no symlinks,
hardlinks, devices, or FIFOs), the full file manifest, critical WASM/v86/Python
files, the Python runtime pin, and the exact model path/size. It then creates the
new release, atomically replaces the `current` symlink, and verifies origin HTTPS
with `--resolve` (bypassing Cloudflare) and cache-busting headers. Any failure
restores the previous `current` symlink before exiting; a partially installed
release is removed when it is not current.

The deployment never edits nginx, TLS/ACME state, `models/`, the demo's ACME
webroot, or the hosting configuration. It never touches `/var/www/term-llm/`,
which the docs deployment owns with `rsync --delete`.

`cleanup-releases.sh` keeps the current release, the newest three workflow-owned
releases, and any direct child passed with `--protect`. The deploy script passes
the immediate rollback target so a manual rollback older than the newest three
cannot be deleted. Cleanup only deletes directories directly under `releases/`
whose names match the workflow's timestamp/SHA pattern; it refuses symlinked
release roots and never follows a path outside `releases/`. Existing manually
named POC releases are left alone.

Both GitHub Actions job concurrency and the host `flock` serialize deploys.

## Required secret and host verification

The workflow reuses the existing `DEPLOY_SSH_KEY` repository secret and the
existing root SSH target `143.198.76.107`. No new secret, certificate, DNS
record, or nginx change is required.

`known_hosts` pins the host's Ed25519 key:

```
SHA256:hFtk253jgXtR0xarGb8QujeDOippF1uE53pfMNn0/5c
```

`deploy.sh` refuses to connect if the file has anything other than that single
fingerprint, and SSH runs with `StrictHostKeyChecking=yes`,
`UserKnownHostsFile` set to this file, `GlobalKnownHostsFile=/dev/null`,
`BatchMode=yes`, and no password/keyboard-interactive fallback. The private key
is written to a `0600` runner-temporary file (or supplied by
`DEPLOY_SSH_KEY_FILE`), never printed, and removed in an `always()` step.

## Local verification

```sh
bash -n ops/browser-demo/*.sh
shellcheck ops/browser-demo/*.sh
go test ./ops/browser-demo
```

The tests use temporary directories and fake `ssh`/`scp`/`curl`/`nginx`
commands. They cover release packaging, symlink-swap and rollback, bounded
cleanup, exact model preflight, 206/Content-Range behavior, host-key pinning,
and rejected unpinned Python runtime bytes. They never contact the live host.
If `actionlint` is installed, run it as well; otherwise GitHub validates the
workflow when it is pushed.

## Operations

- Manual deploy: run the workflow with `workflow_dispatch` from `main`.
- Failed deploy: the job fails, the previous `current` symlink is restored, and
  the incomplete release is removed. Inspect the workflow log and the remote
  `releases/` directory before retrying.
- Disk pressure: `cleanup-releases.sh --root /var/www/term-llm-demo --keep 3
  --dry-run` previews deletions. The model, the current/rollback releases, and
  non-workflow release directories are never candidates.
- Roll back manually to a retained release with an atomic symlink swap:

  ```sh
  root=/var/www/term-llm-demo
  release=releases/<retained-id>
  ln -sfn "$release" "$root/.current.manual"
  mv -Tf "$root/.current.manual" "$root/current"
  ```

No live service, scheduled job, or credential is changed by this directory
until the workflow is merged and triggered by the repository schedule or a
manual dispatch.
