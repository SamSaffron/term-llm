package debuglog

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"time"
)

// TailOptions controls tail behavior
type TailOptions struct {
	Follow bool // Keep watching for new entries
}

// Tail outputs entries from a session file, optionally following for new entries
func Tail(ctx context.Context, filePath string, w io.Writer, opts TailOptions) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)

	// First, read all existing content
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		FormatTailEntry(w, line)
	}

	// If not following, we're done
	if !opts.Follow {
		return nil
	}

	// Follow mode: poll for new content
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Try to read more lines
			for {
				line, err := reader.ReadBytes('\n')
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				FormatTailEntry(w, line)
			}
		}
	}
}

// TailLatest tails the most recent session file
func TailLatest(ctx context.Context, dir string, w io.Writer, opts TailOptions) error {
	summary, err := GetMostRecentSession(dir)
	if err != nil {
		return err
	}
	if summary == nil {
		return &NoSessionsError{}
	}

	return Tail(ctx, summary.FilePath, w, opts)
}

// WatchDir watches the debug log directory for new sessions
// and returns when a new session file is created
func WatchDir(ctx context.Context, dir string) (string, error) {
	// Get initial list of files
	initial, err := listJSONLFiles(dir)
	if err != nil {
		return "", err
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			current, err := listJSONLFiles(dir)
			if err != nil {
				continue
			}

			// Check for new files
			for file := range current {
				if _, exists := initial[file]; !exists {
					return file, nil
				}
			}
		}
	}
}

// listJSONLFiles returns a map of JSONL files in the directory
func listJSONLFiles(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]struct{}), nil
		}
		return nil, err
	}

	files := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		files[entry.Name()] = struct{}{}
	}
	return files, nil
}

// NoSessionsError indicates no sessions were found
type NoSessionsError struct{}

func (e *NoSessionsError) Error() string {
	return "no debug sessions found"
}

// IsNoSessionsError checks if an error is a NoSessionsError
func IsNoSessionsError(err error) bool {
	_, ok := err.(*NoSessionsError)
	return ok
}
