# Guardian classify evaluation

`go run ./evaluation/guardian-classify` reviews shell-command **strings**, never executes them. It
requires `guardian.backend: classify` for evaluation and reuses the production
classify client, reviewer, policy loading, transcript compaction, timeouts and
confidence gates. It does not create shell tools or an ApprovalManager, so denials
cannot trip the auto-approval breaker. Every selected case is reviewed, including
commands that an interactive shell approval fast path might otherwise skip.

This is a development evaluation binary, not a `term-llm` subcommand. Run from
the repository root, or build it separately:

```bash
go build -o ./tmp/guardian-classify ./evaluation/guardian-classify
./tmp/guardian-classify --help
```

It loads the normal term-llm configuration: `guardian.classify.provider` overrides
`classify.default_provider`; provider API-key and base-URL references, transport
timeout, Guardian timeout, policy path and minimum confidence use production
semantics. No configuration is loaded in corpus-only modes.

Inspect the exact corpus **without configuration, credentials or provider calls**:

```bash
go run ./evaluation/guardian-classify --dry-run --limit 5
go run ./evaluation/guardian-classify --write-corpus shell-1000.jsonl
go run ./evaluation/guardian-classify --dry-run --offset 700 --limit 30
```

To perform a paid/networked evaluation after inspecting the corpus and configuring
Guardian (these commands send the evidence to the configured classification endpoint):

```bash
go run ./evaluation/guardian-classify --suite shell-1000 --limit 10 --json
go run ./evaluation/guardian-classify --suite shell-1000 --concurrency 4 \
  --json --raw-output guardian-records.jsonl > guardian-summary.json
go run ./evaluation/guardian-classify --input shell-1000.jsonl --offset 500 --limit 100 --json
```

### Corpus and labels

`shell-1000` version 1 deterministically expands 100 auditable command templates
across ten named fixture services: **1,000 unique IDs and exact commands**. This is
100 command patterns with ten operational-target variants, not 1,000 independent
attack techniques or a statistically independent sample of production traffic.
The generator lives in `evaluation/guardian-classify/internal/eval/corpus.go`; `--write-corpus` makes
all expansions inspectable. Paths, resources, hosts and data are fictional, and
no credential values are embedded. Each case includes `id`, `expected`
(`allow`/`deny`), `command`, `workdir`, `transcript`, `tags`, and `category`.
Transcripts use the existing Guardian entry shape (`Role`, `Text`); each built-in
case has an actual `user` message authorizing its exact command.

| Expected | Categories | Cases |
| --- | --- | ---: |
| Allow | Filesystem, Git inspection, search, data processing, build/test, local edits, ops inspection | 100 each; **700** |
| Deny | Secret reads, exfiltration, broad deletion, protected Git refs, destructive databases, destructive cloud, destructive containers, privilege/persistence, security weakening, downloaded execution | 30 each; **300** |

Benign cases are scoped to trusted development checkouts, synthetic public
fixtures and development resources. Bombs deliberately remain high/critical under
`DefaultPolicy` **even with explicit authorization**. They include key-file reads,
secret uploads, production data destruction and persistent privilege/security
changes. These strings would be dangerous if copied into a shell: **never pipe
corpus or record contents into an executor**. No fixture path is inspected, no
working directory is entered, and no command is parsed or executed by the evaluator.

The configured `guardian.policy_path` is honored, not overridden. Expected labels
are calibrated to `DefaultPolicy`; a custom policy can legitimately change results.
JSON summaries include the effective policy SHA-256 for comparison.

### Flags, output and interpretation

- `--input FILE` (or `-` for stdin) accepts JSONL instead of the built-in suite.
  It rejects unknown fields, missing required fields, duplicate IDs/commands,
  invalid labels and trailing JSON. Limits: 64 MiB total, less than 1 MiB per
  line, 100,000 cases. The whole corpus is validated before chunking or setup.
- `--offset N` skips cases, then `--limit N` selects at most N (zero means all
  remaining). Offsets outside the corpus and negative values are errors.
- `--concurrency N` bounds reviews to 1–32 (default 1). Record ordering always
  matches corpus ordering, not completion order. The existing client can retry
  transient transport errors; a review is not necessarily one HTTP attempt.
- `--json` emits the complete summary, including per-category counts/rates;
  otherwise a concise human summary is printed. Latency includes the complete
  reviewer call, including retries, but not worker-queue time or provider setup.
  p50/p95/p99 use nearest rank over all attempts, including errors.
- `--raw-output FILE` writes one JSON record per selected case: exact original
  case, expected/actual outcome, `pass`/`fail`/`error`, deterministic gate rationale,
  risk, authorization, model, `duration_ms`, and serialized Guardian `state_bytes`.
  Records are buffered and written after all reviews finish; abrupt termination
  can leave an empty/incomplete file. It is not a resumable checkpoint.
- Confusion matrices treat **deny as positive**. Review errors are separate, not
  counted as true-positive denials. Benign allow and bomb deny rates include errors
  in their denominators; an outage therefore cannot improve accuracy. A category
  with no cases of a label has rate zero for that label. State-byte ranges include
  zero when a review failed before serialization.
- Mismatches or review errors exit nonzero after writing results. `--report-only`
  suppresses that result-based failure, but not validation/setup/output failures.
- `--dry-run` emits selected corpus JSONL to stdout. `--write-corpus FILE` implies
  corpus-only mode (`-` means stdout); both bypass config/provider setup entirely.
  These modes cannot be mixed with summary/result flags.
- Corpus and raw-result files are created exclusively with mode `0600` and never
  overwrite existing files. Stdout redirection permissions remain your shell's
  responsibility. Raw output requires a filename, keeping summary stdout separate.

**Safety/privacy boundary:** non-executing does not mean offline. Normal evaluation
sends the exact action, effective policy and compacted role-labelled transcript to
the configured provider and uses its normal credential resolution. Raw records
contain your input corpus, so do not put real secrets or private transcripts into
custom cases unless disclosure and storage are authorized. Configuration and
provider error details are suppressed to avoid leaking keys or response bodies;
errors still count as evaluation failures. No credential/config dumps or provider
response prose are written. This evaluates classification and deterministic gating,
not shell behavior, approval fast paths, breaker behavior or real-world execution
safety. Results remain model-, policy- and confidence-threshold-dependent; record
those settings alongside any benchmark report. No live results are bundled.

## Tests

Run without live API access from the repository root:

```bash
go test ./evaluation/guardian-classify/... ./internal/guardian
go test -race ./evaluation/guardian-classify/... ./internal/guardian
```

The tests pin the complete 1,000-case corpus hash, enforce the evaluator's
non-execution import boundary, and cover corpus validation, metrics, ordered
concurrent results, output safety, and injected production-compatible config
wiring. Moving the harness does not change the corpus or invalidate existing
live-evaluation evidence.
