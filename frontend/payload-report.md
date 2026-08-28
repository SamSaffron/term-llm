# Web UI payload report

Baseline SHA: `d9887abd2fe5f4f710176469b336e3da04d3c489`<br>
Route and feature set: direct `/ui/`, WebRTC disabled<br>
Compression: served-equivalent gzip level 6; hypothetical Brotli quality 11

`scripts/measure_ui_payload.sh` measures a representative rendered production page with a fixed injected bootstrap. Initial requests are derived from the rendered HTML and the generated entry module's static import graph; service-worker cache candidates are parsed from `SHELL_ASSETS`. `frontend/payload-baseline.json` and `payload-final.json` contain the raw reproducible results and final asset SHA-256 values. The legacy control concatenates and minifies the authored JS/CSS with the same esbuild minifier used by the Vite build before applying identical compression. HTML is unchanged in that control.

Cold navigation counts only HTML, CSS, and JavaScript. Icon/manifest and service-worker cache candidates are reported separately. First-party totals contain rendered HTML plus authored application JS/CSS. The vendor total is separate: legacy marked + DOMPurify versus the final Preact/Signals/marked/DOMPurify vendor chunk. The current generated `dist/app.js` is 171,940 raw bytes.

## Cold navigation

| Category / compression | Legacy delivered | Legacy toolchain-minified | Final Preact | Final vs delivered | Final vs minified control |
|---|---:|---:|---:|---:|---:|
| First-party raw | 1,256,193 | 721,959 | 305,907 | −950,286 (−75.6%) | −416,052 (−57.6%) |
| First-party gzip-6 | 293,109 | 196,536 | 76,534 | −216,575 (−73.9%) | −120,002 (−61.1%) |
| First-party Brotli-11 | 251,510 | 163,029 | 65,295 | −186,215 (−74.0%) | −97,734 (−59.9%) |
| Vendor raw | 61,860 | 61,860 | 94,555 | +32,695 (+52.9%) | +32,695 (+52.9%) |
| Vendor gzip-6 | 20,841 | 20,841 | 32,285 | +11,444 (+54.9%) | +11,444 (+54.9%) |
| Vendor Brotli-11 | 18,890 | 18,890 | 29,298 | +10,408 (+55.1%) | +10,408 (+55.1%) |
| Combined raw | 1,318,053 | 783,819 | 400,462 | −917,591 (−69.6%) | −383,357 (−48.9%) |
| Combined gzip-6 | 313,950 | 217,377 | 108,819 | −205,131 (−65.3%) | −108,558 (−49.9%) |
| Combined Brotli-11 | 270,400 | 181,919 | 94,593 | −175,807 (−65.0%) | −87,326 (−48.0%) |
| First-party requests | 43 | 43 | 3 | −40 | −40 |
| Vendor requests | 2 | 2 | 1 | −1 | −1 |
| Combined requests | 45 | 45 | 4 | −41 | −41 |

The larger vendor line is the framework cost: Preact and Signals were added while marked and DOMPurify remain. The combined result remains below both the delivered legacy payload and the same legacy application minified by the new toolchain.

## Lazy rich content

The comparable rich-content scenario loads KaTeX and the dark highlight theme. Final assets are derived from generated `rich-*`, `highlight*`, and `katex*` JS/CSS under `dist/chunks`. Glyph-dependent font requests are excluded from both sides because their count depends on rendered equations; generated WOFF2 files remain embedded and cacheable.

| Compression | Legacy lazy vendors | Final lazy chunks | Delta |
|---|---:|---:|---:|
| Raw | 425,986 | 324,528 | −101,458 (−23.8%) |
| gzip-6 | 124,260 | 94,451 | −29,809 (−24.0%) |
| Brotli-11 | 104,657 | 78,924 | −25,733 (−24.6%) |
| Requests | 5 | 6 | +1 |

The six final requests are `rich-highlight.js`, highlight JS/CSS, `rich-katex.js`, and KaTeX JS/CSS. The generated preload map points at the emitted `dist/chunks/*.css` paths. Vite emits only WOFF2 KaTeX fonts.

## Service-worker cache policy

The worker intentionally performs **no install-time asset fetches** in either version: installation must survive an expired external-auth session. Therefore the honest install request count is zero, not the number of entries in `SHELL_ASSETS`. The byte figures below describe the cache allowlist candidates that can be populated opportunistically by real page requests.

| Compression | Legacy candidates | Final candidates | Delta |
|---|---:|---:|---:|
| Raw | 1,569,542 | 597,780 | −971,762 (−61.9%) |
| gzip-6 | 601,124 | 370,970 | −230,154 (−38.3%) |
| Brotli-11 | 558,910 | 359,919 | −198,991 (−35.6%) |
| Install requests | 0 | 0 | 0 |

The final allowlist contains only the versioned URLs requested directly by HTML: manifest, icon, application CSS, and the entry module. Vite's stable-named vendor, WebRTC, KaTeX, and highlight chunks are imported without the entry module's query string, so they are deliberately excluded from `SHELL_ASSETS` and handled network-first. This prevents a deployment from executing a stale cached chunk graph while still caching successful chunk responses opportunistically.
