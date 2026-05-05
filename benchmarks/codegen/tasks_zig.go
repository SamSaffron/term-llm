package main

import "time"

type zigSumPositiveTask struct{}

func (zigSumPositiveTask) Name() string       { return "zig_sum_positive_perf" }
func (zigSumPositiveTask) Language() string   { return "zig" }
func (zigSumPositiveTask) Difficulty() string { return "medium-perf-correctness" }
func (zigSumPositiveTask) Prompt() string {
	return `Write a complete Zig source file that exports exactly this C ABI function:

export fn sum_positive(xs: [*]const i64, n: isize) i64

The function must return the sum of all positive values in xs[0:n]. Ignore zero and negative values. If n <= 0, return 0.

Requirements:
- Must compile with zig build-lib.
- Export the symbol as sum_positive using the C ABI.
- Do not allocate memory.
- Must be correct for negative numbers, empty arrays, and mixed values.
- Prefer a simple fast loop over cleverness.`
}

func (zigSumPositiveTask) Score(response string, timeout time.Duration) ScoreResult {
	return scoreZig(response, timeout, `
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/resource.h>
#include <time.h>

long sum_positive(long *xs, long n);

static void check(const char *name, long *xs, long n, long want) {
  long got = sum_positive(xs, n);
  if (got != want) {
    fprintf(stderr, "%s: got %ld want %ld\n", name, got, want);
    exit(1);
  }
}

static double elapsed_ms(struct timespec start, struct timespec end) {
  return (double)(end.tv_sec - start.tv_sec) * 1000.0 + (double)(end.tv_nsec - start.tv_nsec) / 1000000.0;
}

int main(void) {
  long empty[] = {1};
  long mixed[] = {-5, 0, 7, -2, 9, 1};
  long negatives[] = {-9, -8, -7};
  check("n<=0", empty, 0, 0);
  check("mixed", mixed, 6, 17);
  check("negative", negatives, 3, 0);

  const long n = 4096;
  long *xs = malloc(sizeof(long) * n);
  if (!xs) return 1;
  long want = 0;
  for (long i = 0; i < n; i++) {
    long v = (i % 11) - 5;
    xs[i] = v;
    if (v > 0) want += v;
  }
  check("bulk", xs, n, want);

  volatile long sink = 0;
  struct timespec a, b;
  clock_gettime(CLOCK_MONOTONIC, &a);
  for (int i = 0; i < 10000; i++) sink += sum_positive(xs, n);
  clock_gettime(CLOCK_MONOTONIC, &b);
  printf("BENCH_WARMUP_MS=%.3f\n", elapsed_ms(a, b));

  clock_gettime(CLOCK_MONOTONIC, &a);
  for (int i = 0; i < 200000; i++) sink += sum_positive(xs, n);
  clock_gettime(CLOCK_MONOTONIC, &b);
  if (sink == 42) puts("impossible");
  printf("BENCH_RUNTIME_MS=%.3f\n", elapsed_ms(a, b));
  struct rusage usage;
  getrusage(RUSAGE_SELF, &usage);
  printf("BENCH_MEMORY_KB=%ld\n", usage.ru_maxrss);
  free(xs);
  return 0;
}
`)
}
