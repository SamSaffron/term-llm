# term-llm docs site

The docs site is built with Hugo.

## Local development

```bash
hugo server --source docs-site
```

## Production build

```bash
hugo --source docs-site --destination /tmp/term-llm-docs
npx --yes pagefind --site /tmp/term-llm-docs
```

The Pagefind step generates the static search index and UI assets under `/pagefind/`.

All documentation content should live in Markdown under `docs-site/content/`.
