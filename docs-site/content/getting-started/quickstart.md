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
Install term-llm and [connect a provider](/getting-started/providers-and-setup/) for your first question. On an interactive first run, the setup wizard helps you choose one.

## 1. Install term-llm

On macOS with Homebrew:

```bash
brew install samsaffron/tap/term-llm
```

Or use the release installer on a supported platform:

```bash
curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh
```

The shell command downloads and executes the [installer script](https://github.com/samsaffron/term-llm/blob/main/install.sh). Inspect it first if you prefer, or [download a release archive](https://github.com/samsaffron/term-llm/releases). Published releases are self-contained: the browser interface is included, with no Node.js or frontend build required.

Confirm the binary is available:

```bash
term-llm version
```

You should see the installed version. If your shell cannot find `term-llm`, follow the installer’s PATH instructions or see [installation](/getting-started/installation/).

## 2. Connect a provider and get a first answer

Choose a [provider API key, supported subscription, or local model](/getting-started/providers-and-setup/) before asking your first question. The default getting-started option is OpenRouter’s free model router. Create an [OpenRouter API key](https://openrouter.ai/keys) and set it in your shell:

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

Both interfaces use the same underlying runtime and provider configuration. Try either one; run these commands separately.

### Open the browser workspace

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
