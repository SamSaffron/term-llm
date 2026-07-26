# Bubble Tea provenance

- Module path: `charm.land/bubbletea/v2` (retained for Bubbles/Huh type identity)
- Upstream: <https://github.com/charmbracelet/bubbletea>
- Durable upstream base: tag `v2.0.6`, commit `fdcd0cfd598195e7043c18ab1bc65dcae03588f5`
- License: MIT; the upstream copyright and permission text are retained verbatim in `LICENSE`

## term-llm-owned divergence

This directory is a reduced application module, not a mirror of a source-spike branch. Relative to the base above, term-llm owns:

- `tea.go`, `renderer.go`, and `cursed_renderer*.go`: renderer-bound `PostFrame`/`TerminalCleanup`, exact acknowledgement scheduling, panic-safe renderer lifecycle, queued-view/scrollback synchronization, output-failure invalidation, bounded result delivery, and incremental changed-row/exact-scroll rendering;
- `screen.go`, `exec.go`, `tty*`, `termios*`, `signals*`, and input/event files: the lifecycle and platform surface reachable by term-llm and retained Bubbles/Huh packages;
- focused renderer, lifecycle, geometry, short-write, queue, stress/fuzz, benchmark, program, option, command, screen, and exec tests plus retained goldens;
- pruning documented in `../README.md`, including examples, tutorials, generic extension APIs, high-level logging/profile helpers, unused queries/events/options, rendererless mode, and their dependencies.

The implementation history is carried by term-llm commits and this file-level contract, not by opaque commits from a temporary import branch. When importing an upstream fix, diff it from the durable base/tag, apply only the required production files and focused tests, preserve the owned surfaces above, update this base if appropriate, and follow the selective-sync and validation checklist in `../README.md`.
