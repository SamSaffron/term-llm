---
title: "Text embeddings"
weight: 4
description: "Generate vector embeddings for search, RAG, clustering, semantic similarity, and retrieval workflows."
featured: true
kicker: "Embeddings"
source_readme_heading: "Text Embeddings"
next:
  label: MCP servers
  url: /guides/mcp-servers/
---
Use `term-llm embed` when you need vectors for retrieval, ranking, clustering, semantic similarity, or local pipeline glue.

```bash
term-llm embed "What is the meaning of life?"
```

Embeddings are numerical representations of text (arrays of floats) that capture semantic meaning. The `embed` command takes text input, calls an embedding API, and outputs vectors.

### Embed Input Methods

```bash
# Positional arguments (each embedded separately)
term-llm embed "first text" "second text" "third text"

# From stdin
echo "Hello world" | term-llm embed

# From files
term-llm embed -f document.txt
term-llm embed -f doc1.txt -f doc2.txt

# Mixed
term-llm embed "query text" -f corpus.txt
```

### Embed Flags

| Flag | Short | Description |
|------|-------|-------------|
| `--provider` | `-p` | Override provider (gemini, openai, jina, voyage, ollama) with optional model |
| `--file` | `-f` | Input file(s) to embed (repeatable) |
| `--format` | | Output format: `json` (default), `array`, `plain` |
| `--output` | `-o` | Write output to file |
| `--dimensions` | | Custom output dimensions (Matryoshka truncation) |
| `--task-type` | | Task type hint (e.g., RETRIEVAL_QUERY, RETRIEVAL_DOCUMENT, SEMANTIC_SIMILARITY) |
| `--similarity` | | Compare texts by cosine similarity instead of outputting vectors |

### Embed Output Formats

```bash
# JSON with metadata (default)
term-llm embed "hello"
# → {"model": "gemini-embedding-001", "dimensions": 3072, "embeddings": [...]}

# Bare JSON array(s) — one per input, for piping
term-llm embed "hello" --format array
# → [0.0023, -0.0094, 0.0156, ...]

# One number per line (single input only)
term-llm embed "hello" --format plain
# → 0.0023
#   -0.0094
#   ...

# Save to file
term-llm embed "hello" -o embeddings.json
```

### Similarity Mode

Compare texts by cosine similarity without manually handling vectors:

```bash
# Pairwise comparison
term-llm embed --similarity "king" "queen"
# → 0.834521

# Rank multiple texts against a query (first argument)
term-llm embed --similarity "What is AI?" "Machine learning is a subset of AI" "The weather is nice" "Neural networks process data"
# → 1. 0.891234  Machine learning is a subset of AI
#   2. 0.812456  Neural networks process data
#   3. 0.234567  The weather is nice
```

### Embed Examples

```bash
# Provider/model selection
term-llm embed "hello" -p openai                          # use OpenAI
term-llm embed "hello" -p openai:text-embedding-3-large   # specific model
term-llm embed "hello" -p gemini                           # use Gemini
term-llm embed "hello" -p jina                             # use Jina (free tier)
term-llm embed "hello" -p voyage                           # use Voyage AI
term-llm embed "hello" -p ollama:nomic-embed-text          # local Ollama

# Custom dimensions (Matryoshka)
term-llm embed "hello" --dimensions 256

# Gemini task type hints
term-llm embed "search query" --task-type RETRIEVAL_QUERY -p gemini
term-llm embed -f doc.txt --task-type RETRIEVAL_DOCUMENT -p gemini
```

### Embedding Providers

| Provider | Default Model | Dimensions | Environment Variable | Free Tier |
|----------|--------------|------------|---------------------|-----------|
| Gemini (default) | `gemini-embedding-001` | 3072 (128–3072) | `GEMINI_API_KEY` | Yes |
| OpenAI | `text-embedding-3-small` | 1536 (customizable) | `OPENAI_API_KEY` | No |
| [Jina AI](https://jina.ai/embeddings/) | `jina-embeddings-v3` | 1024 (customizable) | `JINA_API_KEY` | Yes (10M tokens) |
| [Voyage AI](https://voyageai.com) | `voyage-3.5` | 1024 (256–2048) | `VOYAGE_API_KEY` | No |
| Ollama | `nomic-embed-text` | 768 | — | Local |

Embedding providers use their own credentials, separate from text and image providers. The default provider is auto-detected from your LLM provider (Gemini users → Gemini, OpenAI users → OpenAI, Anthropic users → Voyage if configured, otherwise Gemini).

**Jina AI** is a great choice for getting started — sign up at [jina.ai/embeddings](https://jina.ai/embeddings/) for a free API key with 10M tokens, no credit card required.

**Voyage AI** is Anthropic's recommended embedding partner (acquired by MongoDB, Feb 2025). The API remains fully available.

## When to use it

Typical uses include:

- embedding a query and a corpus for retrieval
- comparing candidate text by cosine similarity
- generating vectors for a local RAG index
- piping embeddings into your own scripts or downstream tools
