# Plans

Design and implementation plans. A plan records intent and constraints; code and
`docs-site/content/` are the source of truth for shipped behaviour.

## Status

Every `plans/*.md` and `plans/archive/*.md` (except this README) carries a
`Status: <value>` line within its first 15 lines, right after the title. Prose
about the status goes in a following paragraph, not on the same line.

| Status | Meaning |
|---|---|
| `proposed` | Not started; may be rejected or rewritten. |
| `in-progress` | Partly shipped; the note says which phases remain. |
| `implemented` | Shipped. Deviations from the text are listed in the note. |
| `superseded` | Replaced by another plan or design; the note names it. |
| `historical` | A review or proposal kept only as a record. |

## Lifecycle

- Active plans (`proposed`, `in-progress`) live in `plans/`.
- When a plan becomes `implemented`, `superseded` or `historical`, `git mv` it to
  `plans/archive/` and update links (`plans/archive/<name>.md`).
- Before marking a plan `implemented`, audit it against the code: confirm the key
  symbols/files exist and list deviations in the status note.
- Ratchet baselines in use today live in `ledger/`, not here.

## Lint

```sh
make plans-lint        # or: scripts/lint-plans.sh
```

Checks: status present and valid; archived plans are not `proposed`/`in-progress`;
active plans are not archived-status; no stale
`plans/<name>.md` links to files that moved or do not exist.
