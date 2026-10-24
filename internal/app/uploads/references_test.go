package uploads

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReferenceRoundTripAndRestart(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, raw := range [][]byte{[]byte("hello"), {}} {
		ref, err := StoreReference(context.Background(), "café + notes.txt", "application/octet-stream", bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if !IsReferenceID(ref.UploadID) || ref.SizeBytes != int64(len(raw)) || ref.Filename != "café + notes.txt" {
			t.Fatalf("reference = %#v", ref)
		}
		// Every resolve reopens file metadata with no in-memory catalog/state.
		for i := 0; i < 2; i++ {
			got, path, content, err := ResolveReference(ref.UploadID)
			if err != nil || got != ref || !bytes.Equal(content, raw) {
				t.Fatalf("resolve = %#v %q %v", got, content, err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 || filepath.Dir(path) != Dir() {
				t.Fatalf("original = %s, info=%v, error=%v", path, info, err)
			}
		}
		metadata := filepath.Join(filepath.Dir(Dir()), "upload-refs", ref.UploadID+".json")
		info, err := os.Stat(metadata)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("metadata = %v, %v", info, err)
		}
	}
	assertNoStagedUploads(t)
	files, err := os.ReadDir(Dir())
	if err != nil || len(files) != 2 {
		t.Fatalf("originals = %v, %v", files, err)
	}
}

func TestReferenceRejectsCorruptionAndForgedPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, id, original, metadata string)
	}{
		{"digest", func(t *testing.T, _, original, _ string) { mustWriteReferenceTest(t, original, []byte("HELLO")) }},
		{"size", func(t *testing.T, _, original, _ string) { mustWriteReferenceTest(t, original, []byte("longer file")) }},
		{"original symlink", func(t *testing.T, _, original, _ string) {
			external := filepath.Join(t.TempDir(), "external")
			mustWriteReferenceTest(t, external, []byte("hello"))
			os.Remove(original)
			if err := os.Symlink(external, original); err != nil {
				t.Skip(err)
			}
		}},
		{"original directory", func(t *testing.T, _, original, _ string) {
			os.Remove(original)
			if err := os.Mkdir(original, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"metadata symlink", func(t *testing.T, _, _, metadata string) {
			raw, err := os.ReadFile(metadata)
			if err != nil {
				t.Fatal(err)
			}
			external := filepath.Join(t.TempDir(), "metadata")
			mustWriteReferenceTest(t, external, raw)
			os.Remove(metadata)
			if err := os.Symlink(external, metadata); err != nil {
				t.Skip(err)
			}
		}},
		{"metadata too large", func(t *testing.T, _, _, metadata string) {
			mustWriteReferenceTest(t, metadata, bytes.Repeat([]byte(" "), maxMetadataBytes+1))
		}},
		{"metadata forged path", func(t *testing.T, _, _, metadata string) {
			raw, _ := os.ReadFile(metadata)
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			m["path"] = "/etc/passwd"
			raw, _ = json.Marshal(m)
			mustWriteReferenceTest(t, metadata, raw)
		}},
		{"metadata wrong ID", func(t *testing.T, _, _, metadata string) {
			raw, _ := os.ReadFile(metadata)
			raw = bytes.Replace(raw, []byte(`"upload_id":"upl_`), []byte(`"upload_id":"bad_`), 1)
			mustWriteReferenceTest(t, metadata, raw)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			ref, err := StoreReference(context.Background(), "notes.txt", "text/plain", strings.NewReader("hello"))
			if err != nil {
				t.Fatal(err)
			}
			original := filepath.Join(Dir(), ref.UploadID)
			metadata := filepath.Join(filepath.Dir(Dir()), "upload-refs", ref.UploadID+".json")
			tc.edit(t, ref.UploadID, original, metadata)
			if _, _, _, err := ResolveReference(ref.UploadID); !errors.Is(err, ErrInvalidUpload) {
				t.Fatalf("corrupt reference error = %v", err)
			}
		})
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, id := range []string{"", "../secret", "/etc/passwd", "upl_" + strings.Repeat("a", 43) + "/x", "upl_" + strings.Repeat("+", 43)} {
		if _, _, _, err := ResolveReference(id); !errors.Is(err, ErrInvalidUpload) {
			t.Fatalf("forged %q: %v", id, err)
		}
	}
	missing := "upl_" + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if _, _, _, err := ResolveReference(missing); !errors.Is(err, ErrUploadNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func mustWriteReferenceTest(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoStagedUploads(t *testing.T) {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(filepath.Dir(Dir()), "upload-staging"))
	if err != nil || len(files) != 0 {
		t.Fatalf("staging = %v, %v", files, err)
	}
}

type failedUploadReader struct{}

func (failedUploadReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReferenceFailureAtomicity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reader   io.Reader
		canceled bool
		want     error
	}{
		{"overflow", io.LimitReader(zeroUploadReader{}, MaxAttachmentBytes+1), false, ErrUploadTooLarge},
		{"truncated", io.MultiReader(strings.NewReader("partial"), failedUploadReader{}), false, io.ErrUnexpectedEOF},
		{"canceled", strings.NewReader("data"), true, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			_, err := StoreReference(ctx, "data.bin", "application/octet-stream", tc.reader)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			assertNoStagedUploads(t)
			for _, name := range []string{"uploads", "upload-refs"} {
				files, err := os.ReadDir(filepath.Join(filepath.Dir(Dir()), name))
				if err != nil || len(files) != 0 {
					t.Fatalf("%s = %v, %v", name, files, err)
				}
			}
		})
	}
}

type zeroUploadReader struct{}

func (zeroUploadReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestReferenceExactLimit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ref, err := StoreReference(context.Background(), "exact.bin", "application/octet-stream", io.LimitReader(zeroUploadReader{}, MaxAttachmentBytes))
	if err != nil || ref.SizeBytes != MaxAttachmentBytes {
		t.Fatalf("exact limit = %#v, %v", ref, err)
	}
	if _, _, raw, err := ResolveReference(ref.UploadID); err != nil || len(raw) != MaxAttachmentBytes {
		t.Fatalf("resolve length=%d, error=%v", len(raw), err)
	}
}

func TestReferenceRejectsPrivateDirectorySymlinks(t *testing.T) {
	for _, dir := range []string{"upload-staging", "upload-refs", "uploads"} {
		t.Run(dir, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			data := filepath.Dir(Dir())
			if err := os.MkdirAll(data, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(data, dir)); err != nil {
				t.Skip(err)
			}
			if _, err := StoreReference(context.Background(), "a.txt", "text/plain", strings.NewReader("a")); !errors.Is(err, errUploadStoreUnavailable) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestReferenceFailurePreservesOtherRequestsAndCompletedOriginals(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ref, err := StoreReference(context.Background(), "kept.txt", "text/plain", strings.NewReader("live original"))
	if err != nil {
		t.Fatal(err)
	}
	staleStage := filepath.Join(filepath.Dir(Dir()), "upload-staging", "other-request-stage")
	mustWriteReferenceTest(t, staleStage, []byte("not owned by this request"))
	if _, err := StoreReference(context.Background(), "failed.bin", "application/octet-stream", failedUploadReader{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(staleStage); err != nil || string(raw) != "not owned by this request" {
		t.Fatalf("other staging file altered: %q, %v", raw, err)
	}
	if _, _, raw, err := ResolveReference(ref.UploadID); err != nil || string(raw) != "live original" {
		t.Fatalf("completed original altered: %q, %v", raw, err)
	}
}

func TestReferencePublicationFailureRemovesUnpublishedOriginal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Simulate a filesystem failure between staging and metadata publication.
	reader := &publicationFailureReader{t: t}
	if _, err := StoreReference(context.Background(), "fail.txt", "text/plain", reader); err == nil {
		t.Fatal("expected publication failure")
	}
	assertNoStagedUploads(t)
	entries, err := os.ReadDir(Dir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("unpublished originals leaked: %v, %v", entries, err)
	}
}

type publicationFailureReader struct {
	t    *testing.T
	done bool
}

func (r *publicationFailureReader) Read(b []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	metadata := filepath.Join(filepath.Dir(Dir()), "upload-refs")
	if err := os.Remove(metadata); err != nil {
		r.t.Fatal(err)
	}
	mustWriteReferenceTest(r.t, metadata, []byte("metadata directory unavailable"))
	return copy(b, []byte("complete original")), nil
}
