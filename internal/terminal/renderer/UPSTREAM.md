# Ultraviolet provenance

- Module path: `github.com/charmbracelet/ultraviolet`
- Upstream: <https://github.com/charmbracelet/ultraviolet>
- Durable upstream base: commit `489999b904687f0b4cb345f748f14e6a1e5f3106` (the version required by Bubble Tea `v2.0.6`)
- License: MIT; the upstream copyright and permission text are retained verbatim in `LICENSE`

## term-llm-owned divergence

This directory is the reduced terminal renderer owned by term-llm and maintained with the sibling runtime, not an independently published library. Relative to the base above, term-llm owns:

- `styled.go`: incremental `StyledString.DrawOver` behavior used by changed-row and exposed-scroll repainting, and rejection of CSI sequences carrying enough parameters to overflow the fixed parameter buffer in `x/ansi`;
- `terminal_renderer.go`, `terminal_renderer_hardscroll.go`, and renderer buffer/hash-map code: exact `HardScroll`, complete stale-row touching, wide-cell deletion correctness, output-state regressions, and per-frame line-damage records that are reset in place rather than reallocated for every line of both buffers on every render;
- `decoder.go`: sequences introduced by a single-byte 8-bit C1 control are decoded consistently with their two-byte `ESC`-prefixed form. X10 mouse reports read their payload from the last three bytes rather than a fixed offset; an abandoned OSC/APC/PM/SOS no longer resynchronizes to a fabricated `Alt+<key>` built from a data byte; an OSC without a `;` separator no longer parses the introducer bytes as its payload; and the OSC give-up test no longer compares against the payload offset of the seven-bit introducer;
- retained cell/buffer/style/event/key/mouse/decoder/reader/poller/TTY implementations needed by Bubble Tea on Linux, macOS/BSD, and Windows;
- focused tests and benchmarks for those retained primitives and renderer paths;
- pruning documented in `../README.md`, including high-level terminal/window/console frameworks, layout/screen widgets, examples/tutorials, repository automation, and their dependencies.

The implementation history is carried by term-llm commits and this file-level contract, not by opaque commits from a temporary import branch. When importing an upstream fix, diff it from the durable base, apply only the required production files and focused tests, preserve the owned renderer behavior above, update this base if appropriate, and follow the selective-sync and validation checklist in `../README.md`.
