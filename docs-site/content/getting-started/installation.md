---
title: "Installation"
weight: 2
description: "Install term-llm with the one-liner, a release archive, or a local source build."
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

### Build from source

Source builds require Go, Node.js 24 or newer, npm, and Make:

```bash
git clone https://github.com/samsaffron/term-llm
cd term-llm
make build
./term-llm version
```

`make build` generates the embedded web UI before compiling the Go binary. Those generated bundles are not checked into Git, so plain `go build` from a fresh checkout is not sufficient. Use the one-line installer for a self-contained release binary without source-build dependencies.

### Shell completions

Generate completion scripts for Bash, Zsh, Fish, or PowerShell with `term-llm config completion`. The easiest setup is to let term-llm install the script in the shell's standard user location:

```sh
term-llm config completion bash --install
term-llm config completion zsh --install
term-llm config completion fish --install
term-llm config completion powershell --install
```

The installer prints the destination and any shell-specific activation steps. Follow the printed instructions for Bash, Zsh, and PowerShell.

For Fish, the script is installed under `$__fish_config_dir/completions/term-llm.fish`. If `XDG_CONFIG_HOME` is absolute, this is `$XDG_CONFIG_HOME/fish/completions/term-llm.fish`; otherwise it is `~/.config/fish/completions/term-llm.fish`. Fish loads the file automatically in new sessions. Run the installer with the same `XDG_CONFIG_HOME` that Fish uses.

To enable completions only for the current Fish session without installing a file, run:

```fish
term-llm config completion fish | source
```

Without `--install`, the command writes the generated script to standard output so you can manage it yourself.
