package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var fencedBlockRe = regexp.MustCompile("(?s)```(?:go|golang|javascript|js|node|ruby|rb|python|py|asm|assembly|x86_64-assembly|zig|s)?\\s*\\n(.*?)\\n```")
var goBenchRe = regexp.MustCompile(`BenchmarkGenerated\S*\s+\d+\s+([0-9.]+)\s+ns/op(?:\s+([0-9.]+)\s+B/op)?(?:\s+([0-9.]+)\s+allocs/op)?`)
var runtimeBenchRe = regexp.MustCompile(`BENCH_RUNTIME_MS=([0-9.]+)`)
var warmupBenchRe = regexp.MustCompile(`BENCH_WARMUP_MS=([0-9.]+)`)
var memoryBenchRe = regexp.MustCompile(`BENCH_MEMORY_KB=([0-9.]+)`)

func extractCode(response string) (string, error) {
	response = strings.TrimSpace(response)
	if response == "" {
		return "", fmt.Errorf("empty response")
	}
	matches := fencedBlockRe.FindStringSubmatch(response)
	if len(matches) >= 2 {
		return strings.TrimSpace(matches[1]), nil
	}
	// Accept raw code as a convenience for providers that obey "no prose" literally.
	if strings.Contains(response, "func ") || strings.Contains(response, "type ") || strings.Contains(response, "export function") || strings.Contains(response, "module.exports") || strings.Contains(response, "def ") || strings.Contains(response, "class ") || strings.Contains(response, ".globl") || strings.Contains(response, ".global") || strings.Contains(response, "export fn") {
		return response, nil
	}
	return "", fmt.Errorf("no code block found")
}

func scoreGoFunction(response string, timeout time.Duration, testBody string, imports ...string) ScoreResult {
	return scoreGo(response, timeout, false, testBody, imports...)
}

func scoreGoFunctionWithRace(response string, timeout time.Duration, testBody string, imports ...string) ScoreResult {
	return scoreGo(response, timeout, true, testBody, imports...)
}

func scoreGo(response string, timeout time.Duration, race bool, testBody string, imports ...string) ScoreResult {
	code, err := extractCode(response)
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: response}
	}
	dir, err := os.MkdirTemp("", "term-llm-codegen-bench-*")
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer os.RemoveAll(dir)

	if !strings.Contains(code, "package ") {
		code = "package main\n\n" + code
	}
	if err := os.WriteFile(filepath.Join(dir, "solution.go"), []byte(code), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}

	testSource := buildTestSource(testBody, imports...)
	if err := os.WriteFile(filepath.Join(dir, "solution_test.go"), []byte(testSource), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module benchsolution\n\ngo 1.22\n"), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}

	args := []string{"test", "-v", ".", "-run", "TestGenerated", "-bench", "BenchmarkGenerated", "-benchmem", "-count", "1"}
	if race {
		args = append([]string{"test", "-race"}, args[1:]...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	out := stdout.String()
	errOut := stderr.String()
	if ctx.Err() == context.DeadlineExceeded {
		return ScoreResult{Pass: false, Score: 0, Details: "scoring timed out", Stdout: out, Stderr: errOut, GeneratedCode: code}
	}
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: "tests failed", Stdout: out, Stderr: errOut, GeneratedCode: code}
	}
	metrics := parseGoBench(out)
	markers := parseRuntimeBench(out)
	if markers.RuntimeMS > 0 {
		metrics.RuntimeMS = markers.RuntimeMS
	}
	if markers.WarmupMS > 0 {
		metrics.WarmupMS = markers.WarmupMS
	}
	if markers.MemoryKB > 0 {
		metrics.MemoryKB = markers.MemoryKB
	}
	return ScoreResult{Pass: true, Score: 1, Details: perfSummary(out), Stdout: out, Stderr: errOut, GeneratedCode: code, Metrics: metrics}
}

func scoreNode(response string, timeout time.Duration, testSource string) ScoreResult {
	code, err := extractCode(response)
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: response}
	}
	dir, err := os.MkdirTemp("", "term-llm-codegen-bench-node-*")
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "solution.mjs"), []byte(code), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	if err := os.WriteFile(filepath.Join(dir, "solution.test.mjs"), []byte(testSource), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "--test", "solution.test.mjs")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	out := stdout.String()
	errOut := stderr.String()
	if ctx.Err() == context.DeadlineExceeded {
		return ScoreResult{Pass: false, Score: 0, Details: "scoring timed out", Stdout: out, Stderr: errOut, GeneratedCode: code}
	}
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: "tests failed", Stdout: out, Stderr: errOut, GeneratedCode: code}
	}
	metrics := parseRuntimeBench(out)
	detail := "ok"
	if metrics.RuntimeMS > 0 {
		detail = fmt.Sprintf("runtime %.2f ms", metrics.RuntimeMS)
	}
	return ScoreResult{Pass: true, Score: 1, Details: detail, Stdout: out, Stderr: errOut, GeneratedCode: code, Metrics: metrics}
}

func scoreRuby(response string, timeout time.Duration, testSource string) ScoreResult {
	code, err := extractCode(response)
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: response}
	}
	dir, err := os.MkdirTemp("", "term-llm-codegen-bench-ruby-*")
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "solution.rb"), []byte(code), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	if err := os.WriteFile(filepath.Join(dir, "solution_test.rb"), []byte(testSource), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	return runScriptScore(timeout, dir, code, "ruby", "solution_test.rb")
}

func scorePython(response string, timeout time.Duration, testSource string) ScoreResult {
	code, err := extractCode(response)
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: response}
	}
	dir, err := os.MkdirTemp("", "term-llm-codegen-bench-python-*")
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "solution.py"), []byte(code), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	if err := os.WriteFile(filepath.Join(dir, "solution_test.py"), []byte(testSource), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	return runScriptScore(timeout, dir, code, "python3", "solution_test.py")
}

func scoreAssembly(response string, timeout time.Duration, testSource string) ScoreResult {
	code, err := extractCode(response)
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: response}
	}
	dir, err := os.MkdirTemp("", "term-llm-codegen-bench-asm-*")
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "solution.s"), []byte(code), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	if err := os.WriteFile(filepath.Join(dir, "test.c"), []byte(testSource), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", "gcc -O2 -Wall -Wextra -no-pie solution.s test.c -o test && ./test")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return scriptResult(ctx, err, stdout.String(), stderr.String(), code)
}

func scoreZig(response string, timeout time.Duration, testSource string) ScoreResult {
	if _, err := exec.LookPath("zig"); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: "zig executable not found", GeneratedCode: response}
	}
	code, err := extractCode(response)
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: response}
	}
	dir, err := os.MkdirTemp("", "term-llm-codegen-bench-zig-*")
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "solution.zig"), []byte(code), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	if err := os.WriteFile(filepath.Join(dir, "test.c"), []byte(testSource), 0o644); err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: err.Error(), GeneratedCode: code}
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", "zig build-lib -O ReleaseFast -dynamic -femit-bin=libsolution.so solution.zig && gcc -O2 -Wall -Wextra test.c -L. -lsolution -Wl,-rpath,. -o test && ./test")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	return scriptResult(ctx, err, stdout.String(), stderr.String(), code)
}

func runScriptScore(timeout time.Duration, dir, code, name string, args ...string) ScoreResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return scriptResult(ctx, err, stdout.String(), stderr.String(), code)
}

func scriptResult(ctx context.Context, err error, out, errOut, code string) ScoreResult {
	if ctx.Err() == context.DeadlineExceeded {
		return ScoreResult{Pass: false, Score: 0, Details: "scoring timed out", Stdout: out, Stderr: errOut, GeneratedCode: code}
	}
	if err != nil {
		return ScoreResult{Pass: false, Score: 0, Details: "tests failed", Stdout: out, Stderr: errOut, GeneratedCode: code}
	}
	metrics := parseRuntimeBench(out)
	detail := "ok"
	if metrics.RuntimeMS > 0 {
		detail = fmt.Sprintf("runtime %.2f ms", metrics.RuntimeMS)
	}
	return ScoreResult{Pass: true, Score: 1, Details: detail, Stdout: out, Stderr: errOut, GeneratedCode: code, Metrics: metrics}
}

func buildTestSource(testBody string, imports ...string) string {
	seen := map[string]bool{"testing": true}
	all := []string{"testing"}
	for _, imp := range imports {
		imp = strings.TrimSpace(imp)
		if imp != "" && !seen[imp] {
			seen[imp] = true
			all = append(all, imp)
		}
	}
	var b strings.Builder
	b.WriteString("package main\n\nimport (\n")
	for _, imp := range all {
		fmt.Fprintf(&b, "\t%q\n", imp)
	}
	b.WriteString(")\n")
	b.WriteString(testBody)
	return b.String()
}

func perfSummary(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "BenchmarkGenerated") {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	return "ok"
}

func parseGoBench(out string) ScoreMetrics {
	match := goBenchRe.FindStringSubmatch(out)
	if len(match) == 0 {
		return ScoreMetrics{}
	}
	metrics := ScoreMetrics{}
	metrics.NSPerOp, _ = strconv.ParseFloat(match[1], 64)
	if len(match) > 2 {
		metrics.BytesPerOp, _ = strconv.ParseFloat(match[2], 64)
	}
	if len(match) > 3 {
		metrics.AllocsPerOp, _ = strconv.ParseFloat(match[3], 64)
	}
	if metrics.NSPerOp > 0 {
		metrics.RuntimeMS = metrics.NSPerOp / 1_000_000
	}
	return metrics
}

func parseRuntimeBench(out string) ScoreMetrics {
	metrics := ScoreMetrics{}
	if match := runtimeBenchRe.FindStringSubmatch(out); len(match) > 0 {
		metrics.RuntimeMS, _ = strconv.ParseFloat(match[1], 64)
	}
	if match := warmupBenchRe.FindStringSubmatch(out); len(match) > 0 {
		metrics.WarmupMS, _ = strconv.ParseFloat(match[1], 64)
	}
	if match := memoryBenchRe.FindStringSubmatch(out); len(match) > 0 {
		metrics.MemoryKB, _ = strconv.ParseFloat(match[1], 64)
	}
	return metrics
}
