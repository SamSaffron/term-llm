Managed term-llm agent runtime image asset.

This directory is written by `term-llm contain image sync`.
Fork it if you want custom image behavior.

The selected Dockerfile installs `term-llm` with the official release installer:

    curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh

By default it installs the latest release. To pin a specific release tag during
build, set `TERM_LLM_VERSION` in the workspace `.env`.

On first boot, if `TERM_LLM_CHATGPT_OAUTH_JSON_B64` is present in the
container environment, the entrypoint decodes it once into
`/home/agent/.config/term-llm/chatgpt_oauth.json` with mode `0600`. This lets the
agent template bootstrap ChatGPT OAuth credentials without baking them into the
image.

This directory is a complete Docker build context. It contains:

    Dockerfile              # legacy Arch entrypoint for existing workspaces
    Dockerfile.arch
    Dockerfile.fedora
    entrypoint.sh
    bootstrap/bootstrap.yaml
    bootstrap/system.md
    bootstrap/soul.md
    bootstrap/services/
    bootstrap/skills/          # default skills: jobs, memory, self, widgets
    bootstrap/scripts/

The image bakes those bootstrap files into `/opt/term-llm/bootstrap`.
`bootstrap.yaml` is rendered to the live agent's `agent.yaml` on first boot.

If a container mounts a static `/seed` directory containing `bootstrap.yaml`, that
seed directory replaces the image bootstrap source for first boot only. There is
no `/seed/agents/<name>` convention in the managed image.

On first boot, bootstrap files are rendered/copied into the persistent `/home/agent`
volume. A default `/home/agent/.config/term-llm/init.sh` is written if one does not
already exist. Future boots ignore `/seed` and image bootstrap files, then run
that persistent init script. PID 1/runit supervision stays root for service
installation under `/etc`, but the Web UI, jobs service, bootstrap jobs,
interactive shells, and normal agent work run as the non-root `agent` user with
passwordless `sudo` available for explicit privilege escalation.
