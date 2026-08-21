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

## Current release compatibility note

Project-aware `term-llm serve web` is enabled by default. Read-only/unsupported session stores and first startup at filesystem root automatically fall back to the legacy UI with a warning; explicit `--projects` makes either condition a startup error, while `--no-projects` opts into legacy mode. Old Web UI assets using `use_default_workspace` are translated to the usable bootstrap project for one compatibility release and logged as deprecated. Old `/v1/worktrees` routes likewise remain startup-repository aliases and return deprecation headers. A `refresh_required` response instructs the browser to clear the term-llm shell cache, update its service worker, and reload; manually hard-refresh if browser policy blocks that recovery.
