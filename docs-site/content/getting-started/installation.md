---
title: "Installation"
weight: 2
description: "Install term-llm with the one-liner, `go install`, or a local source build."
kicker: "Install"
source_readme_heading: "Installation"
featured: true
next:
  label: Provider setup
  url: /getting-started/providers-and-setup/
---
### One-liner (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh
```

Or with options:

```bash
curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh -s -- --version v0.1.0 --install-dir ~/bin
```

### Go install

```bash
go install github.com/samsaffron/term-llm@latest
```

### Build from source

```bash
git clone https://github.com/samsaffron/term-llm
cd term-llm
go build
```

### Shell completions

Generate completion scripts for Bash, Zsh, Fish, or PowerShell with `term-llm config completion`. The easiest setup is to let term-llm install the script in the shell's standard user location:

```sh
term-llm config completion bash --install
term-llm config completion zsh --install
term-llm config completion fish --install
term-llm config completion powershell --install
```

The installer prints the destination and any shell-specific activation steps. Follow the printed instructions for Bash, Zsh, and PowerShell.

For Fish, the script is installed under `$__fish_config_dir/completions/term-llm.fish`: `$XDG_CONFIG_HOME/fish/completions/term-llm.fish` when `XDG_CONFIG_HOME` is an absolute path, or `~/.config/fish/completions/term-llm.fish` otherwise. Fish loads it automatically in new shell sessions. Run the installer from an environment with the same `XDG_CONFIG_HOME` that Fish uses.

To enable completions only for the current Fish session without installing a file, run:

```fish
term-llm config completion fish | source
```

Without `--install`, the command writes the generated script to standard output so you can manage it yourself.
