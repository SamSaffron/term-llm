---
title: "Quickstart"
weight: 1
featured: true
description: "Install term-llm, choose a provider, and run the first useful commands."
kicker: "First run"
next:
  label: Provider setup
  url: /getting-started/providers-and-setup/
---
## Install it

```bash
curl -fsSL https://raw.githubusercontent.com/samsaffron/term-llm/main/install.sh | sh
```

Or with `go install`:

```bash
go install github.com/samsaffron/term-llm@latest
```

## Pick a provider

The fastest zero-friction option is Zen:

```bash
term-llm exec --provider zen "list files"
term-llm ask --provider zen "explain git rebase"
```

If you already use a provider directly, export its API key first:

```bash
export ANTHROPIC_API_KEY=your-key
# or OPENAI_API_KEY / GEMINI_API_KEY / OPENROUTER_API_KEY / XAI_API_KEY
```

## Try the core modes

```bash
term-llm exec "find all go files modified today"
term-llm ask "What is the difference between TCP and UDP?"
term-llm chat
```

## Continue with the docs

- [Installation](/getting-started/installation/)
- [Providers and setup](/getting-started/providers-and-setup/)
- [Usage guide](/guides/usage/)
- [Configuration reference](/reference/configuration/)
