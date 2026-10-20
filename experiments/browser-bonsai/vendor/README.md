# Vendored WebGPU inference engine

`kernels.ts` is the self-contained WebGPU inference engine from Prism ML's
[Talk](https://github.com/PrismML-Eng/Talk). It is vendored unmodified so the
static demo can run Bonsai 2 without a package CDN or a build-time clone.
`web/inference.mjs` adapts its public `Bonsai` class to the demo's OpenAI-style
inference bridge.

## Provenance

| Item | Value |
| --- | --- |
| Upstream repository | `https://github.com/PrismML-Eng/Talk` |
| Commit | `0dfac42dda3e37cc359e84ef5ac2567f821c1871` (`0dfac42`) |
| Upstream path | `kernels.ts` |
| Raw source | `https://raw.githubusercontent.com/PrismML-Eng/Talk/0dfac42dda3e37cc359e84ef5ac2567f821c1871/kernels.ts` |
| SHA-256 | `a773676ae581975985d28813e7a28249d47c035b5ff5b7f5dc66e450c525b899` |
| Size | 3,162,601 bytes |
| License | Apache-2.0 (see `LICENSE` and `NOTICE`) |

The file is copied byte-for-byte. `bundle.mjs` passes it through esbuild, which
only erases TypeScript types and minifies; no engine logic is edited here.

`LICENSE` and `NOTICE` are the upstream Talk files at the same commit. The
upstream `NOTICE` also covers Talk assets (speech models, PDF.js, fonts, and so
on) that this demo does not vendor or ship.

## Model weights

Weights are not vendored. The deployed demo serves the pinned file through its
same-origin Cloudflare-cached `/models/` URL. The Hugging Face URL below records
its original provenance and remains the browser cache key so previously cached
chunks can be reused (`Bonsai.load({ cache: true })`).

| Item | Value |
| --- | --- |
| Repository | `prism-ml/Ternary-Bonsai-2-2B-gguf` |
| Revision | `683f7be2629342a50987627b40881e54761a2430` |
| File | `Ternary-Bonsai-2-2B-PTQ1_0.gguf` |
| Size | 429,183,456 bytes |
| LFS SHA-256 | `36061ad406df8a4903a6db005be350c50fabfaf717f9ef0acf4b76adcd9929fa` |
| License | Apache-2.0 |
| Resolve URL | `https://huggingface.co/prism-ml/Ternary-Bonsai-2-2B-gguf/resolve/683f7be2629342a50987627b40881e54761a2430/Ternary-Bonsai-2-2B-PTQ1_0.gguf` |

No prompt, conversation, or workspace data is written to storage. Only model weights and GPU kernel-tuning metadata may persist; conversation state remains in worker memory.
