// Command precompressui gzips the generated web UI bundle in place so the
// binary embeds compressed bytes and the server can send them unchanged. It
// runs as the last step of `npm --prefix frontend run build`.
//
// Each compressible file X becomes X.gz and X is removed; files that gzip
// would not shrink are left alone. Output is deterministic for a given Go
// toolchain: the gzip header carries no name, timestamp, or host OS.
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const distPath = "internal/serveui/static/dist"

// compressibleExtensions are the text formats Vite emits. Fonts and images
// are already compressed.
var compressibleExtensions = map[string]bool{
	".css":  true,
	".html": true,
	".js":   true,
	".json": true,
	".mjs":  true,
	".svg":  true,
	".txt":  true,
}

type result struct {
	files, rawBytes, storedBytes int
}

func main() {
	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	res, err := compressDir(filepath.Join(root, filepath.FromSlash(distPath)))
	if err != nil {
		fatal(err)
	}
	fmt.Printf("UI bundle precompressed: %d files, %d -> %d bytes\n", res.files, res.rawBytes, res.storedBytes)
}

// compressDir replaces every compressible file under dir with its gzip form.
func compressDir(dir string) (result, error) {
	var res result
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		if !compressibleExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		compressed, err := gzipBytes(raw)
		if err != nil {
			return fmt.Errorf("compress %s: %w", path, err)
		}
		if len(compressed) >= len(raw) {
			// Keep tiny files raw; also drop a stale .gz so lookups stay unambiguous.
			if err := os.Remove(path + ".gz"); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		if err := os.WriteFile(path+".gz", compressed, 0o644); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		res.files++
		res.rawBytes += len(raw)
		res.storedBytes += len(compressed)
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("precompress %s: %w", dir, err)
	}
	return res, nil
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// repoRoot returns the module root containing this command.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "precompressui:", err)
	os.Exit(1)
}
