# Owned internal terminal subsystem

term-llm maintains its terminal event loop/runtime and cell/diff/output renderer under `internal/terminal/`. These are application-owned source modules: their filesystem placement, pruning policy, behavior, tests, and selective upstream maintenance all belong to this repository. They are not external dependency copies, independently consumed libraries, or architecturally upstream-owned components.

## Architecture

- [`runtime/`](runtime/) is the reduced Bubble Tea-derived event loop and terminal runtime. It owns programs, input, commands, terminal mode transitions, render scheduling, exact frame acknowledgements, process handoff, and shutdown.
- [`renderer/`](renderer/) is the reduced Ultraviolet-derived cell, diff, terminal-input, and output renderer. It owns retained cell state, incremental changed-row/exact-scroll output, terminal decoding, platform pollers/readers, and write recovery.
- The root application, Bubbles, and Huh consume the runtime through `charm.land/bubbletea/v2`; the runtime consumes the renderer through `github.com/charmbracelet/ultraviolet`. The root `go.mod` pins both identities to these exact in-repository directories, and the runtime's nested `go.mod` pins its standalone renderer dependency to `../renderer`.

The runtime/renderer split is an internal architectural boundary. Bubble Tea and Ultraviolet name the upstream projects from which the retained code was derived; they do not describe maintenance ownership.

## Compatibility identities and Go module boundaries

The runtime deliberately preserves the module path `charm.land/bubbletea/v2`. term-llm imports that path directly, and Bubbles `v2.1.0` and Huh `v2.0.3` exchange concrete Bubble Tea types through that exact path. Renaming the module would create distinct Go types and require unnecessary Bubbles/Huh copies. The renderer likewise preserves `github.com/charmbracelet/ultraviolet` so the runtime and root module resolve one exact renderer identity. These upstream names are compatibility identities only; filesystem placement and maintenance ownership remain term-llm's.

Placing nested modules below the root module's `internal/` directory does not change their declared import paths or trigger Go's `internal` package visibility rule. Each nested `go.mod` establishes its own module root, so consumers still import `charm.land/bubbletea/v2` and `github.com/charmbracelet/ultraviolet`, neither of which contains an `internal` path element. Root builds, Bubbles/Huh compilation, standalone nested builds, and the exact-resolution buildguard exercise this boundary explicitly.

These are application-specific source modules, not general-purpose library distributions. The compatibility contract is the source/API surface reachable from term-llm and its retained Bubbles/Huh versions. The owned modules and all 22 Bubbles/Huh packages have been compile-checked for `linux/amd64`, `darwin/arm64`, and `windows/amd64`, including each platform's selected terminal, poller, TTY, signal, termios, cancel-reader, and Bubbles file-picker files. The term-llm application and releases remain scoped to their existing Linux and macOS targets; terminal ownership does not add Windows application support.

The root `go.mod` has exact replacements at `./internal/terminal/runtime` and `./internal/terminal/renderer`, plus the pre-existing owned reflow replacement at `./internal/reflow`. There are no references to source-spike trees or paths outside this repository. Because Go does not carry a dependency's `replace` directives into an external module build—and nested modules are excluded from the root module archive—`go install github.com/samsaffron/term-llm@latest` cannot assemble these sources. Install a release artifact or run `go install .` from a complete checkout.

## Retained source

### Bubble Tea

The reduced `charm.land/bubbletea/v2` module retains:

- the `Model`, `Msg`, `Cmd`, `View`, `Program`, `ProgramOption`, batching, sequencing, ticking, quit/interrupt/suspend, raw output, printing, resize, and program-control APIs used by term-llm, Bubbles, or Huh;
- concrete key, focus, paste, mouse, cursor, background-color, window-size, and mode-report types required by those consumers;
- `ExecProcess` and terminal release/restore behavior used for shell/editor handoff;
- environment-driven terminal capability detection and the options actually used by the retained consumers (`WithContext`, `WithInput`, `WithOutput`, `WithEnvironment`, `WithoutSignalHandler`, `WithFPS`, and `WithWindowSize`);
- Unix, macOS/BSD, and Windows signal, termios, raw-terminal, controlling-TTY, and TTY-open implementations;
- the cursed renderer, incremental changed-row and exact-scroll paths, renderer-result scheduling, renderer-bound `PostFrame` acknowledgement, and `TerminalCleanup` lifecycle;
- focused command/options/program/screen/exec tests, renderer lifecycle/output/geometry/scroll regressions, the retained screen goldens, renderer benchmarks, and the exact-overlap fuzz target.

### Ultraviolet

The reduced `github.com/charmbracelet/ultraviolet` module retains:

- cell/style/link, rectangle, buffer, screen-buffer, styled-string, cursor, tab-stop, event, key, mouse, environment, and terminal-decoder primitives used by Bubble Tea;
- terminal input readers and cancel readers, with epoll on Linux, kqueue/select on macOS/BSD, native Windows polling/input, and fallback pollers for other supported targets;
- controlling-TTY open helpers for Unix, Windows, and unsupported-platform fallback;
- `TerminalRenderer`, render buffers and hash maps, inline/fullscreen output, exact `HardScroll`, incremental `DrawOver`, and the wide-cell and repeated-row geometry fixes;
- focused buffer/cell/style/event/key/decoder/poller/reader/renderer tests, renderer output and hard-scroll regressions, and both exact-scroll benchmarks.

Both modules retain their upstream MIT `LICENSE` verbatim. Each module's `UPSTREAM.md` records a durable upstream base and a file-level description of term-llm-owned divergence; it intentionally does not depend on ephemeral source-spike commit IDs.

## Deleted source

The pruning intentionally deletes the following disconnected material.

### From both modules

- upstream GitHub issue templates, workflows, CODEOWNERS, Dependabot configuration, release/lint configuration, repository attributes/ignore files, task/release files, and upstream badges/readmes;
- all examples, tutorials, example-only nested modules, recordings, screenshots, GIF/JPEG assets, and upgrade/tutorial documents;
- tests and goldens whose implementation or API was intentionally removed. Tests for retained renderer, input, lifecycle, geometry, and platform code remain.

### Bubble Tea-only removals

- clipboard read/write commands and events;
- startup environment events (while retaining `WithEnvironment`), foreground/cursor color queries, cursor-position queries, termcap capability queries, and terminal-version queries;
- key-release, paste-boundary, keyboard-enhancement-response, color-profile-message, and the other disconnected event adapters not consumed by term-llm/Bubbles/Huh;
- the generic exported `Exec`/`ExecCommand` extension point (while retaining `ExecProcess`), wall-clock-aligned `Every`, package-global logging helpers, and the unused xterm helper;
- rendererless mode and its nil renderer, plus unused catch-panic, ignore-signal, rendererless, filter, and forced-color-profile options;
- the corresponding implementation-only dependencies and tests.

### Ultraviolet-only removals

- the independent high-level `Terminal`, `Console`, `TerminalScreen`, `Window`, and window-size notifier frameworks;
- the border widget, `layout` constraint/flex package, `screen` retained-mode package, Cassowary solver, and LRU cache;
- delay/terminal mode files used only by the removed high-level terminal framework;
- their tests and implementation-only dependencies.

## Selective upstream sync

Do not replace either directory wholesale or run an automated upstream merge. To take a fix:

1. Fetch the relevant upstream repository in a temporary checkout outside this repository.
2. Diff or format-patch the selected upstream commit against the pinned base in the module's `UPSTREAM.md`.
3. Apply only the production files and focused tests required by the compatibility boundary. Preserve the owned renderer changes and both module paths.
4. Re-run a whole-source import/symbol audit for term-llm, Bubbles, and Huh before deleting newly introduced APIs. Do not infer compatibility from the Linux chat package alone.
5. Update the pinned base and selected-commit list in `UPSTREAM.md`, then repeat platform, race, checkptr, fuzz/stress, and benchmark checks.
6. Run `go mod tidy` in the changed owned terminal module and at the repository root. The root build guard must continue to resolve both modules to their exact in-repository directories with `GOWORK=off`.

## Tests and benchmarks

From the repository root:

```sh
scripts/verify_nested_modules.sh
VERIFY_RACE=1 scripts/verify_nested_modules.sh
scripts/fuzz_owned_renderer.sh

(cd internal/terminal/runtime && GODEBUG=checkptr=2 go test ./...)
(cd internal/terminal/renderer && GODEBUG=checkptr=2 go test ./...)
(cd internal/terminal/runtime && go test -run '^$' -bench '^BenchmarkCursedRenderer$' -benchmem .)
(cd internal/terminal/renderer && go test -run '^$' -bench '^BenchmarkRendererScroll' -benchmem .)

# Isolate user configuration and skills for the complete application suite
# while reusing the existing read-only module download cache.
GOMODCACHE=$(go env GOMODCACHE)
TEST_HOME=$(mktemp -d)
HOME="$TEST_HOME" XDG_CONFIG_HOME="$TEST_HOME/config" XDG_DATA_HOME="$TEST_HOME/data" XDG_CACHE_HOME="$TEST_HOME/cache" GOMODCACHE="$GOMODCACHE" go test ./...
rm -rf "$TEST_HOME"
```

Cross-platform reachability for the owned terminal subsystem is checked without executing foreign binaries. Root application cross-builds should continue to use the Linux and macOS targets in `.goreleaser.yml`; Windows here verifies only the terminal runtime/renderer boundary and must not drive unrelated application process-control changes:

```sh
scripts/cross_build_owned_terminal.sh
```

`internal/reflow` currently has no test files, so its entries in the nested script provide build, vet, checkptr, and race-instrumented compilation coverage rather than meaningful behavioral test coverage. Bubble Tea and Ultraviolet have the focused tests described above.

Renderer fuzz targets are committed with regression corpus entries and CI executes both targets (rather than only replaying seeds):

```sh
scripts/fuzz_owned_renderer.sh
# Optional longer local run:
FUZZ_DIFFERENTIAL_TIME=60s FUZZ_SHIFT_TIME=30s scripts/fuzz_owned_renderer.sh
```

There were no separate upstream NOTICE files at the pinned revisions.
