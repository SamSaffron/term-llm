# Clipboard platform shim

Vendored from `github.com/atotto/clipboard` v0.1.4 (BSD-3-Clause; see LICENSE).
The existing native implementations are retained. Go formatting adds modern
build-tag equivalents and normalizes import order; there are no native behavior
changes. `clipboard_wasm.go` adds explicit unsupported errors for browser/WASI
hosts, which have no operating-system clipboard commands.

The root `go.mod` replacement is needed because the terminal dependency imports
this package even though the browser demo does not use its native clipboard.
