---
title: "Providers and setup"
weight: 3
description: "Connect one model, run a first question, and choose a default for your terminal and browser workflows."
kicker: "Providers"
featured: true
next:
  label: Choose a workflow
  url: /guides/
---
term-llm runs on your machine; you choose where model inference happens. Start with **one** of the options below. You can add more providers and choose different models for individual agents later.

## Try without an API key

[OpenCode Zen](https://opencode.ai) offers access to supported free hosted models:

```bash
term-llm ask --provider zen "Explain git rebase in three sentences"
```

You should receive a short explanation. `--provider zen` explicitly selects the provider for this command; it does not change your saved default.

Zen is a third-party service. Free model availability, capacity, and limits can change, and paid models require a Zen API key. Your prompt is sent to Zen, not processed locally. If the default model is unavailable, inspect the current catalog:

```bash
term-llm models --provider zen
```

Choose an available model with `--provider zen:MODEL_ID`, replacing `MODEL_ID` with the catalog entry. If no free model is available, use another connection option below.

## Use a provider API key

For example, with Anthropic:

```bash
export ANTHROPIC_API_KEY=your-key
term-llm ask --provider anthropic "Explain git rebase in three sentences"
```

Replace `your-key` with your own key. Do not commit credentials to a repository. OpenAI, Gemini, OpenRouter, and other providers have their own environment variables; see the [credential reference](/reference/providers-and-models/#credentials).

Provider API usage is billed by the provider. Installing term-llm does not include paid model access.

## Use a supported subscription

For an eligible ChatGPT account:

```bash
term-llm ask --provider chatgpt "Explain git rebase in three sentences"
```

Follow the browser authentication flow on first use. Available models and limits depend on your account. Other supported integrations include [GitHub Copilot](/reference/provider-setup-details/#option-12-use-github-copilot), [Grok subscription OAuth](/reference/provider-setup-details/#grok-subscription-oauth), and [Claude Code](/reference/provider-setup-details/#option-11-use-claude-code-claude-bin); each has its own setup requirements.

## Use a local model

Start your [Ollama](https://ollama.com) or [LM Studio](https://lmstudio.ai) server and load a model first. Then list the models it exposes:

```bash
term-llm models --provider ollama
# Or: term-llm models --provider lmstudio
```

Use the returned model ID:

```bash
term-llm ask --provider ollama:MODEL_ID "Explain git rebase in three sentences"
```

Replace `MODEL_ID` with an installed model. Model capability and your hardware determine which workflows work well; agent tasks need a model with suitable tool support.

Local inference does not automatically make every feature local. Search, MCP tools, media generation, and Guardian review may contact other services. Check those routes before sending sensitive material. See [local endpoint configuration](/reference/provider-setup-details/#option-10-use-local-llms-ollama-lm-studio) and the [Guardian privacy note](/reference/built-in-tools/#approval-modes).

## Save your preferred provider

On an interactive first run, term-llm can guide you through provider setup. You can also set your default in `$XDG_CONFIG_HOME/term-llm/config.yaml` (normally `~/.config/term-llm/config.yaml`). For example:

```yaml {title="config.yaml"}
default_provider: zen
```

Merge that setting into an existing configuration rather than replacing the whole file. Substitute `anthropic`, `chatgpt`, or another configured provider if you chose a different option.

After saving a default, you can omit `--provider`:

```bash
term-llm chat
term-llm serve web
```

`chat` starts the terminal interface. `serve web` starts the browser interface and prints its URL and authentication instructions. Start them separately; each remains running until you stop it.

## Check your setup

```bash
term-llm providers --configured
term-llm models --provider zen
```

Replace `zen` with your chosen provider. If a request fails:

- **Authentication error:** verify the provider name, key, or subscription login.
- **Model unavailable:** list the current catalog and choose an available model.
- **Local connection refused:** start your local model server and check its endpoint.
- **Rate limit or capacity error:** wait, or switch to another configured provider.

For specific settings, see [Provider setup details](/reference/provider-setup-details/). For reasoning controls and model capabilities, see [Providers and models](/reference/providers-and-models/).

<details class="legacy-provider-links">
<summary>Looking for a previous provider setup section?</summary>

These detailed instructions now live in the reference. Existing section links are kept here.

<p id="option-1-try-it-free-with-zen" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-1-try-it-free-with-zen">Option 1: Try it free with Zen →</a></p>
<p id="option-2-use-api-key" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-2-use-api-key">Option 2: Use API key →</a></p>
<p id="option-3-use-chatgpt-pluspro-subscription" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-3-use-chatgpt-pluspro-subscription">Option 3: Use ChatGPT (Plus/Pro subscription) →</a></p>
<p id="grok-subscription-oauth" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#grok-subscription-oauth">Grok subscription OAuth →</a></p>
<p id="option-4-use-xai-public-developer-api-grok" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-4-use-xai-public-developer-api-grok">Option 4: Use xAI public developer API (Grok) →</a></p>
<p id="option-5-use-venice" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-5-use-venice">Option 5: Use Venice →</a></p>
<p id="option-6-use-near-ai-cloud" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-6-use-near-ai-cloud">Option 6: Use NEAR AI Cloud →</a></p>
<p id="option-7-use-sambanova-cloud" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-7-use-sambanova-cloud">Option 7: Use SambaNova Cloud →</a></p>
<p id="option-8-use-openrouter" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-8-use-openrouter">Option 8: Use OpenRouter →</a></p>
<p id="model-discovery" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#model-discovery">Model Discovery →</a></p>
<p id="provider-discovery" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#provider-discovery">Provider Discovery →</a></p>
<p id="option-9-use-aws-bedrock" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-9-use-aws-bedrock">Option 9: Use AWS Bedrock →</a></p>
<p id="option-10-use-local-llms-ollama-lm-studio" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-10-use-local-llms-ollama-lm-studio">Option 10: Use local LLMs (Ollama, LM Studio) →</a></p>
<p id="option-11-use-claude-code-claude-bin" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-11-use-claude-code-claude-bin">Option 11: Use Claude Code (claude-bin) →</a></p>
<p id="option-12-use-github-copilot" class="legacy-provider-anchor"><a href="/reference/provider-setup-details/#option-12-use-github-copilot">Option 12: Use GitHub Copilot →</a></p>
</details>
