package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var fencedBlockRe = regexp.MustCompile("(?s)```(?:go|golang)?\\s*\\n(.*?)\\n```")

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
	if strings.Contains(response, "func ") || strings.Contains(response, "type ") {
		return response, nil
	}
	return "", fmt.Errorf("no Go code block found")
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

	args := []string{"test", ".", "-run", "TestGenerated", "-bench", "BenchmarkGenerated", "-benchmem", "-count", "1"}
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
	return ScoreResult{Pass: true, Score: 1, Details: perfSummary(out), Stdout: out, Stderr: errOut, GeneratedCode: code}
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
