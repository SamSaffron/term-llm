---
title: "Quickstart"
weight: 1
featured: true
description: "Install term-llm, get a useful first answer, and choose the terminal or the full browser workspace."
kicker: "First run"
next:
  label: Connect your preferred provider
  url: /getting-started/providers-and-setup/
---
Install term-llm and [connect a provider](/getting-started/providers-and-setup/) for your first question. For Terminal commands, an interactive first-run setup wizard helps you choose one.

## 1. Install term-llm

### macOS

[Download term-llm for Mac](https://term-llm.com/mac/download) (recommended for
macOS 14+). Open the zip, move **term-llm.app** to **Applications**, and open it.
The app manages the CLI and starts the local web service for you. See the
[Mac app installation details](/getting-started/installation/#macos-app-recommended).

To use the CLI in Terminal, `~/.local/bin` must be on your PATH. If your shell
cannot find `term-llm`, add it to `~/.zprofile`:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zprofile
```

Open a new Terminal window, or run `source ~/.zprofile` in your current Zsh session.

Prefer a terminal-only macOS install? Homebrew is still supported:

```bash
brew install term-llm/tap/term-llm
```

The [shell installer](#linux) below and [release archives](https://github.com/term-llm/term-llm/releases)
also remain supported on macOS.

### Linux

```bash
curl -fsSL https://raw.githubusercontent.com/term-llm/term-llm/main/install.sh | sh
```

This downloads and executes the [shell installer](https://github.com/term-llm/term-llm/blob/main/install.sh).
Inspect it first if you prefer, or [download a release archive](https://github.com/term-llm/term-llm/releases).

### Windows

In PowerShell:

```powershell
irm https://raw.githubusercontent.com/term-llm/term-llm/main/install.ps1 | iex
```

This downloads and executes the [PowerShell installer](https://github.com/term-llm/term-llm/blob/main/install.ps1).
Inspect it first if you prefer. See [Windows installation details](/getting-started/installation/#windows-powershell)
for requirements and PATH instructions.

### Check the CLI

Published CLI releases include the browser interface, with no Node.js or frontend
build required. If you plan to use Terminal, confirm the binary is available:

```bash
term-llm version
```

You should see the installed version. If your shell cannot find `term-llm`,
check the Mac app PATH step above or follow your installer's PATH instructions.

## 2. Connect a provider and get a first answer

**Using the Mac app?** Open its workspace window for browser chat; you do not
need to start another web service. Choose a configured provider in the browser
workspace's **Settings → Model**. Provider credentials must be available to the
running service; exporting an API key in Terminal does not change the environment
of an already-running app-managed service. See [provider setup](/getting-started/providers-and-setup/)
for credential and configuration options.

**Using Terminal?** Choose a [provider API key, supported subscription, or local model](/getting-started/providers-and-setup/) before asking your first question. The default getting-started option is OpenRouter’s free model router. Create an [OpenRouter API key](https://openrouter.ai/keys) and set it in your shell:

```bash
export OPENROUTER_API_KEY=your-key
```

Replace `your-key` with your own key. Then run the command below in an interactive terminal. If you have no saved configuration, the setup wizard asks you to choose a provider and saves it as your default. Select OpenRouter to use `openrouter/free` for both the main and fast models, or follow the setup instructions for another provider. Saved provider and model choices are preserved.

```bash
term-llm ask "Explain git rebase in three sentences"
```

You should receive a short explanation of how rebase replays commits onto a new base. Wording varies by model. This request asks a question; it does not run `git rebase` or change your repository.

> OpenRouter receives your prompts and routes them to an available free model. Free usage has limits and model availability varies; other models and features may incur charges. You can also [use a local model](/getting-started/providers-and-setup/#use-a-local-model).

## 3. Choose your interface

Choose the Mac app window, a browser, or Terminal. For CLI installations, run the commands below separately.

### Open the browser workspace

**Mac app:** open the app's workspace window. The local web service is already
managed by the app, including start-at-login; skip the command below.

**CLI install:** start the web service yourself:

```bash
term-llm serve web
```

Open the URL printed in your terminal and follow the printed authentication instructions. Keep this terminal process running while you use the interface; press **Ctrl+C** to stop the server.

You get a complete workspace: saved conversations, project and worktree organization, model selection, tool activity, live file diffs, attachments, and an interactive shell. Some controls appear only when a project, attachment, or file change makes them relevant.

[Explore the browser workspace](/guides/web-ui-and-api/). For remote access, read the [authentication and deployment guidance](/guides/web-ui-and-api/#authentication) before exposing the server. Do not disable authentication on a public listener.

### Stay in your terminal

```bash
term-llm chat
```

Ask follow-up questions in a persistent terminal conversation. When you are ready to work with a repository, run this from its directory:

```bash
term-llm chat @codebase
```

Try asking: “Where does this application handle configuration?” Review workspace access requests before granting them. The quality of agent work depends on the chosen model and its tool support.

## 4. Try a real workflow

Choose a task that fits your work. These examples use your configured default provider.

**Review staged changes without editing them:**

```bash
term-llm ask @reviewer "review the staged changes"
```

Run it in a Git repository with staged changes. The built-in reviewer is read-only and git-aware. Expect observations tied to the code, not automatic fixes.

**Preview a targeted edit:**

```bash
term-llm edit "improve error handling" -f main.go --dry-run
```

Replace `main.go` with a file in your project. `--dry-run` previews the change without writing it to disk.

**Choose and run a command in plain English:**

```bash
term-llm exec "list files"
```

You’ll see an interactive picker with suggested commands and an explanation for each. Use **↑/↓** to highlight an option, **i** for more information, and **Enter** to run the selected command. Choose **“something else...”** to refine your request, or **Esc** to cancel.

For this example, suggestions might include `ls`, `ls -la`, or `ls -lh`. Check the command before selecting it: Enter runs it, rather than just copying it. If you want to print the selected command instead of running it, add `--print-only`.

## 5. Make it your own

- [Save a default provider](/getting-started/providers-and-setup/#save-your-preferred-provider) so you can omit `--provider`.
- [Choose an agent](/guides/agents/) for coding, review, research, or editing.
- [Understand approval modes](/reference/built-in-tools/#approval-modes) before giving agents broader tool access. Ordinary chat, ask, and web sessions default to Guardian-reviewed auto mode; use `--approval prompt` if you want human approval for unmatched actions.
- [Browse workflows](/guides/) for jobs, MCP tools, media, and deployment.
