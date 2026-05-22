---
title: "Search"
weight: 6
description: "Use web search in term-llm, choose external providers, and control native versus external search routing."
kicker: "Web search"
featured: true
next:
  label: Jobs
  url: /guides/job-runner/
---
## Search modes

When you use `-s` or `--search`, term-llm can answer with either:

- **native provider search** when the model backend supports it
- **external search tools** using the configured search provider plus page reading

Examples:

```bash
term-llm ask "latest node.js version" -s
term-llm exec "install latest docker" -s
```

## Force native or external behavior

```bash
term-llm ask "latest news" -s --native-search
term-llm ask "latest news" -s --no-native-search
```

Use this when you want consistency, debugging clarity, or to work around a provider’s native search behavior.

## Default providers

term-llm defaults to Exa MCP for external search and Jina Reader for page fetch:

```yaml
search:
  provider: exa_mcp
  fetch_provider: jina
  force_external: false

  exa_mcp:
    url: https://mcp.exa.ai/mcp # optional; this is the default
    api_key: ${EXA_API_KEY}    # optional; raises Exa MCP free-tier limits
```

With these defaults, `web_search` uses Exa's free remote MCP server, while `read_url` continues to use the default Jina markdown reader. You do not need an Exa API key unless you want higher Exa MCP limits.

## Configure external search

Common configurations:

Default: Exa MCP search + Jina page fetch:

```yaml
search:
  provider: exa_mcp
  fetch_provider: jina
```

Use Exa MCP for both search and page fetch:

```yaml
search:
  provider: exa_mcp
  fetch_provider: exa_mcp
```

Use another search provider while keeping Jina for `read_url`:

```yaml
search:
  provider: perplexity
  fetch_provider: jina
  perplexity:
    api_key: ${PERPLEXITY_API_KEY}
```

Provider-specific credentials:

```yaml
search:
  exa:
    api_key: ${EXA_API_KEY}

  exa_mcp:
    # url is optional; defaults to https://mcp.exa.ai/mcp
    # api_key is optional; set it to raise Exa MCP free-tier limits
    api_key: ${EXA_API_KEY}

  brave:
    api_key: ${BRAVE_API_KEY}

  google:
    api_key: ${GOOGLE_SEARCH_API_KEY}
    cx: ${GOOGLE_SEARCH_CX}
```

Available external providers:

| Provider | Credentials | Notes |
|---|---|---|
| DuckDuckGo | none | free fallback option |
| Exa | `EXA_API_KEY` | semantic search |
| Exa MCP | optional `EXA_API_KEY` | default, free remote MCP-backed search (`provider: exa_mcp`); can also back page fetch with `fetch_provider: exa_mcp` |
| Perplexity | `PERPLEXITY_API_KEY` | search API with concise answer-oriented results |
| Tavily | `TAVILY_API_KEY` | agent-oriented search |
| Brave | `BRAVE_API_KEY` | independent index |
| Google | `GOOGLE_SEARCH_API_KEY` + `GOOGLE_SEARCH_CX` | Custom Search |

Available fetch providers:

| Provider | Notes |
|---|---|
| Jina | default `read_url` implementation (`fetch_provider: jina`) |
| Exa MCP | use Exa MCP `web_fetch_exa` for `read_url` (`fetch_provider: exa_mcp`) |
| none | do not expose the external `read_url` tool (`fetch_provider: none`) |

`search.provider` and `search.fetch_provider` are independent. For example, `provider: exa_mcp` with `fetch_provider: jina` gives Exa MCP search results but keeps Jina for reading individual pages.

## Native versus external priority

Priority is:

1. CLI override: `--native-search` or `--no-native-search`
2. global config: `search.force_external: true`
3. provider config: `use_native_search: false`
4. default provider behavior

Example:

```yaml
search:
  force_external: true

providers:
  gemini:
    use_native_search: false
```

## Search in chat and agents

In chat mode:

- `Ctrl+S` toggles web search
- `/search` toggles web search

Agents can also enable search in their configuration:

```yaml
search: true
```

That exposes web search and page-reading tools to the agent.

## When to prefer each mode

Use **native search** when:

- you want the provider’s built-in grounding behavior
- you trust the provider’s integrated search product
- you want fewer moving pieces

Use **external search** when:

- you want one search stack across providers
- you need provider-independent behavior
- you want to debug the search pipeline more explicitly

## Related pages

- [Providers and models](/reference/providers-and-models/)
- [Configuration](/reference/configuration/)
- [Usage](/guides/usage/)
