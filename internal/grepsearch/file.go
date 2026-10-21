// Package grepsearch provides cancellation-aware file scanning for grep's Go fallback.
package grepsearch

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
)

// Scan reads a text file and visits matching lines with their source context.
// A nil context retains ordinary grep's original, synchronous scan behavior.
func Scan(ctx context.Context, path string, re *regexp.Regexp, maxMatches int, visit func([]string, int) error) error {
	file, err := os.Open(path)
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
