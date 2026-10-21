//go:build unix

package grepsearch

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"
)

func scanNotRegular(t *testing.T, path string) {
	t.Helper()
	// Even a regressed blocking os.Open is released during failure cleanup.
	defer releaseFIFOReader(path)
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- Scan(context.Background(), path, regexp.MustCompile("x"), 1, func([]string, int) error { calls++; return nil })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) || calls != 0 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("scan blocked")
	}
}

func TestScanIntentRejectsFIFOWithoutWriter(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skip(err)
	}
	scanNotRegular(t, fifo)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	scanNotRegular(t, link)
}

func TestScanIntentRejectsDirAndSocket(t *testing.T) {
	dir := t.TempDir()
	scanNotRegular(t, dir)
	sock := filepath.Join(os.TempDir(), "gs"+filepath.Base(dir)+".sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	defer os.Remove(sock)
	scanNotRegular(t, sock)
}

func TestScanIntentFollowsSymlinkToRegular(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "t.txt")
	if err := os.WriteFile(target, []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := Scan(context.Background(), link, regexp.MustCompile("x"), 1, func([]string, int) error { calls++; return nil }); err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestOpenRegularFileRejectsFIFOAfterPrecheckRace(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := os.WriteFile(fifo, []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Stat(fifo)
	if err != nil || !expected.Mode().IsRegular() {
		t.Fatalf("precheck: %v %v", expected, err)
	}
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	defer releaseFIFOReader(fifo)
	type result struct {
		file *os.File
		err  error
	}
	done := make(chan result, 1)
	go func() {
		f, err := openRegularFile(fifo)
		done <- result{f, err}
	}()
	select {
	case r := <-done:
		if !errors.Is(r.err, ErrNotRegular) || r.file != nil {
			t.Fatalf("file=%v err=%v", r.file, r.err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("open blocked without writer")
	}
	// No reader may remain after rejection: an O_NONBLOCK writer succeeds
	// exactly when a reader fd is still open on this FIFO.
	fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err == nil {
		_ = syscall.Close(fd)
		t.Fatal("rejected FIFO leaked an open reader descriptor")
	}
	if !errors.Is(err, syscall.ENXIO) {
		t.Fatalf("writer after rejection: %v", err)
	}
}

func TestScanPlainNilContextFIFOStillBlocksUntilWriter(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Skip(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Scan(nil, fifo, regexp.MustCompile("x"), 1, func([]string, int) error { return nil })
	}()
	select {
	case err := <-done:
		t.Fatalf("plain scan returned without writer: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	// Release the blocked os.Open by opening a writer, then close it (EOF).
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("plain scan did not finish after writer")
	}
}

func releaseFIFOReader(path string) {
	if fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
		_ = syscall.Close(fd)
	}
}

func TestScanIntentRejectsDevice(t *testing.T) {
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip(err)
	}
	scanNotRegular(t, "/dev/null")
}
