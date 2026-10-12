package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompressDirReplacesCompressibleFiles(t *testing.T) {
	dir := t.TempDir()
	js := []byte(strings.Repeat("export const value = 'term-llm';\n", 200))
	font := bytes.Repeat([]byte{0x77, 0x4f, 0x46, 0x32}, 200)
	tiny := []byte("x")
	write := func(name string, data []byte) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("chunks/app-abcdefgh.js", js)
	write("assets/font-abcdefgh.woff2", font)
	write("chunks/tiny-abcdefgh.js", tiny)
	write("chunks/tiny-abcdefgh.js.gz", []byte("stale"))

	res, err := compressDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.files != 1 || res.rawBytes != len(js) {
		t.Fatalf("result = %+v, want one compressed file of %d bytes", res, len(js))
	}

	assertMissing(t, filepath.Join(dir, "chunks/app-abcdefgh.js"))
	stored, err := os.ReadFile(filepath.Join(dir, "chunks/app-abcdefgh.js.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if got := gunzip(t, stored); !bytes.Equal(got, js) {
		t.Fatal("compressed file does not round-trip")
	}
	zr, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		t.Fatal(err)
	}
	if !zr.ModTime.IsZero() || zr.Name != "" || zr.OS != 255 {
		t.Fatalf("gzip header is not deterministic: mtime=%v name=%q os=%d", zr.ModTime, zr.Name, zr.OS)
	}

	if got, _ := os.ReadFile(filepath.Join(dir, "assets/font-abcdefgh.woff2")); !bytes.Equal(got, font) {
		t.Fatal("incompressible font was modified")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "chunks/tiny-abcdefgh.js")); !bytes.Equal(got, tiny) {
		t.Fatal("file gzip cannot shrink must stay raw")
	}
	assertMissing(t, filepath.Join(dir, "chunks/tiny-abcdefgh.js.gz"))

	// A second run is a no-op and produces identical bytes.
	again, err := compressDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.files != 0 {
		t.Fatalf("second run compressed %d files, want 0", again.files)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "chunks/app-abcdefgh.js.gz")); !bytes.Equal(got, stored) {
		t.Fatal("second run changed stored bytes")
	}
}

func TestGzipBytesIsDeterministic(t *testing.T) {
	data := []byte(strings.Repeat("deterministic ", 500))
	first, err := gzipBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := gzipBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("gzip output differs between runs")
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should not exist (err=%v)", path, err)
	}
}

func gunzip(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
