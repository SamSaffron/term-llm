package guardian

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// FileEscalationLogger appends escalation records to a JSONL file so they can
// later be used to fine-tune the classifier. The file grows without bound; there
// is no rotation or retention, so the user rotates or deletes it.
type FileEscalationLogger struct {
	path   string
	warn   func(string)
	mu     sync.Mutex
	warned atomic.Bool
}

// NewFileEscalationLogger returns a logger that appends one JSON line per
// escalation to path, creating the parent directory when needed. It writes
// nothing until the first LogEscalation call and reports failures through its
// own warn signal, so a broken log never changes a review result.
func NewFileEscalationLogger(path string) *FileEscalationLogger {
	return &FileEscalationLogger{path: path, warn: func(message string) { log.Print(message) }}
}

// LogEscalation appends one JSON line. It never keeps the file open between
// writes, refuses to write through a non-regular file, and reports failures
// through its own warn signal at most once per logger.
func (l *FileEscalationLogger) LogEscalation(record Escalation) error {
	if l == nil {
		return nil
	}
	if err := l.append(record); err != nil {
		l.fail(err)
		return err
	}
	return nil
}

func (l *FileEscalationLogger) append(record Escalation) error {
	path := strings.TrimSpace(l.path)
	if path == "" {
		return fmt.Errorf("path is empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}
	// Encoding needs no lock: only the file itself is shared.
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	line = append(line, '\n')

	// The path check and the open must not interleave with another writer, or a
	// path swapped in between could redirect these records. O_NOFOLLOW is not
	// portable, so re-verify the opened descriptor against the path instead.
	l.mu.Lock()
	defer l.mu.Unlock()
	// These records contain transcript evidence. A symlink, directory or device
	// at this path could redirect them somewhere the user did not choose, so
	// refuse anything that is not already a regular file instead of following it.
	// Existing files keep their mode: they may be user-managed.
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect path: %w", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	if err := verifyOpenedEscalationPath(file, path); err != nil {
		file.Close()
		return err
	}
	// One write per record keeps concurrent appends line-intact.
	_, writeErr := file.Write(line)
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close: %w", closeErr)
	}
	return nil
}

// verifyOpenedEscalationPath confirms the descriptor still refers to the regular
// file that was inspected, so a path replaced between the check and the open
// cannot receive the record.
func verifyOpenedEscalationPath(file *os.File, path string) error {
	fdInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect open file: %w", err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect path: %w", err)
	}
	if !after.Mode().IsRegular() || !fdInfo.Mode().IsRegular() || !os.SameFile(after, fdInfo) {
		return fmt.Errorf("path changed during open")
	}
	return nil
}

func (l *FileEscalationLogger) fail(err error) {
	if !l.warned.CompareAndSwap(false, true) {
		return
	}
	warn := l.warn
	if warn == nil {
		warn = func(message string) { log.Print(message) }
	}
	warn(fmt.Sprintf("guardian: escalation log %s: %v", l.path, err))
}
