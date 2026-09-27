// Package runtimeoutput routes application diagnostics away from a terminal owned
// by an interactive UI. It never redirects the process's stdout or stderr:
// renderer output, CLI output, and intentional subprocess hand-offs stay intact.
package runtimeoutput

import (
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

var sink struct {
	sync.Mutex
	file   *os.File
	logger *slog.Logger
}

var ErrAlreadyActive = errors.New("interactive diagnostic log already active")

// Start opens the interactive diagnostic log before a UI takes ownership of the
// terminal. Call Close only after its background workers have stopped. The
// caller must not run two independently owned terminal UIs at the same time.
func Start(dir string) (func(), error) {
	sink.Lock()
	defer sink.Unlock()
	if sink.logger != nil {
		return nil, ErrAlreadyActive
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("diagnostic directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create diagnostic directory: %w", err)
	}
	path := filepath.Join(dir, "tui.log")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("interactive diagnostic log is not a regular file: %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect interactive diagnostic log: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open interactive diagnostic log: %w", err)
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("restrict interactive diagnostic log: %w", err)
	}
	sink.file = f
	sink.logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func() {
		sink.Lock()
		defer sink.Unlock()
		if sink.file == f {
			sink.logger = nil
			sink.file = nil
			_ = f.Close()
		}
	}, nil
}

// StartBestEffort keeps interactive output isolated even when the diagnostic
// directory cannot be opened. The caller must display the returned warning
// before the UI starts rendering. An overlapping UI remains an error.
func StartBestEffort(dir string) (close func(), warning error, err error) {
	close, err = Start(dir)
	if err == nil || errors.Is(err, ErrAlreadyActive) {
		return close, nil, err
	}
	warning = err
	sink.Lock()
	defer sink.Unlock()
	if sink.logger != nil {
		return nil, nil, ErrAlreadyActive
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	sink.logger = logger
	return func() {
		sink.Lock()
		defer sink.Unlock()
		if sink.logger == logger {
			sink.logger = nil
		}
	}, warning, nil
}

// Active reports whether a terminal UI currently owns the diagnostic sink.
func Active() bool {
	sink.Lock()
	defer sink.Unlock()
	return sink.logger != nil
}

// Debug and Warn preserve slog behavior outside an interactive UI. Inside it,
// both levels go to the same restricted diagnostic file, never a footer toast.
func Debug(message string, args ...any) { record(slog.LevelDebug, message, args...) }
func Warn(message string, args ...any)  { record(slog.LevelWarn, message, args...) }
func Info(message string, args ...any)  { record(slog.LevelInfo, message, args...) }
func Error(message string, args ...any) { record(slog.LevelError, message, args...) }

func record(level slog.Level, message string, args ...any) {
	sink.Lock()
	if sink.logger != nil {
		sink.logger.Log(nil, level, message, args...)
		sink.Unlock()
		return
	}
	sink.Unlock()
	switch level {
	case slog.LevelDebug:
		slog.Debug(message, args...)
	case slog.LevelInfo:
		slog.Info(message, args...)
	case slog.LevelError:
		slog.Error(message, args...)
	default:
		slog.Warn(message, args...)
	}
}

// Printf writes raw formatted bytes to the active sink atomically. It retains
// historical stderr behavior outside a terminal UI.
func Printf(format string, args ...any) {
	sink.Lock()
	defer sink.Unlock()
	if sink.logger != nil {
		if sink.file != nil {
			_, _ = fmt.Fprintf(sink.file, format, args...)
		}
	} else {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// Writer routes optional streaming performance telemetry to the active log.
// It preserves direct stderr output in non-interactive mode.
func Writer() io.Writer { return writer{} }

type writer struct{}

func (writer) Write(p []byte) (int, error) {
	sink.Lock()
	defer sink.Unlock()
	if sink.logger != nil {
		if sink.file != nil {
			return sink.file.Write(p)
		}
		return len(p), nil
	}
	return os.Stderr.Write(p)
}

// Fallback preserves a caller's CLI writer before/after the UI, but prevents
// detached background workers from writing to it while another UI is active.
// As with Start, callers must finish synchronous CLI writes before giving a
// new UI ownership of the terminal.
func Fallback(out io.Writer) io.Writer { return fallback{out: out} }

type fallback struct{ out io.Writer }

func (w fallback) Write(p []byte) (int, error) {
	sink.Lock()
	if sink.logger != nil {
		if sink.file != nil {
			n, err := sink.file.Write(p)
			sink.Unlock()
			return n, err
		}
		sink.Unlock()
		return len(p), nil
	}
	sink.Unlock()
	if w.out == nil {
		return len(p), nil
	}
	// A caller's writer can itself be a Fallback (or invoke arbitrary code).
	// Never hold the global sink lock while dispatching outside the UI.
	return w.out.Write(p)
}

// Logf retains standard log behavior (including its configured output) outside
// a terminal UI, while routing internal runtime diagnostics to the safe sink.
func Logf(format string, args ...any) {
	sink.Lock()
	if sink.logger != nil {
		sink.logger.Info(fmt.Sprintf(format, args...))
		sink.Unlock()
		return
	}
	sink.Unlock()
	log.Printf(format, args...)
}
