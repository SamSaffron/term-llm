---
title: "Installation"
weight: 2
description: "Install term-llm for Mac (recommended on macOS), or use Homebrew, the Linux/macOS shell installer, Windows PowerShell, or release archives."
kicker: "Install"
source_readme_heading: "Installation"
featured: true
next:
  label: Provider setup
  url: /getting-started/providers-and-setup/
---
### macOS app (recommended)

[Download term-llm for Mac](https://term-llm.com/mac/download), the recommended
macOS install. It is a native app, Developer ID signed and notarized, with a
universal build for **Apple silicon and Intel**. Requires **macOS 14 or later**.

1. Open the downloaded zip.
2. Move **term-llm.app** to **Applications**.
3. Open **term-llm.app**.

On first run, the app installs and manages the term-llm CLI in its own managed
location, linked into `~/.local/bin` for use in Terminal. It runs the local
`term-llm serve web` service with start-at-login, and provides a native window
and menu bar for the browser workspace and chat. The app keeps its managed CLI
updated from GitHub releases and updates itself automatically via Sparkle.

If a fresh macOS shell cannot find `term-llm`, add `~/.local/bin` to your PATH
in `~/.zprofile`:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zprofile
```

Open a new Terminal window to pick up the change, or run `source ~/.zprofile`
in your current Zsh session.

Prefer a terminal-only install, or using an older macOS version? [Homebrew](#homebrew),
the [shell installer](#linuxmacos-shell-installer), and [release archives](https://github.com/term-llm/term-llm/releases)
remain supported on macOS.

### Supported release platforms

Release archives support **Linux/macOS** on **amd64/arm64** and **Windows** on
**amd64**. They include the browser workspace; Go and Node.js are only needed
for source builds. You can also [download an archive and its checksums](https://github.com/term-llm/term-llm/releases)
instead of using an installer.

### Linux/macOS (shell installer)

Requires `curl`, `tar`, `gzip`, `awk`, and a SHA-256 utility (`sha256sum` or `shasum`).
The installer verifies the release archive against its published
checksum before installation and does not run the installed binary. See
[download integrity and trust](/reference/version-and-updates/#download-integrity-and-trust)
for the limits of this verification.

```bash
curl -fsSL https://raw.githubusercontent.com/term-llm/term-llm/main/install.sh | sh
```

Or with options:

```bash
curl -fsSL https://raw.githubusercontent.com/term-llm/term-llm/main/install.sh | sh -s -- --version v0.1.0 --install-dir ~/bin
```

### Windows (PowerShell)

Requires PowerShell 5.1 or later. The installer verifies the Windows ZIP's
published SHA-256 checksum before installing `term-llm.exe`. Older releases may
not include Windows builds.

Windows supports `ask`, `exec`, `chat`, tools, sessions, and `serve web`/`serve api`.
An explicit `SHELL` overrides automatic selection; otherwise it uses PowerShell 7,
Windows PowerShell, then `COMSPEC`. Bash syntax is not automatically portable.
Service installation and Windows ARM64 are unsupported.

```powershell
irm https://raw.githubusercontent.com/term-llm/term-llm/main/install.ps1 | iex
```

For a downloaded installer, choose a different destination with:

```powershell
.\install.ps1 -InstallDir C:\tools\term-llm
```

The default is `%LOCALAPPDATA%\term-llm\bin`. If it is not on `PATH`, the installer
prints a command to add it. Open a new terminal after changing `PATH`.

### Homebrew

```bash
brew install term-llm/tap/term-llm
```

Homebrew installs the correct release binary for Apple Silicon or Intel macOS. Future stable releases are available through `brew upgrade term-llm`.

### Build from source

Source builds require the Go version declared in `go.mod`, Node.js 24 or newer, npm, and Make:

```bash
git clone https://github.com/term-llm/term-llm
cd term-llm
make build
./term-llm version
```

`make build` generates the embedded web UI and packages its readable source archive before compiling the Go binary. Those generated bundles are not checked into Git, so plain `go build` from a fresh checkout is not sufficient. Use the one-line installer for a self-contained release binary without source-build dependencies.

On macOS, both `make` and `make build` sign the native macOS binary with the
identifier `com.term-llm.cli`. Signing identity is selected in this order:

1. A nonempty `CODESIGN_IDENTITY` environment variable or Make command-line override.
2. A user-local, plain-text `codesign-identity` file under
   `$XDG_CONFIG_HOME/term-llm/` when `XDG_CONFIG_HOME` is absolute, otherwise
   `~/.config/term-llm/`.
3. The existing binary's certificate authority, read **before** rebuilding.
4. Ad-hoc signing (`-`) when nothing else is configured, so a fresh clone builds
   without a signing certificate.

To opt this machine into certificate signing, choose an existing identity from
`security find-identity -v -p codesigning` and save its exact name or fingerprint
as the file's only line. For example, with the default config location:

```sh
mkdir -p ~/.config/term-llm
printf '%s\n' 'Apple Development: Your Name (CERTIFICATE_ID)' > ~/.config/term-llm/codesign-identity
chmod 600 ~/.config/term-llm/codesign-identity
make
codesign --verify --strict ./term-llm
codesign -dr - ./term-llm
```

The file is outside the checkout, applies to other clones built by this user,
and is read as **data**, never sourced as shell code. An empty, malformed or
unreadable configured file fails the build; an unavailable certificate or signing
failure also fails rather than silently falling back to ad-hoc signing. Use an
explicit `CODESIGN_IDENTITY=- make build` to request ad-hoc signing deliberately.
No keys or certificates are created or imported by the build. Because this
personal identity also signs other checkouts, build untrusted branches or pull
requests with `CODESIGN_IDENTITY=- make build` so they do not inherit the trusted
binary's signing identity and existing macOS privacy grants.

`make check-changed` deliberately uses ad-hoc signing for its verification build:
its isolated HOME hides the user's keychain. Normal `make build` retains the
certificate-selection and fail-closed behavior above.

Certificate signing gives successive builds a consistent identity under the same
certificate and identifier; it does not grant macOS privacy permissions,
notarize the binary, or guarantee that permissions survive certificate changes.
After changing signing identity, restart already-running term-llm processes and,
if required, refresh their specific permission entries in System Settings.
Ad-hoc signing binds the designated requirement to a particular binary hash and
may require new permission grants after rebuilding. Direct `go build` bypasses
this Make signing policy. Cross-builds to non-macOS targets and builds on other
operating systems do not use this signing step.

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
