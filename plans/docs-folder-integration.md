# Retire `docs/` into the documentation site

Status: **implemented**. Every file in `docs/` was routed to a real home and the
folder deleted, without dropping content that users or contributors need. The
design and constraints below are retained as the record of why each destination
was chosen. Outcome: two new guides, three merges into existing pages, four new
package READMEs plus an append, one archived proposal, and one deleted dead
script. `npm --prefix docs-site test` passes with 47 articles.

## What `docs/` actually is

It is not a documentation set. It is a drop zone: seven files, each written by the
feature PR that needed somewhere to record behaviour, none of them ever routed to
`docs-site/`. Nothing builds them, nothing links to them from the product, and
CI never reads them — `.github/workflows/deploy-docs-site.yml` only watches
`docs-site/**`, so these pages have never been published to term-llm.com.

The result is that four genuinely user-facing features are invisible on the docs
site: live voice, `term-llm doctor`, steering/Steer now, and `term-llm process
restart`. Meanwhile contributor rules (how to add a SQLite migration, how to add a
doctor check) sit in the same folder as an explicitly superseded design proposal.

Each file mixes up to three audiences in one document, which is why "move the
folder to the site" is not the answer. The split has to happen per section.

## Classification

| File | Lines | User/operator | Contributor | Historical |
| --- | --- | --- | --- | --- |
| `live-voice.md` | 296 | ~270 | — | — |
| `signals.md` | 240 | 1–136 | 137–240 | — |
| `sigusr2-redesign.md` | 249 | — | — | all (self-declared superseded) |
| `doctor.md` | 81 | 1–63 | 64–81 | — |
| `steering.md` | 63 | 1–22 | 23–63 | — |
| `web-ui-reliability.md` | 56 | 3–32 | 33–56 | — |
| `sqlite-migrations.md` | 54 | — | all | — |

## Constraints the site imposes

These are the reasons a naive `git mv docs/*.md docs-site/content/guides/` fails.
Every one was verified against the current site build.

1. **Navigation is explicit.** `docs-site/data/navigation.yaml` lists pages by
   name; `layouts/partials/docs-nav.html` renders only what is listed. A page
   absent from that file still builds but fails `npm --prefix docs-site test`
   at `Article missing from documentation navigation`.
2. **One `h1` per page.** Hugo renders `title` front matter as the `h1`, so the
   leading `# Title` line of each source file must be deleted, not carried over.
3. **Front matter is required.** `title`, `description`, `kicker`, usually
   `weight`; optional `featured`, `next: {label, url}`, `aliases`. The checker
   asserts a canonical URL and social image on every article.
4. **Links and fragments are validated.** Every local link and `#fragment` must
   resolve in the built site. The cross-references in these files
   (`signals.md` → `sigusr2-redesign.md`, `doctor.md` → `docs/sqlite-migrations.md`,
   `sqlite-migrations.md` → `docs/doctor.md`) must be rewritten or removed.
5. **Duplicate heading IDs fail.** Merging a section into an existing page can
   collide with a heading already there — check before pasting. Match the host
   page's heading level too: `guides/debugging.md` uses `###` throughout with no
   `##`, so pasted `##` sections would break its hierarchy and its table of
   contents (`markup.tableOfContents` runs from level 2 to 3).
6. **No aliases needed.** These pages were never public, so there are no URLs to
   preserve — unlike the `/guides/herdr/` precedent in `terminal-host-lifecycle.md`.

`scripts/check_steering_naming.sh` used to allowlist the literal path
`docs/steering.md`, which would have constrained where that file's legacy-alias
paragraphs could go. The script has been deleted: it was invoked by nothing —
not `.github/workflows/ci.yml`, not the `Makefile`, not AGENTS.md — so it was a
vocabulary audit that never actually ran. Its only remaining callers were
`docs/steering.md` itself and this plan.

## Destination map

Decisions settled: doctor becomes a section rather than its own page, live voice
diagnostics go to debugging, steering splits across the two guides that own its
surfaces, and contributor content goes to package READMEs. That leaves only two
new pages.

### A. Publish — new site pages

**`guides/live-voice.md`** ← `live-voice.md` lines 1–154, 222–239, 272–296
Nav: `Browser workspace` group, after `web-ui-and-api`. `weight: 8`,
`kicker: "Browser workspace"`, `featured: true`. This is the largest gap on the
site today and the only one a new user would notice.
Keep: intro and enablement, "Who owns what", the control lane, session switching,
the OpenAI and Gemini provider sections, "Behaviour worth knowing", HTTP surface.
Add a `next:` pointing at `webrtc-direct-routing` and update
`web-ui-and-api.md`'s `next` to hand off to this page instead.

**`guides/process-reload.md`** ← `signals.md` lines 1–136
Nav: `Web and deployment` group, after `background-services`. `weight: 18`,
`kicker: "Deploy agents"`. Title it for the task ("Reload a running process"),
not the signal. Covers the invocation table, the safe-point contract, and the
`term-llm process list/restart` CLI — all operator surface. Drop the
`sigusr2-redesign.md` link in the header.

### B. Merge into existing site pages

**`guides/debugging.md`** absorbs three things and becomes the troubleshooting
hub. It currently has only `###` headings under its `h1`, so its table of
contents is empty (`markup.tableOfContents` starts at level 2). Promote the four
existing `###` sections to `##` and add the new material at `##`. Hugo derives
heading IDs from text, not level, so no existing fragment changes. Final order:

1. `## Check the install with term-llm doctor` ← `doctor.md` lines 1–63, its
   own subsections demoted one level. First, because it is what you run before
   anything else.
2. `## Exercise automatic compaction locally` (promoted, unchanged)
3. `## Audit CLI provider wire traffic` (promoted, unchanged)
4. `## Trace agy-bin generation requests` (promoted, unchanged)
5. `## Live voice diagnostics` ← `live-voice.md` lines 155–221, both the Gemini
   diagnostics and the PCM artifact capture. They are gated behind
   `serve --debug`/`TERM_LLM_LIVE_DEBUG`, so they read as debugging, not feature
   documentation. The live voice guide links here.
6. `## Web interface reliability` ← `web-ui-reliability.md` lines 3–32: safe
   diagnostics, operational thresholds, idempotency replay boundary.
7. `## Debug logging` (promoted, unchanged)

**`reference/configuration.md`** ← `live-voice.md` lines 240–271, the `live.*`
table, as a new "Live voice" section. The guide keeps only the three-line
`live: enabled: true` snippet and links here, matching how `audio-generation.md`
and this reference already divide.

**`guides/usage.md`** ← `steering.md` lines 1–6 and the terminal half of 7–22:
Enter queues steering, the composer label change, the pending queue with
Up/Down/Delete, and `Esc` to steer right away. This is terminal core flow, which
is what `usage.md` owns.

**`guides/web-ui-and-api.md`** ← the web half of `steering.md` lines 7–22 (the
**Steer now** / **Steer all now** buttons beside Remove) plus the route list from
lines 35–48, minus the legacy-alias paragraph, next to the existing
commit/publish route tables.

**README "Common entry points"** → add Live voice. README already states that the
detailed docs live at term-llm.com and are authored in this repo, which is
precisely the claim `docs/` contradicts today.

### C. Move to contributor docs

`internal/terminal/README.md` is the existing precedent for package-local
contributor documentation, and AGENTS.md already points at it. Follow it rather
than growing AGENTS.md, which is deliberately a map.

- `sqlite-migrations.md` (all 54 lines) → **`internal/sqliteutil/README.md`**.
  Fix its closing `docs/doctor.md` reference to the new site URL.
- `doctor.md` lines 64–81 ("Adding a check") → **`internal/doctor/README.md`**.
- `signals.md` lines 137–240 ("How to add a mode", "Mode adapters",
  "Verification") → **`internal/restart/README.md`**. It documents
  `internal/restart.Coordinator` and the `scripts/test_sigusr2_*.py` proofs, so
  it belongs beside the code.
- `steering.md` lines 23–34, 49–63 (ownership/recovery, DB migration, review
  notes) → **`internal/session/README.md`**, minus the two `interject`
  paragraphs and minus "Review and verification notes", which is a PR report.
- `web-ui-reliability.md` lines 33–56 (storage migration/rollback, accessibility
  smoke checklist) → **`frontend/README.md`**, referenced from AGENTS.md's
  existing frontend section.

Then add one line to AGENTS.md "Common Change Paths" pointing at the four new
package READMEs, so they are discoverable the way `internal/terminal/README.md` is.

### D. Archive

`sigusr2-redesign.md` → **`plans/sigusr2-redesign.md`**. It is a closed-PR review
whose recommendation was explicitly superseded; `plans/` already holds exactly
this kind of document (`plans/live-voice-mode.md` opens the same way). Rewrite its
`signals.md` pointer to the published guide URL. Do not publish it.

## Inbound references to fix before deleting `docs/`

Verified with `rg 'docs/[a-z-]*\.md'`, excluding `tmp/`:

| Location | Current | Action |
| --- | --- | --- |
| `internal/tools/live_switch_session_test.go:108` | comment cites `docs/live-voice.md` | Update comment text — the only reference in shipped code |
| `docs/doctor.md:29`, `docs/sqlite-migrations.md:54` | mutual `docs/…` links | Resolved by the moves above |
| `plans/live-voice-mode.md:4` | `See docs/live-voice.md` | Optional: point at `/guides/live-voice/` |
| `plans/live-voice-session-control-plane.md:526,563,583` | `docs/live-voice.md` | Optional: same |

The `plans/` entries are historical records rather than live documentation, so
correcting them is a courtesy, not a blocker. `tmp/` also references these paths
but is gitignored scratch; ignore it.

## Execution

Five commits, each independently verifiable, so a failure never leaves the site
half-migrated.

1. **Contributor split.** Create the four `internal/*/README.md` files and
   `frontend/README.md`; add the AGENTS.md pointer. Delete the migrated sections
   from `docs/`. Verify: `make build`.
2. **Archive.** `git mv docs/sigusr2-redesign.md plans/` and fix its cross-link.
3. **New site pages.** Add the four pages with front matter, register each in
   `docs-site/data/navigation.yaml`, strip the source `h1`s, rewrite cross-links.
   Verify: `npm --prefix docs-site test`.
4. **Merges.** Config table, debugging thresholds, steering routes into their
   existing pages; check for heading-ID collisions. Verify: same site test.
5. **Delete.** `git rm -r docs/`, fix the remaining inbound references, and
   confirm `rg 'docs/[a-z-]+\.md' -g '!tmp'` returns nothing meaningful.

Verification for the site phases, per `docs-site/README.md`:

```sh
cd docs-site && npx playwright install chromium && cd ..
npm --prefix docs-site test
```

Root checks are not required for documentation-only commits, but phases 1 and 5
touch Go files, so run `make build` and `go test ./internal/tools` there.

## Editorial rules for the port

The site's voice is task-first and present-tense. These files were written as
implementation records, so porting is not copy-paste:

- Delete "Verification", "Review and verification notes", and "Findings checked
  against the source" sections. They describe what a PR proved, not what the
  software does. The contributor READMEs keep only the runnable proof commands.
- Delete release-transition notes ("retains … for **one release**", "Cleanup task
  for the following release"). They expire and will quietly go stale on a public
  page.
- Rewrite `internal/...` package citations out of user-facing pages; keep them in
  the package READMEs where they are useful.
- Lead each new page with what the reader can do, matching the existing
  `description` style in front matter.

## Decisions taken

1. **`doctor`** — a section in `guides/debugging.md`, not its own reference page.
2. **Live voice diagnostics** — moved to `guides/debugging.md`, not kept in the
   live voice guide. The guide links to them.
3. **Steering** — split across `guides/usage.md` (terminal) and
   `guides/web-ui-and-api.md` (web controls and the route table), rather than
   getting its own page.
4. **Contributor content** — package-local READMEs following
   `internal/terminal/README.md`, discoverable through a new "Package
   Documentation" section in AGENTS.md.

## Final state

| Destination | Content |
| --- | --- |
| `docs-site/content/guides/live-voice.md` | new, 218 lines |
| `docs-site/content/guides/process-reload.md` | new, 147 lines |
| `docs-site/content/guides/debugging.md` | +158: doctor, live voice diagnostics, web reliability |
| `docs-site/content/reference/configuration.md` | +37: the `live.*` table and its notes |
| `docs-site/content/guides/web-ui-and-api.md` | +32: Steer now controls and the steering routes |
| `docs-site/content/guides/usage.md` | +12: terminal steering |
| `internal/sqliteutil/README.md` | migration rules (git-detected rename of the original) |
| `internal/restart/README.md` | new, 108 lines: coordinator, adapters, acceptance proofs |
| `internal/doctor/README.md` | new: adding a check |
| `internal/session/README.md` | new: steering persistence and ownership |
| `frontend/README.md` | +27: storage rollback, accessibility checklist, payload baseline |
| `plans/sigusr2-redesign.md` | archived proposal, cross-link repointed |
| deleted | `docs/` (7 files) and `scripts/check_steering_naming.sh` |

Dropped deliberately: the "Verification"/"Review and verification notes" sections
of `steering.md`, the legacy `/interrupt` alias and payload-baseline release
notes, and internal Go citations in user-facing prose. The `Options.Observer`
progress contract moved to `internal/doctor/README.md` rather than being lost.
