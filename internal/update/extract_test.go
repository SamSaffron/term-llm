package update

import (
	"archive/tar"
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
	binary := archiveTestEntry{RepoName, tar.TypeReg, "inert binary fixture"}
	for _, tc := range []struct {
		name    string
		entries []archiveTestEntry
		ok      bool
	}{
		{"valid", []archiveTestEntry{binary, {"LICENSE", tar.TypeReg, "license"}}, true},
		{"unrelated traversal never written", []archiveTestEntry{{"../outside", tar.TypeReg, "evil"}, binary}, true},
		{"missing", []archiveTestEntry{{"LICENSE", tar.TypeReg, "license"}}, false},
		{"nested executable", []archiveTestEntry{{"nested/" + RepoName, tar.TypeReg, binary.body}}, false},
		{"absolute executable", []archiveTestEntry{{"/" + RepoName, tar.TypeReg, binary.body}}, false},
		{"traversal executable", []archiveTestEntry{{"../" + RepoName, tar.TypeReg, binary.body}}, false},
		{"symlink", []archiveTestEntry{{RepoName, tar.TypeSymlink, ""}}, false},
		{"hardlink", []archiveTestEntry{{RepoName, tar.TypeLink, ""}}, false},
		{"empty", []archiveTestEntry{{RepoName, tar.TypeReg, ""}}, false},
		{"duplicate", []archiveTestEntry{binary, binary}, false},
		{"binary oversize", []archiveTestEntry{{RepoName, tar.TypeReg, strings.Repeat("x", 65)}}, false},
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
	valid := testReleaseArchive(t, []archiveTestEntry{{RepoName, tar.TypeReg, "inert"}})
	corrupt := bytes.Clone(valid)
	corrupt[len(corrupt)-8] ^= 0xff // Break gzip CRC without touching the tar.
	bomb := testReleaseArchive(t, []archiveTestEntry{
		{RepoName, tar.TypeReg, "inert"},
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
