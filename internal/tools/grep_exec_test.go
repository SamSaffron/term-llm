package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestBuildRipgrepArgs_DelimitsPatternAndPath(t *testing.T) {
	for _, pattern := range []string{"needle", "--pre=bash", "--follow", "--ignore-file", "-e", "--"} {
		for _, filesOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/filesOnly=%v", pattern, filesOnly), func(t *testing.T) {
				args := buildRipgrepArgs(GrepArgs{Pattern: pattern, FilesWithMatches: filesOnly}, "--search-path", 2, 100)
				want := []string{"-e", pattern, "--", "--search-path"}
				if got := args[len(args)-len(want):]; !reflect.DeepEqual(got, want) {
					t.Fatalf("ripgrep argument suffix = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestExecuteRipgrep_OptionLikePatterns(t *testing.T) {
	if !ripgrepAvailable() {
		t.Skip("ripgrep not installed")
	}
	// Even before the fix, this inert nonexistent preprocessor cannot execute
	// anything. Chdir also confines any accidental implicit search to the fixture.
	dir := t.TempDir()
	t.Chdir(dir)
	patterns := []string{"--pre=term-llm-nonexistent-preprocessor", "--follow", "--ignore-file", "-e", "--"}
	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			path := filepath.Join(dir, "sample.txt")
			if err := os.WriteFile(path, []byte(pattern+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, filesOnly := range []bool{false, true} {
				tool := NewGrepTool(nil, DefaultOutputLimits())
				result, err := tool.executeRipgrep(context.Background(), GrepArgs{Pattern: pattern, FilesWithMatches: filesOnly}, path, 2, 100)
				if err != nil {
					t.Fatalf("filesOnly=%v: %v", filesOnly, err)
				}
				if len(result.matches) != 1 || result.matches[0].FilePath != path {
					t.Fatalf("filesOnly=%v: matches = %+v, want one match in %s", filesOnly, result.matches, path)
				}
			}
		})
	}
}

func TestExecuteRipgrep_UsesSearchWorkingDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ripgrep uses a POSIX shell")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "rg"), []byte("#!/bin/sh\npwd -P\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	dir := canonicalTempDir(t)
	file := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(file, []byte("needle\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, file} {
		t.Run(path, func(t *testing.T) {
			// The fake rg returns its CWD as a filename, exposing the actual
			// child-process directory without changing the test process CWD.
			tool := NewGrepTool(nil, DefaultOutputLimits())
			result, err := tool.executeRipgrep(context.Background(), GrepArgs{Pattern: "needle", FilesWithMatches: true}, path, 2, 100)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.matches) != 1 || strings.TrimSpace(result.matches[0].FilePath) != dir {
				t.Fatalf("child CWD = %+v, want %s", result.matches, dir)
			}
		})
	}
}
