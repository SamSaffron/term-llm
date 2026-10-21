// Package grepsearch provides cancellation-aware file scanning for grep's Go fallback.
package grepsearch

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// ErrNotRegular reports that a cancellable scan refused a non-regular file
// (FIFO, socket, device or directory) that could block or never end.
var ErrNotRegular = errors.New("not a regular file")

// openRegular is the cancellable-scan open path. It rejects non-regular files
// before opening (following symlinks) and again on the opened descriptor.
func openRegular(ctx context.Context, path string) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	file, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// openRegularFile verifies the opened descriptor is regular, closing it
// otherwise. Unix opens are non-blocking to cover the precheck/open FIFO race.
func openRegularFile(path string) (*os.File, error) {
	file, err := openNonblock(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, ErrNotRegular
	}
	return file, nil
}

// Scan reads a text file and visits matching lines with their source context.
// A nil context retains ordinary grep's original, synchronous scan behavior.
func Scan(ctx context.Context, path string, re *regexp.Regexp, maxMatches int, visit func([]string, int) error) error {
	var file *os.File
	var err error
	if ctx == nil {
		file, err = os.Open(path)
	} else {
		file, err = openRegular(ctx, path)
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if ctx != nil {
		stop := context.AfterFunc(ctx, func() { _ = file.Close() })
		defer stop()
	}
	check := func() error {
		if ctx != nil {
			return ctx.Err()
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	buf := make([]byte, 512)
	n, err := file.Read(buf)
	if err != nil && n == 0 {
		return err
	}
	contentType := http.DetectContentType(buf[:n])
	if !strings.HasPrefix(contentType, "text/") && !strings.Contains(contentType, "json") && !strings.Contains(contentType, "xml") {
		return fmt.Errorf("binary file")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if err := check(); err != nil {
			return err
		}
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	count := 0
	for index, line := range lines {
		if err := check(); err != nil {
			return err
		}
		if !re.MatchString(line) {
			continue
		}
		if err := visit(lines, index); err != nil {
			return err
		}
		count++
		if count >= maxMatches {
			break
		}
	}
	return check()
}
