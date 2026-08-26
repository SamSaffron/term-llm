package filetrack

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, opts Options) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "file_history.db"), opts)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestOpenCreatesPrivateSQLiteFiles(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "nested", "file_history.db")
	store, err := Open(dbPath, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			// SQLite may not create every sidecar on every platform/version.
			continue
		}
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != 0600 {
			t.Fatalf("%s mode = %o, want 0600", path, got)
		}
	}
}

func TestRecordChangeKinds(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()

	tests := []struct {
		name     string
		rec      ChangeRecord
		wantKind string
		wantNil  bool
	}{
		{
			name: "create",
			rec: ChangeRecord{
				SessionID: "s1", Path: "/tmp/a.txt",
				After: []byte("hello\n"), BeforeMissing: true,
			},
			wantKind: KindCreate,
		},
		{
			name: "modify",
			rec: ChangeRecord{
				SessionID: "s1", Path: "/tmp/a.txt",
				Before: []byte("hello\n"), After: []byte("hello world\n"),
			},
			wantKind: KindModify,
		},
		{
			name: "delete",
			rec: ChangeRecord{
				SessionID: "s1", Path: "/tmp/a.txt",
				Before: []byte("hello world\n"), AfterMissing: true,
			},
			wantKind: KindDelete,
		},
		{
			name: "identical content is a no-op",
			rec: ChangeRecord{
				SessionID: "s1", Path: "/tmp/b.txt",
				Before: []byte("same\n"), After: []byte("same\n"),
			},
			wantNil: true,
		},
		{
			name: "missing to missing is a no-op",
			rec: ChangeRecord{
				SessionID: "s1", Path: "/tmp/c.txt",
				BeforeMissing: true, AfterMissing: true,
			},
			wantNil: true,
		},
		{
			name:    "empty session is a no-op",
			rec:     ChangeRecord{Path: "/tmp/d.txt", After: []byte("x"), BeforeMissing: true},
			wantNil: true,
		},
	}

	var lastSeq int64
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			change, err := store.RecordChange(ctx, tt.rec)
			if err != nil {
				t.Fatalf("RecordChange: %v", err)
			}
			if tt.wantNil {
				if change != nil {
					t.Fatalf("expected nil change, got %+v", change)
				}
				return
			}
			if change == nil {
				t.Fatal("expected change, got nil")
			}
			if change.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", change.Kind, tt.wantKind)
			}
			if change.Seq <= lastSeq {
				t.Fatalf("seq = %d, want > %d", change.Seq, lastSeq)
			}
			lastSeq = change.Seq
		})
	}
}

func TestRecordChangeConcurrentSeqs(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.RecordChange(ctx, ChangeRecord{
				SessionID:     "parallel-session",
				Path:          fmt.Sprintf("/tmp/%02d.txt", i),
				BeforeMissing: true,
				After:         []byte(fmt.Sprintf("content %d\n", i)),
			})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("RecordChange: %v", err)
		}
	}

	rows, err := store.db.QueryContext(ctx, "SELECT seq FROM file_changes WHERE session_id = ? ORDER BY seq", "parallel-session")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := make(map[int]bool, n)
	for rows.Next() {
		var seq int
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		seen[seq] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for seq := 1; seq <= n; seq++ {
		if !seen[seq] {
			t.Fatalf("missing seq %d in %v", seq, seen)
		}
	}
}

func TestRelativeAndAbsolutePathsMerge(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	dir := t.TempDir()
	t.Chdir(dir)

	abs := filepath.Join(dir, "dup.txt")
	if _, err := store.RecordChange(ctx, ChangeRecord{
		SessionID:     "s1",
		Path:          "dup.txt",
		BeforeMissing: true,
		After:         []byte("one\n"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s1",
		Path:      abs,
		Before:    []byte("one\n"),
		After:     []byte("two\n"),
	}); err != nil {
		t.Fatal(err)
	}

	changes, err := store.ListSessionChanges(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != abs || changes[0].Kind != KindCreate {
		t.Fatalf("changes = %+v, want one canonical create for %s", changes, abs)
	}

	content, err := store.GetFileDiffContent(ctx, "s1", "dup.txt")
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || content.Path != abs || string(content.After) != "two\n" {
		t.Fatalf("content = %+v", content)
	}

	paths, err := store.SessionPaths(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != abs {
		t.Fatalf("paths = %+v, want [%s]", paths, abs)
	}
}

func TestBlobDedup(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	content := []byte("duplicated content\n")

	for i, path := range []string{"/tmp/x.txt", "/tmp/y.txt"} {
		_, err := store.RecordChange(ctx, ChangeRecord{
			SessionID: "s1", Path: path, After: content, BeforeMissing: true,
		})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}

	var blobCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM blobs").Scan(&blobCount); err != nil {
		t.Fatal(err)
	}
	if blobCount != 1 {
		t.Fatalf("blob count = %d, want 1 (content-addressed dedup)", blobCount)
	}
}

func TestPerFileCapTruncates(t *testing.T) {
	store := openTestStore(t, Options{MaxFileBytes: 10})
	ctx := context.Background()

	change, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s1", Path: "/tmp/big.txt",
		After: []byte(strings.Repeat("x", 100) + "\n"), BeforeMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !change.Truncated {
		t.Fatal("expected truncated change for oversized content")
	}
	if change.AfterHash != "" {
		t.Fatal("oversized content must not be retained")
	}
	if change.Adds != 0 {
		t.Fatalf("adds = %d, want 0 for truncated change", change.Adds)
	}
	if change.AfterSize != 101 {
		t.Fatalf("after_size = %d, want 101 (metadata still recorded)", change.AfterSize)
	}
}

func TestBinaryContentTruncates(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()

	change, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s1", Path: "/tmp/bin.dat",
		After: []byte{0x00, 0x01, 0x02, 0xFF}, BeforeMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !change.Truncated || !change.IsBinary {
		t.Fatalf("expected unsupported binary to be binary+truncated, got %+v", change)
	}
}

func TestImageContentIsRetained(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		mediaType string
		prefix    string
	}{
		{name: "PNG", path: "/tmp/image.png", mediaType: "image/png", prefix: "\x89PNG\r\n\x1a\n"},
		{name: "animated GIF", path: "/tmp/image.gif", mediaType: "image/gif", prefix: "GIF89a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t, Options{})
			ctx := context.Background()
			before := []byte(tt.prefix + "old image payload")
			after := []byte(tt.prefix + "new image payload")

			change, err := store.RecordChange(ctx, ChangeRecord{
				SessionID: "s1", Path: tt.path, Before: before, After: after,
			})
			if err != nil {
				t.Fatal(err)
			}
			if change.Truncated || !change.IsBinary {
				t.Fatalf("expected retained binary image, got %+v", change)
			}
			if change.Adds != 0 || change.Dels != 0 {
				t.Fatalf("image line counts = +%d -%d, want zero", change.Adds, change.Dels)
			}

			content, err := store.GetFileDiffContent(ctx, "s1", tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if content == nil || !content.IsImage {
				t.Fatalf("image diff content = %+v", content)
			}
			if len(content.Before) != 0 || len(content.After) != 0 {
				t.Fatal("image diff metadata should not load blob bodies")
			}
			beforeSide, err := store.GetFileDiffSide(ctx, "s1", tt.path, "before")
			if err != nil {
				t.Fatal(err)
			}
			afterSide, err := store.GetFileDiffSide(ctx, "s1", tt.path, "after")
			if err != nil {
				t.Fatal(err)
			}
			if beforeSide == nil || afterSide == nil || beforeSide.MediaType != tt.mediaType || afterSide.MediaType != tt.mediaType {
				t.Fatalf("image sides = before %+v, after %+v", beforeSide, afterSide)
			}
			if !bytes.Equal(beforeSide.Data, before) || !bytes.Equal(afterSide.Data, after) {
				t.Fatal("retained image contents do not match")
			}

			changes := mustList(t, store, "s1")
			if len(changes) != 1 || changes[0].Truncated || changes[0].Adds != 0 || changes[0].Dels != 0 {
				t.Fatalf("image cumulative change = %+v", changes)
			}
		})
	}
}

func TestGetFileDiffSideLoadsOnlyRequestedImage(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	before := []byte("\x89PNG\r\n\x1a\nbefore")
	after := []byte("\x89PNG\r\n\x1a\nafter")
	change := mustRecord(t, store, ChangeRecord{
		SessionID: "s1", Path: "/tmp/image.png", Before: before, After: after,
	})

	// If serving the baseline also loads the current side, this deliberately
	// broken current blob will make the request fail.
	if _, err := store.db.Exec("UPDATE blobs SET compression = 'broken' WHERE hash = ?", change.AfterHash); err != nil {
		t.Fatal(err)
	}
	changes := mustList(t, store, "s1")
	if len(changes) != 1 || changes[0].Truncated {
		t.Fatalf("image list metadata = %+v, want retained image without loading bodies", changes)
	}
	side, err := store.GetFileDiffSide(ctx, "s1", "/tmp/image.png", "before")
	if err != nil {
		t.Fatal(err)
	}
	if side == nil || side.MediaType != "image/png" || !bytes.Equal(side.Data, before) {
		t.Fatalf("before side = %+v", side)
	}

	if _, err := store.GetFileDiffSide(ctx, "s1", "/tmp/image.png", "after"); err == nil {
		t.Fatal("expected broken requested side to fail")
	}

	created := mustRecord(t, store, ChangeRecord{
		SessionID: "s1", Path: "/tmp/created.gif", BeforeMissing: true, After: []byte("GIF89acreated"),
	})
	if created.Truncated {
		t.Fatal("created GIF should be retained")
	}
	if _, err := store.GetFileDiffSide(ctx, "s1", "/tmp/created.gif", "before"); !errors.Is(err, ErrInvalidDiffSide) {
		t.Fatalf("created image before side error = %v, want ErrInvalidDiffSide", err)
	}
}

func TestSessionBudgetExhaustion(t *testing.T) {
	store := openTestStore(t, Options{MaxSessionBytes: 30})
	ctx := context.Background()

	first, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s1", Path: "/tmp/a.txt",
		After: []byte("0123456789012345678\n"), BeforeMissing: true, // 20 bytes
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Truncated {
		t.Fatal("first change should fit the budget")
	}

	second, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s1", Path: "/tmp/b.txt",
		After: []byte("0123456789012345678\n"), BeforeMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Truncated {
		t.Fatal("second change should exceed the session budget and truncate")
	}
}

func TestCumulativeResolution(t *testing.T) {
	ctx := context.Background()

	t.Run("create then modify stays create", func(t *testing.T) {
		store := openTestStore(t, Options{})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", After: []byte("v1\n"), BeforeMissing: true})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", Before: []byte("v1\n"), After: []byte("v2\n")})

		changes := mustList(t, store, "s")
		if len(changes) != 1 || changes[0].Kind != KindCreate {
			t.Fatalf("changes = %+v, want single create", changes)
		}

		content, err := store.GetFileDiffContent(ctx, "s", "/f")
		if err != nil {
			t.Fatal(err)
		}
		if len(content.Before) != 0 || string(content.After) != "v2\n" {
			t.Fatalf("diff content = %q → %q, want empty → v2", content.Before, content.After)
		}
	})

	t.Run("create then delete is omitted", func(t *testing.T) {
		store := openTestStore(t, Options{})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", After: []byte("v1\n"), BeforeMissing: true})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", Before: []byte("v1\n"), AfterMissing: true})

		if changes := mustList(t, store, "s"); len(changes) != 0 {
			t.Fatalf("changes = %+v, want none (net no-op)", changes)
		}
	})

	t.Run("delete then recreate is modify vs baseline", func(t *testing.T) {
		store := openTestStore(t, Options{})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", Before: []byte("orig\n"), AfterMissing: true})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", After: []byte("new\n"), BeforeMissing: true})

		changes := mustList(t, store, "s")
		if len(changes) != 1 || changes[0].Kind != KindModify {
			t.Fatalf("changes = %+v, want single modify", changes)
		}

		content, err := store.GetFileDiffContent(ctx, "s", "/f")
		if err != nil {
			t.Fatal(err)
		}
		if string(content.Before) != "orig\n" || string(content.After) != "new\n" {
			t.Fatalf("diff content = %q → %q, want orig → new", content.Before, content.After)
		}
	})

	t.Run("modify back to baseline content is omitted", func(t *testing.T) {
		store := openTestStore(t, Options{})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", Before: []byte("v1\n"), After: []byte("v2\n")})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", Before: []byte("v2\n"), After: []byte("v1\n")})

		if changes := mustList(t, store, "s"); len(changes) != 0 {
			t.Fatalf("changes = %+v, want none (returned to baseline)", changes)
		}
	})

	t.Run("modify then delete is delete with baseline content", func(t *testing.T) {
		store := openTestStore(t, Options{})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", Before: []byte("a\nb\n"), After: []byte("a\nc\n")})
		mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/f", Before: []byte("a\nc\n"), AfterMissing: true})

		changes := mustList(t, store, "s")
		if len(changes) != 1 || changes[0].Kind != KindDelete {
			t.Fatalf("changes = %+v, want single delete", changes)
		}
		if changes[0].Dels != 2 {
			t.Fatalf("dels = %d, want 2 (baseline content)", changes[0].Dels)
		}
	})
	t.Run("image cumulative spans", func(t *testing.T) {
		png := func(label string) []byte { return []byte("\x89PNG\r\n\x1a\n" + label) }

		t.Run("multiple modifies keep baseline and latest", func(t *testing.T) {
			store := openTestStore(t, Options{})
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/image.png", Before: png("a"), After: png("b")})
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/image.png", Before: png("b"), After: png("c")})

			changes := mustList(t, store, "s")
			if len(changes) != 1 || changes[0].Kind != KindModify || changes[0].Truncated {
				t.Fatalf("changes = %+v", changes)
			}
			before, err := store.GetFileDiffSide(ctx, "s", "/image.png", "before")
			if err != nil {
				t.Fatal(err)
			}
			after, err := store.GetFileDiffSide(ctx, "s", "/image.png", "after")
			if err != nil {
				t.Fatal(err)
			}
			if string(before.Data) != string(png("a")) || string(after.Data) != string(png("c")) {
				t.Fatalf("image sides = %q → %q", before.Data, after.Data)
			}
		})

		t.Run("create then modify stays create", func(t *testing.T) {
			store := openTestStore(t, Options{})
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/image.png", After: png("a"), BeforeMissing: true})
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/image.png", Before: png("a"), After: png("b")})

			changes := mustList(t, store, "s")
			if len(changes) != 1 || changes[0].Kind != KindCreate || changes[0].Truncated {
				t.Fatalf("changes = %+v", changes)
			}
			if _, err := store.GetFileDiffSide(ctx, "s", "/image.png", "before"); !errors.Is(err, ErrInvalidDiffSide) {
				t.Fatalf("before error = %v, want ErrInvalidDiffSide", err)
			}
			after, err := store.GetFileDiffSide(ctx, "s", "/image.png", "after")
			if err != nil || after == nil || string(after.Data) != string(png("b")) {
				t.Fatalf("after = %+v, err = %v", after, err)
			}
		})

		t.Run("return to baseline is omitted", func(t *testing.T) {
			store := openTestStore(t, Options{})
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/image.png", Before: png("a"), After: png("b")})
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/image.png", Before: png("b"), After: png("a")})
			if changes := mustList(t, store, "s"); len(changes) != 0 {
				t.Fatalf("changes = %+v, want none", changes)
			}
		})

		t.Run("mixed image and text span is truncated", func(t *testing.T) {
			store := openTestStore(t, Options{})
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/mixed", Before: png("a"), After: png("b")})
			// Simulate an incomplete tracking sequence where an intervening
			// image-to-text transition was not captured.
			mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/mixed", Before: []byte("old\n"), After: []byte("new\n")})

			changes := mustList(t, store, "s")
			if len(changes) != 1 || !changes[0].Truncated {
				t.Fatalf("changes = %+v, want truncated mixed span", changes)
			}
			content, err := store.GetFileDiffContent(ctx, "s", "/mixed")
			if err != nil {
				t.Fatal(err)
			}
			if content == nil || !content.Truncated || content.IsImage || len(content.Before) != 0 || len(content.After) != 0 {
				t.Fatalf("mixed content = %+v", content)
			}
		})
	})
}

func TestSeqSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file_history.db")
	ctx := context.Background()

	store, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s", Path: "/f", After: []byte("v1\n"), BeforeMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	second, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s", Path: "/f", Before: []byte("v1\n"), After: []byte("v2\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq != first.Seq+1 {
		t.Fatalf("seq after reopen = %d, want %d", second.Seq, first.Seq+1)
	}
}

func TestSessionPaths(t *testing.T) {
	store := openTestStore(t, Options{})
	mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/a", After: []byte("1\n"), BeforeMissing: true})
	mustRecord(t, store, ChangeRecord{SessionID: "s", Path: "/b", After: []byte("2\n"), BeforeMissing: true})
	mustRecord(t, store, ChangeRecord{SessionID: "other", Path: "/c", After: []byte("3\n"), BeforeMissing: true})

	paths, err := store.SessionPaths(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want 2 entries", paths)
	}
}

func TestBlobRoundTrip(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	content := []byte(strings.Repeat("compressible line of text\n", 100))

	change, err := store.RecordChange(ctx, ChangeRecord{
		SessionID: "s", Path: "/f", After: content, BeforeMissing: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.getBlob(ctx, change.AfterHash)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("blob round trip mismatch")
	}

	var stored int
	var compression string
	if err := store.db.QueryRow("SELECT LENGTH(data), compression FROM blobs WHERE hash = ?", change.AfterHash).
		Scan(&stored, &compression); err != nil {
		t.Fatal(err)
	}
	if compression != "gzip" || stored >= len(content) {
		t.Fatalf("expected gzip-compressed blob smaller than %d, got %d (%s)", len(content), stored, compression)
	}
}

func TestGCSweepsStaleSessionsAndBlobs(t *testing.T) {
	store := openTestStore(t, Options{})
	ctx := context.Background()
	mustRecord(t, store, ChangeRecord{SessionID: "gone", Path: "/f", After: []byte("v\n"), BeforeMissing: true})

	// Empty sessions DB path: only the age sweep and blob sweep run.
	if err := store.GC(ctx, "", 0); err != nil {
		t.Fatal(err)
	}
	// Rows survive without an age limit or sessions DB.
	if paths, _ := store.SessionPaths(ctx, "gone"); len(paths) != 1 {
		t.Fatal("rows should survive GC without constraints")
	}

	// Manually delete the change rows, then verify the blob sweep collects orphans.
	if _, err := store.db.Exec("DELETE FROM file_changes"); err != nil {
		t.Fatal(err)
	}
	if err := store.GC(ctx, "", 0); err != nil {
		t.Fatal(err)
	}
	var blobCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM blobs").Scan(&blobCount); err != nil {
		t.Fatal(err)
	}
	if blobCount != 0 {
		t.Fatalf("blob count after GC = %d, want 0", blobCount)
	}
}

func TestGCAgainstSessionsDB(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Minimal stand-in for sessions.db: just the sessions(id) table the GC reads.
	sessionsPath := filepath.Join(dir, "sessions.db")
	sessDB, err := sql.Open("sqlite", sessionsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessDB.Exec("CREATE TABLE sessions (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := sessDB.Exec("INSERT INTO sessions (id) VALUES ('live')"); err != nil {
		t.Fatal(err)
	}
	sessDB.Close()

	store, err := Open(filepath.Join(dir, "file_history.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	mustRecord(t, store, ChangeRecord{SessionID: "live", Path: "/keep", After: []byte("k\n"), BeforeMissing: true})
	mustRecord(t, store, ChangeRecord{SessionID: "deleted", Path: "/drop", After: []byte("d\n"), BeforeMissing: true})

	if err := store.GC(ctx, sessionsPath, 0); err != nil {
		t.Fatal(err)
	}

	if paths, _ := store.SessionPaths(ctx, "live"); len(paths) != 1 {
		t.Fatal("live session rows must survive GC")
	}
	if paths, _ := store.SessionPaths(ctx, "deleted"); len(paths) != 0 {
		t.Fatal("rows for sessions missing from sessions.db must be swept")
	}
}

func TestGCEnforcesTotalBudget(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "file_history.db"), Options{MaxTotalBytes: 256 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	// Poorly compressible text (pseudo-random printable ASCII) so the DB
	// demonstrably exceeds the cap without tripping the binary sniff.
	noisy := func(seed byte) []byte {
		content := make([]byte, 128*1024)
		x := uint32(seed) + 1
		for i := range content {
			x = x*1664525 + 1013904223
			content[i] = byte(32 + (x>>16)%95)
		}
		return content
	}
	// CURRENT_TIMESTAMP has second resolution, so these records often tie on
	// created_at; the session_id tiebreak makes "a-oldest" prune first.
	for i, sessionID := range []string{"a-oldest", "b-middle", "c-newest"} {
		mustRecord(t, store, ChangeRecord{
			SessionID: sessionID, Path: "/work/f.txt",
			After: noisy(byte(i)), BeforeMissing: true,
		})
	}

	if err := store.GC(ctx, "", 0); err != nil {
		t.Fatal(err)
	}

	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	size, err := databaseSize(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if size > 256*1024 {
		t.Fatalf("db size after GC = %d, want <= %d", size, 256*1024)
	}

	// The most recently changed session survives; the oldest is pruned first.
	if paths, _ := store.SessionPaths(ctx, "c-newest"); len(paths) != 1 {
		t.Fatal("newest session history must survive budget pruning")
	}
	if paths, _ := store.SessionPaths(ctx, "a-oldest"); len(paths) != 0 {
		t.Fatal("oldest session history must be pruned first")
	}
}

func TestRecordChangeEnforcesTotalBudget(t *testing.T) {
	dir := t.TempDir()
	const maxTotalBytes = 256 * 1024
	store, err := Open(filepath.Join(dir, "file_history.db"), Options{MaxTotalBytes: maxTotalBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	// Poorly compressible text makes each ordinary record materially grow the
	// database without tripping the binary-content check.
	noisy := func(seed byte) []byte {
		content := make([]byte, 128*1024)
		x := uint32(seed) + 1
		for i := range content {
			x = x*1664525 + 1013904223
			content[i] = byte(32 + (x>>16)%95)
		}
		return content
	}
	// Use reverse-lexical IDs to prove insertion order, not session ID, decides
	// recency when SQLite timestamps tie within the same second.
	for i, sessionID := range []string{"z-oldest", "m-middle", "a-newest"} {
		mustRecord(t, store, ChangeRecord{
			SessionID: sessionID, Path: "/work/f.txt",
			After: noisy(byte(i)), BeforeMissing: true,
		})
	}

	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	size, err := databaseSize(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	if size > maxTotalBytes {
		t.Fatalf("db size after ordinary records = %d, want <= %d", size, maxTotalBytes)
	}
	if paths, _ := store.SessionPaths(ctx, "a-newest"); len(paths) != 1 {
		t.Fatal("newest session history must survive live budget pruning")
	}
	if paths, _ := store.SessionPaths(ctx, "m-middle"); len(paths) != 1 {
		t.Fatal("live budget enforcement pruned more sessions than required")
	}
	if paths, _ := store.SessionPaths(ctx, "z-oldest"); len(paths) != 0 {
		t.Fatal("oldest session history must be pruned by live budget enforcement")
	}
}

func TestRecordChangeRejectsChangePrunedByTotalBudget(t *testing.T) {
	store := openTestStore(t, Options{MaxTotalBytes: 1})
	change, err := store.RecordChange(context.Background(), ChangeRecord{
		SessionID: "current", Path: "/work/f.txt", After: []byte("content"), BeforeMissing: true,
	})
	if err == nil || change != nil {
		t.Fatalf("RecordChange() = (%+v, %v), want nil change and budget error", change, err)
	}
	if store.uncheckedTotalBytes != 0 || store.uncheckedTotalRecordCount != 0 {
		t.Fatalf("failed check counters = (%d bytes, %d records), want reset", store.uncheckedTotalBytes, store.uncheckedTotalRecordCount)
	}
	if paths, queryErr := store.SessionPaths(context.Background(), "current"); queryErr != nil || len(paths) != 0 {
		t.Fatalf("rolled-back session paths = %v, %v; want none", paths, queryErr)
	}
}

func TestRecorderSwallowsAndConverts(t *testing.T) {
	store := openTestStore(t, Options{})
	rec := NewRecorder(store)
	ctx := context.Background()

	fc := rec.RecordChange(ctx, ChangeRecord{
		SessionID: "s", Path: "/f", After: []byte("a\nb\n"), BeforeMissing: true,
	})
	if fc == nil {
		t.Fatal("expected file change")
	}
	if fc.Kind != KindCreate || fc.Adds != 2 || fc.Seq != 1 {
		t.Fatalf("file change = %+v", fc)
	}

	// No-op returns nil without error.
	if fc := rec.RecordChange(ctx, ChangeRecord{Path: "/f"}); fc != nil {
		t.Fatalf("expected nil for empty session, got %+v", fc)
	}
}

func mustRecord(t *testing.T, store *Store, rec ChangeRecord) *Change {
	t.Helper()
	change, err := store.RecordChange(context.Background(), rec)
	if err != nil {
		t.Fatalf("RecordChange(%s): %v", rec.Path, err)
	}
	return change
}

func mustList(t *testing.T, store *Store, sessionID string) []CumulativeChange {
	t.Helper()
	changes, err := store.ListSessionChanges(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListSessionChanges: %v", err)
	}
	return changes
}

func TestRecordChangeSessionLockDoesNotGateOtherSessions(t *testing.T) {
	store := openTestStore(t, Options{})
	unlockA := store.lockRecordSession("session-a")
	defer unlockA()

	done := make(chan error, 1)
	go func() {
		_, err := store.RecordChange(context.Background(), ChangeRecord{
			SessionID:     "session-b",
			Path:          "/tmp/session-b.txt",
			BeforeMissing: true,
			After:         []byte("b"),
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("session B file tracking waited for session A's session lock")
	}
}

func TestRecordChangeConcurrentAcrossSessions(t *testing.T) {
	store := openTestStore(t, Options{})
	const sessions = 8
	const changesPerSession = 8
	start := make(chan struct{})
	errCh := make(chan error, sessions*changesPerSession)
	var wg sync.WaitGroup
	for sessionIndex := 0; sessionIndex < sessions; sessionIndex++ {
		for changeIndex := 0; changeIndex < changesPerSession; changeIndex++ {
			wg.Add(1)
			go func(sessionIndex, changeIndex int) {
				defer wg.Done()
				<-start
				_, err := store.RecordChange(context.Background(), ChangeRecord{
					SessionID:     fmt.Sprintf("session-%d", sessionIndex),
					Path:          fmt.Sprintf("/tmp/session-%d-%d.txt", sessionIndex, changeIndex),
					BeforeMissing: true,
					After:         []byte("content"),
				})
				errCh <- err
			}(sessionIndex, changeIndex)
		}
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	for sessionIndex := 0; sessionIndex < sessions; sessionIndex++ {
		changes := mustList(t, store, fmt.Sprintf("session-%d", sessionIndex))
		if len(changes) != changesPerSession {
			t.Fatalf("session %d changes = %d, want %d", sessionIndex, len(changes), changesPerSession)
		}
	}
}
