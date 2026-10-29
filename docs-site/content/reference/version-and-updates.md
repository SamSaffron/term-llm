---
title: "Version and updates"
weight: 3
description: "Check the installed version, upgrade term-llm, and disable automatic update checks."
kicker: "Lifecycle"
source_readme_heading: "Version & Updates"
next:
  label: Usage tracking
  url: /reference/usage-tracking/
---
term-llm automatically checks for updates once per day and notifies you when a new version is available.

```bash
term-llm version       # Show version info
term-llm upgrade       # Upgrade to latest version
term-llm upgrade --version v0.2.0  # Install specific version
```

To disable update checks, set `TERM_LLM_SKIP_UPDATE_CHECK=1`.

## Download integrity and trust

The shell installer and `term-llm upgrade` fetch `checksums.txt` from the selected
GitHub release and require an exact SHA-256 match before extracting or installing
the archive. Missing, malformed, duplicate, or mismatched checksum entries for
the selected asset abort the operation. Downloads and redirects must use HTTPS.
Archives are limited to
128 MiB, checksum manifests to 1 MiB, extracted binaries to 256 MiB, and total
decompressed archive data to 512 MiB. The shell installer's conservative file
resource limits halve these caps on shells using 512-byte `ulimit` blocks,
including typical `curl ... | sh` invocations: 64 MiB archives, 512 KiB manifests,
128 MiB binaries, and 256 MiB decompressed data. Only the top-level regular
`term-llm` file is installed. The shell installer does not execute the downloaded
binary.

These are integrity checks, **not publisher authentication**. The archive and
checksum manifest share the same GitHub release trust boundary; someone able to
replace both can publish a matching malicious binary. Neither consumer currently
verifies a release signature or build attestation. Installing with the one-liner
also trusts the installer script fetched from GitHub. Releases without a usable
checksum manifest cannot be installed through these paths.

## Filesystem attribution compatibility note

`file_changes` now contains only trusted direct transitions and declaration-verified shell `transform`/`generate` transitions. Existing clients can continue parsing the additive response, but `affected_paths` no longer attributes shell effects. Materializations, observations, claim diagnostics, provenance, baseline state, and content availability are returned separately. Historical shell/custom/client/MCP rows are conservatively labeled unverified and excluded from current Agent Changes totals.

## Current release compatibility note

Project-aware `term-llm serve web` is enabled by default. Read-only/unsupported session stores and first startup at filesystem root automatically fall back to single-workspace mode with a warning; explicit `--projects` makes either condition a startup error, while `--no-projects` deliberately selects single-workspace mode for dedicated agent deployments. Old Web UI assets using `use_default_workspace` are translated to the usable bootstrap project for one compatibility release and logged as deprecated. Old `/v1/worktrees` routes likewise remain startup-repository aliases and return deprecation headers. A `refresh_required` response instructs the browser to clear the term-llm shell cache, update its service worker, and reload; manually hard-refresh if browser policy blocks that recovery.
