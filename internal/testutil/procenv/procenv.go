// Package procenv owns process-wide test environment isolation shared by command
// and application test binaries. Call Setup once, before m.Run.
package procenv

import (
	"fmt"
	"os"
	"path/filepath"
)

func Setup() (cleanup func(), err error) {
	// Root-command tests execute multiple commands in one process. Do not let a
	// developer's profiling environment make them contend for a fixed port.
	if err := os.Unsetenv("TERM_LLM_PPROF"); err != nil {
		fmt.Fprintf(os.Stderr, "unset TERM_LLM_PPROF: %v\n", err)
		return nil, err
	}
	// macOS exposes /var as a symlink to /private/var. Production workspace
	// paths are symlink-resolved, so create test temp directories beneath the
	// canonical root as well.
	if tempRoot, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		if err := os.Setenv("TMPDIR", filepath.Clean(tempRoot)); err != nil {
			fmt.Fprintf(os.Stderr, "set canonical TMPDIR: %v\n", err)
			return nil, err
		}
	}
	for key, value := range map[string]string{
		// No test may reach the network for a rate card, and none may have its
		// prices swapped by a refresh that lands mid-run.
		"TERM_LLM_OFFLINE":    "1",
		"GIT_CONFIG_GLOBAL":   "/dev/null",
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "Test User",
		"GIT_AUTHOR_EMAIL":    "test@example.com",
		"GIT_COMMITTER_NAME":  "Test User",
		"GIT_COMMITTER_EMAIL": "test@example.com",
	} {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintf(os.Stderr, "set %s: %v\n", key, err)
			return nil, err
		}
	}

	// Cost assertions read a cached model-price directory. Point it at a temp
	// directory so a developer's refreshed catalog cannot change the rates a
	// test prices with.
	cacheHome, err := os.MkdirTemp("", "term-llm-process-test-cache-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp XDG_CACHE_HOME: %v\n", err)
		return nil, err
	}
	if err := os.Setenv("XDG_CACHE_HOME", cacheHome); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_CACHE_HOME: %v\n", err)
		return nil, err
	}

	dataHome, err := os.MkdirTemp("", "term-llm-process-test-data-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp XDG_DATA_HOME: %v\n", err)
		return nil, err
	}
	if err := os.Setenv("XDG_DATA_HOME", dataHome); err != nil {
		fmt.Fprintf(os.Stderr, "set XDG_DATA_HOME: %v\n", err)
		return nil, err
	}
	return func() { _ = os.RemoveAll(dataHome) }, nil
}
