# Bubble Tea provenance

- Module path: `charm.land/bubbletea/v2` (retained for Bubbles/Huh type identity)
- Upstream: <https://github.com/charmbracelet/bubbletea>
- Durable upstream base: tag `v2.0.6`, commit `fdcd0cfd598195e7043c18ab1bc65dcae03588f5`
- License: MIT; the upstream copyright and permission text are retained verbatim in `LICENSE`

## term-llm-owned divergence

This directory is the reduced terminal event loop/runtime owned by term-llm, not a mirror of a source-spike branch. Relative to the base above, term-llm owns:

- `tea.go`, `renderer.go`, and `cursed_renderer*.go`: renderer-bound `PostFrame`/`TerminalCleanup`, exact acknowledgement scheduling, panic-safe renderer lifecycle, queued-view/scrollback synchronization, output-failure invalidation, bounded result delivery, and incremental changed-row/exact-scroll rendering. `Kill` cancels the input reader and waits up to 500 ms for its read loop before closing/restoring the terminal; this bounded join avoids descriptor races without allowing a broken platform cancellation to hang shutdown;
- `screen.go`, `exec.go`, `tty*`, `termios*`, `signals*`, and input/event files: the lifecycle and platform surface reachable by term-llm and retained Bubbles/Huh packages;
- focused renderer, lifecycle, geometry, short-write, queue, stress/fuzz, benchmark, program, option, command, screen, and exec tests plus retained goldens;
- pruning documented in `../README.md`, including examples, tutorials, generic extension APIs, high-level logging/profile helpers, unused queries/events/options, rendererless mode, and their dependencies.

The implementation history is carried by term-llm commits and this file-level contract, not by opaque commits from a temporary import branch. When importing an upstream fix, diff it from the durable base/tag, apply only the required production files and focused tests, preserve the owned surfaces above, update this base if appropriate, and follow the selective-sync and validation checklist in `../README.md`.

## Selected upstream commits landed

Each was landed red-test-first (a focused test that fails without the fix) and adapted to the owned surfaces above. The durable base stays at `v2.0.6`: the rest of the upstream range is still unapplied, so diffs continue to be taken from that tag.

- `faf4dcf` (v2.0.9) `fix: pendingErase in cursedRenderer` (#1755): the unchanged-view shortcut could drop the repaint that `resize()`/`clearScreen()` erase requires, so a clear-screen or resize could leave stale content on screen. Covered by `TestCursedRendererRepaintsSameViewAfterClearScreen` and `TestCursedRendererRepaintsSameViewAfterResize` (same-size inline, height-only inline, and same-size alternate-screen variants - the three cases where only `pendingErase` can force the repaint; a height change under alt screen is already forced by geometry and is kept as a documented control).
- `0d3e281` (v2.0.9) `fix: restore kitty keyboard stack on exit` (#1750): Kitty keyboard state is pushed on screen entry and popped on exit/screen switch instead of being set/reset in place, only the flags-change path updates the top entry, and no keyboard-enhancement query is sent when input is disabled. Covered by `TestCursedRendererPushesAndPopsKittyKeyboardStack` (exact push/pop accounting, so a double push or missing pop fails), `TestCursedRendererStacksKittyKeyboardPerScreenAcrossScreenSwitch`, `TestCursedRendererStacksKittyKeyboardAcrossStopStart` (suspend/resume) and `TestProgramWithNoInputSkipsKeyboardEnhancementSequences`. The twelve `testdata/TestViewModel/*` and `testdata/TestClearMsg/*` fixtures that encoded the old set/reset bytes were regenerated; their only deltas are `\x1b[=1;1u`->`\x1b[>1u`, `\x1b[=0;1u`->`\x1b[<1u`, the flags-3 form of the same, and the removal of the pre-switch reset that is now suppressed because nothing had been pushed. Upstream also touched two fixtures (`bg_fg_cur_color`, `read_set_clipboard`) that this fork prunes.
- `074596e` (v2.0.7) `fix: skip input reader restore when input is disabled` (#1680): `RestoreTerminal` no longer rebuilds an input reader for `WithInput(nil)` programs (the read loop otherwise dereferences a nil reader and panics). Covered by `TestTeaExecWithNilInput`, which drives a real `Run()` with `WithInput(nil)` and an `ExecProcess` command and fails with the panic when the guard is removed. The guard is expressed as `!p.disableInput`, which is equivalent to upstream's `p.input != nil` because `options.go` sets `disableInput = input == nil` and `Run` only defaults `p.input` to `os.Stdin` when input is enabled.
- `1862dfb` (v2.0.9) `fix: assign MouseButton11 = uv.MouseButton11` (#1754): the bare constant repeated `MouseButton10`. Covered by `TestOwnedMouseButton11Mapping`.
- `dc4b017` (v2.0.9) `fix(key): map media record to ultraviolet code` (#1757): the bare constant repeated `KeyMediaPrev`. Covered by `TestOwnedKeyMediaRecordMapping`.
- `930e18c` (v2.0.9) `fix: don't panic in ProgressBarState.String() for out-of-range values` (#1748). Covered by `TestProgressBarStateStringOutOfRange`; term-llm itself does not use `ProgressBar`, so this is API robustness.

Not landed from that range: `c60f0c5` (the `onMouse` race) is already present via the owned renderer lifecycle, and `878d7df`/`db569ad` are dependency bumps for the sibling renderer module rather than runtime fixes.

Fork-local hardening (no upstream commit, landed red-test-first against the owned surfaces):
- `setNoInput` now takes `s.mu` like its sibling setters (`setLogger`, `setOptimizations`), so the `noInput` field that `start`, `updateKeyboardEnhancementsLocked` and `resetKeyboardEnhancements` read under the lock can no longer be written unsynchronized by a caller that does not respect the construction-time ordering. Covered by `TestSetNoInputConcurrentWithFlush`, which fails under `-race` (and only under `-race`) when the transition is unlocked.
- `close()` drains the screen renderer before writing its keyboard reset, so a program stopped before the next frame (suspend/resume followed by a kill) no longer emits the Kitty pop before the push that the resumed `start` queued, which would leave an orphan entry pushed on the terminal. Covered by `TestKittyStackPushPrecedesPopWhenStoppedBeforeFlush`. The drain reports no error, matching the adjacent cursor-move flush.
