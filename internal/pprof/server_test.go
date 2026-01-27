package pprof

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerStartStop(t *testing.T) {
	srv := NewServer()

	// Start on random port
	port, err := srv.Start(0)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	if port == 0 {
		t.Fatal("Start() returned port 0")
	}

	// Verify we can reach the pprof endpoint
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/", port))
	if err != nil {
		t.Fatalf("GET /debug/pprof/ error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /debug/pprof/ status = %d, want 200", resp.StatusCode)
	}

	// Stop the server
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		t.Errorf("Stop() error: %v", err)
	}
}

func TestServerPort(t *testing.T) {
	srv := NewServer()

	port, err := srv.Start(0)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer srv.Stop(context.Background())

	if got := srv.Port(); got != port {
		t.Errorf("Port() = %d, want %d", got, port)
	}
}

func TestServerSpecificPort(t *testing.T) {
	srv := NewServer()

	// Use a high port that's likely to be available
	wantPort := 49152 + (os.Getpid() % 1000)

	port, err := srv.Start(wantPort)
	if err != nil {
		// Port might be in use, skip test
		t.Skipf("could not bind to port %d: %v", wantPort, err)
	}
	defer srv.Stop(context.Background())

	if port != wantPort {
		t.Errorf("Start(%d) returned port %d", wantPort, port)
	}
}

func TestPrintUsage(t *testing.T) {
	var buf bytes.Buffer
	PrintUsage(&buf, 12345)

	output := buf.String()
	if output == "" {
		t.Error("PrintUsage() produced no output")
	}

	// Check that the port appears in the output
	if !bytes.Contains(buf.Bytes(), []byte("12345")) {
		t.Errorf("PrintUsage() output missing port 12345:\n%s", output)
	}

	// Check for expected commands
	expected := []string{
		"http://127.0.0.1:12345",
		"term-llm pprof cpu",
		"term-llm pprof heap",
		"term-llm pprof goroutine",
	}
	for _, want := range expected {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("PrintUsage() output missing %q", want)
		}
	}
}

func TestPortFileWriteRead(t *testing.T) {
	// Set up a temporary cache directory
	tmpDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmpDir)

	srv := NewServer()
	port, err := srv.Start(0)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// Check the port file was written
	readPort, err := ReadPortFile()
	if err != nil {
		t.Fatalf("ReadPortFile() error: %v", err)
	}
	if readPort != port {
		t.Errorf("ReadPortFile() = %d, want %d", readPort, port)
	}

	// Check IsServerRunning
	runningPort, running := IsServerRunning()
	if !running {
		t.Error("IsServerRunning() returned false")
	}
	if runningPort != port {
		t.Errorf("IsServerRunning() port = %d, want %d", runningPort, port)
	}

	// Stop the server
	srv.Stop(context.Background())

	// Port file should be removed
	_, err = ReadPortFile()
	if err == nil {
		t.Error("ReadPortFile() should error after Stop()")
	}
}

func TestReadPortFileNotExists(t *testing.T) {
	// Set up a temporary cache directory with no port file
	tmpDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmpDir)

	_, err := ReadPortFile()
	if err == nil {
		t.Error("ReadPortFile() should error when file doesn't exist")
	}
}

func TestIsServerRunningNoServer(t *testing.T) {
	// Set up a temporary cache directory
	tmpDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", tmpDir)

	// Write a port file for a port that's not listening
	portPath := filepath.Join(tmpDir, "term-llm", "pprof.port")
	os.MkdirAll(filepath.Dir(portPath), 0755)
	os.WriteFile(portPath, []byte("65432"), 0600)

	port, running := IsServerRunning()
	if running {
		t.Error("IsServerRunning() should return false when server not running")
	}
	if port != 0 {
		t.Errorf("IsServerRunning() port = %d, want 0", port)
	}
}

func TestGetCacheDir(t *testing.T) {
	// Test with XDG_CACHE_HOME set
	t.Setenv("XDG_CACHE_HOME", "/custom/cache")
	dir, err := GetCacheDir()
	if err != nil {
		t.Fatalf("GetCacheDir() error: %v", err)
	}
	if dir != "/custom/cache/term-llm" {
		t.Errorf("GetCacheDir() = %q, want %q", dir, "/custom/cache/term-llm")
	}

	// Test without XDG_CACHE_HOME
	os.Unsetenv("XDG_CACHE_HOME")
	dir, err = GetCacheDir()
	if err != nil {
		t.Fatalf("GetCacheDir() error: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".cache", "term-llm")
	if dir != want {
		t.Errorf("GetCacheDir() = %q, want %q", dir, want)
	}
}

func TestPprofEndpoints(t *testing.T) {
	srv := NewServer()
	port, err := srv.Start(0)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer srv.Stop(context.Background())

	endpoints := []string{
		"/debug/pprof/",
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
		"/debug/pprof/allocs",
		"/debug/pprof/block",
		"/debug/pprof/mutex",
		"/debug/pprof/threadcreate",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			url := fmt.Sprintf("http://127.0.0.1:%d%s", port, ep)
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET %s error: %v", ep, err)
			}
			defer resp.Body.Close()

			// Read body to ensure endpoint works
			_, err = io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading response error: %v", err)
			}

			if resp.StatusCode != http.StatusOK {
				t.Errorf("GET %s status = %d, want 200", ep, resp.StatusCode)
			}
		})
	}
}
