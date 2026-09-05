---
title: "Shell integration"
weight: 11
description: "Alias and shell-completion setup for using term-llm comfortably from the command line."
kicker: "Ergonomics"
source_readme_heading: "Shell Integration (Recommended)"
next:
  label: Version and updates
  url: /reference/version-and-updates/
---
Commands run by term-llm don't appear in your shell history. To fix this, add a shell function that uses `--print-only` mode.

### Zsh

Add to `~/.zshrc`:

```zsh
tl() {
  local cmd
  cmd=$(command term-llm exec --print-only "$@") || return
  if [[ -n "$cmd" ]]; then
    print -s "$cmd"  # add to history
    eval "$cmd"
  fi
}
```

### Bash

Add to `~/.bashrc`:

```bash
tl() {
  local cmd
  cmd=$(command term-llm exec --print-only "$@") || return
  if [[ -n "$cmd" ]]; then
    history -s "$cmd"  # add to history
    eval "$cmd"
  fi
}
```

This `tl` function is an **exec-only helper**, not a general alias for `term-llm`. Use `tl "list files"` for the picker; continue to use `term-llm ask`, `term-llm chat`, and other full commands normally:

```bash
tl "find large files"
tl "install latest docker" -s      # with web search
tl "compress this folder"
```

The selected command is evaluated by your current shell, so shell state changes such as `cd` can persist and the command is added to history. Review the selection before pressing Enter. Adding `--auto-pick`/`-a` to this helper bypasses the picker and immediately evaluates the generated command; the outer `eval` is not protected by term-llm’s direct-execution autorun gate.
