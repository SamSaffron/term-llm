# Ultraviolet provenance

- Module path: `github.com/charmbracelet/ultraviolet`
- Upstream: <https://github.com/charmbracelet/ultraviolet>
- Durable upstream base: commit `489999b904687f0b4cb345f748f14e6a1e5f3106` (the version required by Bubble Tea `v2.0.6`)
- License: MIT; the upstream copyright and permission text are retained verbatim in `LICENSE`

## term-llm-owned divergence

This directory is the reduced renderer implementation owned with Bubble Tea, not an independently published Ultraviolet fork. Relative to the base above, term-llm owns:

- `styled.go`: incremental `StyledString.DrawOver` behavior used by changed-row and exposed-scroll repainting;
- `terminal_renderer.go`, `terminal_renderer_hardscroll.go`, and renderer buffer/hash-map code: exact `HardScroll`, complete stale-row touching, wide-cell deletion correctness, and output-state regressions;
- retained cell/buffer/style/event/key/mouse/decoder/reader/poller/TTY implementations needed by Bubble Tea on Linux, macOS/BSD, and Windows;
- focused tests and benchmarks for those retained primitives and renderer paths;
- pruning documented in `../README.md`, including high-level terminal/window/console frameworks, layout/screen widgets, examples/tutorials, repository automation, and their dependencies.

The implementation history is carried by term-llm commits and this file-level contract, not by opaque commits from a temporary import branch. When importing an upstream fix, diff it from the durable base, apply only the required production files and focused tests, preserve the owned renderer behavior above, update this base if appropriate, and follow the selective-sync and validation checklist in `../README.md`.
