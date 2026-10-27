# Owned QR encoder

This directory is term-llm's owned fork of
[`skip2/go-qrcode`](https://github.com/skip2/go-qrcode), imported from exact upstream
commit **`da1b6568686e89143e94f980a98bc2dbd5537f13`**. term-llm maintains the code
and tests here; it is not a generated vendor directory. Review upstream changes
against that commit before selectively applying them, and record local changes
below.

The import includes every library Go source and unit test from the upstream root,
`bitset`, and `reedsolomon` packages, plus the original MIT `LICENSE`. Copyright
notices are preserved. The upstream CLI (`qrcode/`), `go.mod`, and CI configuration
are intentionally excluded. This fork belongs to the root term-llm module and
adds no third-party production dependencies.

## Local changes

- All library imports now use
  `github.com/samsaffron/term-llm/internal/qrcode` and its subpackages. Existing
  package documentation is retained in `qrcode.go`, `bitset/bitset.go`, and
  `reedsolomon/reed_solomon.go`; there are no duplicate package comments.
- Split upstream `version.go` into types/methods (`version.go`), format/version
  information (`version_info.go`), and version-table declarations for ranges
  1–10, 11–20, 21–30, and 31–40 (`version_table_<first>_<last>.go`).
  `version_table.go` concatenates those ranges with the standard-library
  `slices.Concat`, preserving all 160 entries, block values, and their original
  order (ascending version, then Low, Medium, High, Highest). Only table
  indentation changes; every non-test Go file stays below 1,000 lines.
- Fix upstream [issue #67](https://github.com/skip2/go-qrcode/issues/67):
  `WriteColorFile` checks the error from `New` before assigning colors. Regression
  tests cover empty and oversized numeric, alphanumeric, and byte content at all
  four recovery levels, requiring an error without a panic or output file.
- Fix the timing-pattern boundary described in upstream
  [issue #60](https://github.com/skip2/go-qrcode/issues/60): timing patterns end at
  coordinate `size-9`, leaving the white finder separators at `size-8` to finder
  drawing. Regression tests check occupancy and alternating values for all 40
  versions, with and without a quiet zone. This removes a redundant separator
  write without changing the completed symbol.
- Resolve the integer-to-string vet diagnostics discussed in upstream
  [PR #53](https://github.com/skip2/go-qrcode/pull/53): decoder tests explicitly
  convert integers to runes before strings, retaining their intended Unicode
  character semantics rather than substituting decimal text or raw bytes.
- Isolate example PNG outputs in `t.TempDir()` and correctly fail the file-writing
  example when encoding/writing fails. Upstream wrote `example.png` and
  `example2.png` in the source directory, and one example silently ignored write
  failures when cleanup also succeeded.
- Remove the decoder mismatch helper's diagnostic PNG write into the working
  directory (the error already includes hexadecimal content), and fail fast in
  decoder tests/benchmarks on errors rather than continuing with a nil QR code.
- Apply `gofmt`, including its formatting of upstream documentation comments.

Other encoding behavior and upstream APIs are intentionally unchanged.

## Verification

Run from the repository root using the isolated repository test wrapper:

```sh
make test PKG='./internal/qrcode/...'
```

External-command decoder tests remain **opt-in**, exactly as upstream. They need
`zbarimg` on `PATH` (for example, the `zbar-tools` system package) and are not
required for normal unit tests:

```sh
scripts/gotest.sh ./internal/qrcode -args -test-decode -test-decode-fuzz
```

The opt-in suite checks basic content, all version/recovery-level combinations,
Unicode character content, and deterministic randomized printable input. The
`-test-decode` flag also enables the upstream decoder benchmark when benchmarks
are explicitly requested.
