# Embedded frontends

- Chat entry is `src/main.tsx`; Hub views/styles live in `src/hub/`, Hub transport in `src/api/hub-client.ts`.
- Raw network transport stays in `src/api/`; markdown sanitization is `src/domain/markdown.ts`, hooks in `markdown-hooks.ts`, with rendering cases in `rendering.test.ts`.
- `src/api/generated/types.ts` comes from `make api-types`; edit its Go registry instead of the output.
- Follow [README.md](README.md) for asset hashing, separate Hub build, caching, storage privacy, and accessibility checks. Keep Hub assets out of the chat service worker.
- Author under `frontend/`, never generated `internal/serveui/static/dist`; the directory is embedded and versions hash its contents.
- Static shell assets outside dist require checking `internal/serveui/embed.go`, `static/index.html`, `static/sw.js`, and `embed_test.go`.
- Format with `npm --prefix frontend run format`; then run `format:check`, `lint`, `typecheck`, `test`, and `make frontend`.
- CI checks formatting and linting but never rewrites files; keep lint suppressions narrow and documented rather than disabling rules inline.
