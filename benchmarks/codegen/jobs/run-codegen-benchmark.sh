#!/usr/bin/env bash
set -euo pipefail

ROOT="$(git -C "$(dirname "$0")/../../.." rev-parse --show-toplevel)"
cd "$ROOT"

exec go run ./benchmarks/codegen \
  -provider "${BENCH_PROVIDER:-claude-bin}" \
  -tasks "${BENCH_TASKS:-all}" \
  -runs "${BENCH_RUNS:-1}" \
  -concurrency "${BENCH_CONCURRENCY:-2}" \
  -budget "${BENCH_BUDGET:-4h}" \
  -timeout "${BENCH_TASK_TIMEOUT:-5m}" \
  -score-timeout "${BENCH_SCORE_TIMEOUT:-20s}" \
  -out "${BENCH_OUT:-benchmarks/codegen/results}"
