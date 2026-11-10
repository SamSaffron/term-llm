# Embedded QuickJS provenance and binding

The product embeds `quickjs.wasm` from **quickjs-wasi 3.6.2**; it does not run the npm JavaScript wrapper, Node, extensions, a native interpreter, or a downloaded binary at runtime. The thin Go adapter uses wazero v1.11.0. `js/wasm` hosts return an explicit unavailable result.

## Exact published artifact

- URL: https://registry.npmjs.org/quickjs-wasi/-/quickjs-wasi-3.6.2.tgz
- npm integrity: `sha512-FCqGtGOrMgzUiIrMNMA2YnsOxCNwo31dzqXvclXUC6xeT35NJLKXQJsvbeCTjvoFAwZgEAPg8U6+KAPDGXn8Mg==`
- `package/quickjs.wasm`: 637405 bytes
- SHA-256: `d4c9375f2b1ca4dc95f72c8aa2982a7a9951ac8011490d79c6582df732b4bbd9`
- Package license: MIT, Vercel, Inc. (`LICENSE.quickjs-wasi`). Embedded QuickJS-NG is MIT (`LICENSE.quickjs-ng`).

The npm SLSA provenance statement at https://registry.npmjs.org/-/npm/v1/attestations/quickjs-wasi@3.6.2 identifies source https://github.com/vercel-labs/quickjs-wasi at commit `5a7a0eeda87c99542f8cf3095b6d61ecfa755977`, workflow `.github/workflows/release.yml`, invocation https://github.com/vercel-labs/quickjs-wasi/actions/runs/35422428978/attempts/1. That source tree pins `quickjs-ng` to `6d46d07d04041b40f4f49eaa7fdebe44c314c699`. This is provenance inspection, not an independent cryptographic verification of npm's Sigstore bundle, and not a claim of a byte-identical local source rebuild. There is no assumed v3.6.2 Git tag or invented npm gitHead.

Reacquire in repository scratch space and verify **before** replacing the embedded file:

```sh
mkdir -p tmp/quickjs-acquire
curl -fSL https://registry.npmjs.org/quickjs-wasi/-/quickjs-wasi-3.6.2.tgz -o tmp/quickjs-acquire/package.tgz
openssl dgst -sha512 -binary tmp/quickjs-acquire/package.tgz | openssl base64 -A
# Compare against the integrity above, excluding the sha512- prefix.
tar -xzf tmp/quickjs-acquire/package.tgz -C tmp/quickjs-acquire
sha256sum tmp/quickjs-acquire/package/quickjs.wasm
```

For a source rebuild, check out the provenance source and its exact submodule, follow its `Makefile`/release workflow with wasi-sdk32 and pinned Binaryen130 (`pnpm install --frozen-lockfile`, `make`). Upstream optimization/toolchain choices affect bytes; a different hash requires an explicit artifact review and ABI/limit gate, not silently changing the pinned checksum. Normal Go/release builds never perform these steps and need no wasi-sdk, npm or CGO for the interpreter (the existing frontend build still uses npm).

## Narrow sandbox aperture

`TestPinnedABI` enumerates every actual import/export and checks the embedded hash. The adapter uses only lifecycle/value allocation, string properties, function calls, eval of source, one-step promise job pumping, limits/interrupt and the module resolver ABI from `c/interface.c`. No bytecode or snapshot ingestion, extension loading, std/os, dynamic linking, reflected Go values or direct filesystem/network access is exposed to JS. The WASI reactor has no preopened directories, environment, stdin or sockets. The six actual WASI imports are clock_time_get, fd_close, fd_fdstat_get, fd_seek, fd_write and random_get; the six env imports are host_call, host_interrupt, host_promise_rejection, host_module_normalize, host_module_load and host_get_timezone_offset.

Every instance, including reentrant string/allocator calls inside a synchronous host trampoline, is accessed by one owner goroutine. The callback copies a bounded JSON request and returns immediately. Only the owner pumps jobs or settles completions; workers cannot touch WASM memory. Each run has its own context, completion channel and instance, so a completion after cancellation cannot address another generation. Wazero closes on context cancellation; no QuickJS cleanup export is called after a forced close. Fresh instances share compilation cache only, never guest memory or globals.

The upstream 1MiB linker stack permits a maximum512KiB QuickJS stack setting. The exact artifact's custom allocator enforces the64MiB heap limit (its source documents why the default QuickJS usable-size path would not suffice on WASI). Tests exercise actual recursion/allocation failures, rather than asserting only that setters returned successfully. Active execution is accumulated around outer WASM calls, excludes waiting on host work, and is interrupted at2s; wall time separately includes approvals/I/O. Native Go tools and remote effects are not memory-sandboxed or forcibly killed by these JS limits.

## Synchronous and asynchronous policies

The original `code_mode` path is a synchronous bounded cell: 64MiB QuickJS heap, 128MiB WASM linear cap, explicit caller-carried state, 2s active JS and a120s/300s wall bound. It remains an explicit compatibility surface.

The engine-owned async `spawn_script`/`wait_script`/`cancel_script` path starts one fresh instance per cell with a different policy: **256MiB QuickJS heap**, **512MiB WASM linear cap** (the linear cap must be larger than the heap), 512KiB stack, 1MiB total JSON state with a256KiB per-value cap, 2s active JS per cell, and no whole-cell wall kill. The 2s guard is accumulated JS execution only; time spent waiting for host tools or `sleep(ms)` does not consume it. A request or wait cancellation does not stop the cell; only explicit cancel or session teardown does. `max_wait` is in seconds (0–600, default60); zero polls immediately. It bounds one observation, not the cell.

Visible text is bounded per observation by `max_output_tokens` (default10000, converted at4 bytes per estimated token). Output withheld by that observation budget stays queued; the retained collector is separately bounded at1MiB of text and16 images/16MiB of decoded image bytes. Excess collector output is permanently truncated and reported in the metadata envelope rather than failing otherwise successful work. State is committed only after successful completion; failed, cancelled or torn-down cells discard staged state, and image handles are rejected from state.
