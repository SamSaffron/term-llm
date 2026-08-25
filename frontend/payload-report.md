# Web UI payload report

Baseline SHA: `d9887abd2fe5f4f710176469b336e3da04d3c489`<br>
Route and feature set: direct `/ui/`, WebRTC disabled<br>Compression: served-equivalent gzip level 6; hypothetical Brotli quality 11

`scripts/measure_ui_payload.sh` measures a representative rendered production page with a fixed injected bootstrap, then reads the generated assets used by that page. `frontend/payload-baseline.json` and `payload-final.json` contain the raw reproducible results and final asset SHA-256 values. The legacy control concatenates and minifies the authored JS/CSS with the same esbuild minifier used by the new Vite build before applying identical compression. HTML is unchanged in that control.

Cold navigation counts only HTML, CSS, and JavaScript. Icon/manifest and the service-worker shell are reported separately. First-party totals contain rendered HTML plus authored application JS/CSS. The vendor total is kept separate: legacy marked + DOMPurify versus the final deterministic Preact/Signals/marked/DOMPurify vendor chunk. The current generated `dist/app.js` is 157,066 raw bytes; the tables below use that artifact, not the earlier smaller intermediate bundle.

## Cold navigation

| Category / compression | Legacy delivered | Legacy toolchain-minified | Final Preact | Final vs delivered | Final vs minified control |
|---|---:|---:|---:|---:|---:|
| First-party raw | 1,256,193 | 721,959 | 279,405 | −976,788 (−77.8%) | −442,554 (−61.3%) |
| First-party gzip-6 | 293,109 | 196,536 | 69,737 | −223,372 (−76.2%) | −126,799 (−64.5%) |
| First-party Brotli-11 | 251,510 | 163,029 | 59,731 | −191,779 (−76.3%) | −103,298 (−63.4%) |
| Vendor raw | 61,860 | 61,860 | 94,555 | +32,695 (+52.9%) | +32,695 (+52.9%) |
| Vendor gzip-6 | 20,841 | 20,841 | 32,285 | +11,444 (+54.9%) | +11,444 (+54.9%) |
| Vendor Brotli-11 | 18,890 | 18,890 | 29,298 | +10,408 (+55.1%) | +10,408 (+55.1%) |
| Combined raw | 1,318,053 | 783,819 | 373,960 | −944,093 (−71.6%) | −409,859 (−52.3%) |
| Combined gzip-6 | 313,950 | 217,377 | 102,022 | −211,928 (−67.5%) | −115,355 (−53.1%) |
| Combined Brotli-11 | 270,400 | 181,919 | 89,029 | −181,371 (−67.1%) | −92,890 (−51.1%) |
| First-party requests | 43 | 43 | 3 | −40 | −40 |
| Vendor requests | 2 | 2 | 1 | −1 | −1 |
| Combined requests | 45 | 45 | 4 | −41 | −41 |

The larger vendor line is the framework cost: Preact and Signals were added while marked and DOMPurify remain. The combined result remains below both the delivered legacy payload and the same legacy application minified by the new toolchain.

## Lazy rich content

The comparable rich-content scenario loads KaTeX and the dark highlight theme. Glyph-dependent font requests are excluded from both sides because their count depends on rendered equations; generated WOFF2 files remain embedded and cacheable.

| Compression | Legacy lazy vendors | Final lazy chunks | Delta |
|---|---:|---:|---:|
| Raw | 425,986 | 324,528 | −101,458 (−23.8%) |
| gzip-6 | 124,260 | 94,451 | −29,809 (−24.0%) |
| Brotli-11 | 104,657 | 78,924 | −25,733 (−24.6%) |
| Requests | 5 | 6 | +1 |

The six deterministic final requests are `rich-highlight.js`, highlight JS/CSS, `rich-katex.js`, and KaTeX JS/CSS. The split entry chunks reflect the actual independent dynamic imports. Vite emits only WOFF2 KaTeX fonts.

## Service-worker shell

| Compression | Legacy precache | Final precache | Delta |
|---|---:|---:|---:|
| Raw | 1,569,542 | 665,833 | −903,709 (−57.6%) |
| gzip-6 | 601,124 | 396,458 | −204,666 (−34.0%) |
| Brotli-11 | 558,910 | 383,653 | −175,257 (−31.4%) |
| Requests | 46 | 5 | −41 |

The shell includes manifest, icon, application CSS/JS, and the initial vendor chunk. The WebRTC chunk is added only when that server feature is enabled. KaTeX/highlight remain opportunistically cached on first use; unversioned lazy chunks use network-first service-worker handling so a deployment cannot execute stale chunk code.
