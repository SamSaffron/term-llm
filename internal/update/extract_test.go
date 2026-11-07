package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveTestEntry struct {
	name string
	kind byte
	body string
}

func testReleaseArchive(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Mode: 0o755}
		if entry.kind == tar.TypeSymlink || entry.kind == tar.TypeLink {
			header.Linkname = "../outside"
		} else {
			header.Size = int64(len(entry.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			if _, err := tw.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinarySafety(t *testing.T) {
	binary := archiveTestEntry{releaseBinaryName(), tar.TypeReg, "inert binary fixture"}
	for _, tc := range []struct {
		name    string
		entries []archiveTestEntry
		ok      bool
	}{
		{"valid", []archiveTestEntry{binary, {"LICENSE", tar.TypeReg, "license"}}, true},
		{"unrelated traversal never written", []archiveTestEntry{{"../outside", tar.TypeReg, "evil"}, binary}, true},
		{"missing", []archiveTestEntry{{"LICENSE", tar.TypeReg, "license"}}, false},
		{"nested executable", []archiveTestEntry{{"nested/" + releaseBinaryName(), tar.TypeReg, binary.body}}, false},
		{"absolute executable", []archiveTestEntry{{"/" + releaseBinaryName(), tar.TypeReg, binary.body}}, false},
		{"traversal executable", []archiveTestEntry{{"../" + releaseBinaryName(), tar.TypeReg, binary.body}}, false},
		{"symlink", []archiveTestEntry{{releaseBinaryName(), tar.TypeSymlink, ""}}, false},
		{"hardlink", []archiveTestEntry{{releaseBinaryName(), tar.TypeLink, ""}}, false},
		{"empty", []archiveTestEntry{{releaseBinaryName(), tar.TypeReg, ""}}, false},
		{"duplicate", []archiveTestEntry{binary, binary}, false},
		{"binary oversize", []archiveTestEntry{{releaseBinaryName(), tar.TypeReg, strings.Repeat("x", 65)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archivePath := filepath.Join(dir, "release.tar.gz")
			if err := os.WriteFile(archivePath, testReleaseArchive(t, tc.entries), 0o600); err != nil {
				t.Fatal(err)
			}
			dest, err := os.CreateTemp(dir, "dest")
			if err != nil {
				t.Fatal(err)
			}
			defer dest.Close()
			err = extractBinaryWithLimits(archivePath, dest, 64, 16<<10)
			if (err == nil) != tc.ok {
				t.Fatalf("extract() = %v; success want %v", err, tc.ok)
			}
			if tc.ok {
				got, err := os.ReadFile(dest.Name())
				if err != nil || string(got) != binary.body {
					t.Fatalf("extracted = %q, %v", got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside")); !os.IsNotExist(err) {
				t.Fatalf("archive wrote outside extraction destination: %v", err)
			}
		})
	}
}

func TestExtractRejectsDecompressionBombAndCorruption(t *testing.T) {
	valid := testReleaseArchive(t, []archiveTestEntry{{releaseBinaryName(), tar.TypeReg, "inert"}})
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 0xff // Break gzip CRC without touching the tar.
	bomb := testReleaseArchive(t, []archiveTestEntry{
		{releaseBinaryName(), tar.TypeReg, "inert"},
		{"padding", tar.TypeReg, strings.Repeat("x", 32<<10)},
	})
	// Also bound bytes after the tar end marker and concatenated gzip members.
	trailingBomb := append(bytes.Clone(valid), testReleaseArchive(t, []archiveTestEntry{{"padding", tar.TypeReg, strings.Repeat("x", 32<<10)}})...)
	for _, tc := range []struct {
		name    string
		archive []byte
	}{
		{"CRC mismatch", corrupt}, {"truncated gzip", valid[:len(valid)-4]},
		{"decompression bomb", bomb}, {"concatenated bomb", trailingBomb},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "release.tar.gz")
			if err := os.WriteFile(path, tc.archive, 0o600); err != nil {
				t.Fatal(err)
			}
			dest, err := os.CreateTemp(dir, "dest")
			if err != nil {
				t.Fatal(err)
			}
			defer dest.Close()
			if err := extractBinaryWithLimits(path, dest, 64, 8<<10); err == nil {
				t.Fatal("accepted corrupt/oversize archive")
			}
		})
	}
}

func testReleaseZip(t *testing.T, entries []archiveTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, entry := range entries {
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractZipReleaseBinary(t *testing.T) {
	tests := []struct {
		name    string
		entries []archiveTestEntry
		max     int64
		want    string
		wantErr bool
	}{
		{name: "root executable", entries: []archiveTestEntry{{name: "README.md", body: "docs"}, {name: "term-llm.exe", body: "MZbinary"}}, max: 64, want: "MZbinary"},
		{name: "nested executable is ignored", entries: []archiveTestEntry{{name: "bin/term-llm.exe", body: "nope"}}, max: 64, wantErr: true},
		{name: "missing executable", entries: []archiveTestEntry{{name: "README.md", body: "docs"}}, max: 64, wantErr: true},
		{name: "duplicate executable", entries: []archiveTestEntry{{name: "term-llm.exe", body: "a"}, {name: "term-llm.exe", body: "b"}}, max: 64, wantErr: true},
		{name: "oversized executable", entries: []archiveTestEntry{{name: "term-llm.exe", body: "too large"}}, max: 4, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "term-llm_1.2.3_windows_amd64.zip")
			if err := os.WriteFile(path, testReleaseZip(t, tc.entries), 0o600); err != nil {
				t.Fatal(err)
			}
			dest, err := os.CreateTemp(dir, "dest")
			if err != nil {
				t.Fatal(err)
			}
			defer dest.Close()
			err = extractBinaryWithLimitsNamed(path, dest, tc.max, 1<<20, "term-llm.exe")
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected extraction error")
				}
				return
			}
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			data, err := os.ReadFile(dest.Name())
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Fatalf("extracted %q, want %q", data, tc.want)
			}
		})
	}
}
