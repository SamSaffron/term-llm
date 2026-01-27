package pprof

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
)

// Server wraps the net/http/pprof server for runtime profiling.
type Server struct {
	server   *http.Server
	listener net.Listener
	port     int
}

// NewServer creates a new pprof Server.
func NewServer() *Server {
	return &Server{}
}

// Start binds the pprof server to localhost on the given port.
// Use port 0 for a random available port.
// Returns the actual port the server is listening on.
func (s *Server) Start(port int) (int, error) {
	// Bind to localhost only for security
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("bind to %s: %w", addr, err)
	}

	s.listener = listener
	s.port = listener.Addr().(*net.TCPAddr).Port

	// Use a dedicated mux with explicit pprof handlers to avoid exposing
	// any other handlers that may be registered on http.DefaultServeMux
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	s.server = &http.Server{
		Handler: mux,
	}

	// Start serving in a goroutine
	go func() {
		// Ignore ErrServerClosed which is expected on shutdown
		if err := s.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "pprof server error: %v\n", err)
		}
	}()

	// Write port to registry file for auto-discovery
	if err := writePortFile(s.port); err != nil {
		// Non-fatal - just log it
		fmt.Fprintf(os.Stderr, "warning: could not write pprof port file: %v\n", err)
	}

	return s.port, nil
}

// Port returns the port the server is listening on.
func (s *Server) Port() int {
	return s.port
}

// Stop gracefully shuts down the pprof server.
func (s *Server) Stop(ctx context.Context) error {
	// Remove the port registry file
	removePortFile()

	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

// PrintUsage prints helpful pprof commands to the given writer.
func PrintUsage(w io.Writer, port int) {
	fmt.Fprintf(w, "\npprof server: http://127.0.0.1:%d\n\n", port)
	fmt.Fprintf(w, "Quick commands (from another terminal):\n")
	fmt.Fprintf(w, "  term-llm pprof cpu       # 30 second CPU profile\n")
	fmt.Fprintf(w, "  term-llm pprof heap      # memory allocation profile\n")
	fmt.Fprintf(w, "  term-llm pprof goroutine # goroutine stack dump\n")
	fmt.Fprintf(w, "  term-llm pprof web       # open in browser\n\n")
}

// GetCacheDir returns the XDG cache directory for term-llm.
// Uses $XDG_CACHE_HOME if set, otherwise ~/.cache
func GetCacheDir() (string, error) {
	if xdgCache := os.Getenv("XDG_CACHE_HOME"); xdgCache != "" {
		return filepath.Join(xdgCache, "term-llm"), nil
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".cache", "term-llm"), nil
}

// getPortFilePath returns the path to the pprof port registry file.
func getPortFilePath() (string, error) {
	cacheDir, err := GetCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, "pprof.port"), nil
}

// writePortFile writes the port to the registry file for auto-discovery.
func writePortFile(port int) error {
	portPath, err := getPortFilePath()
	if err != nil {
		return err
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(filepath.Dir(portPath), 0755); err != nil {
		return err
	}

	return os.WriteFile(portPath, []byte(strconv.Itoa(port)), 0600)
}

// removePortFile removes the port registry file.
func removePortFile() {
	portPath, err := getPortFilePath()
	if err != nil {
		return
	}
	os.Remove(portPath)
}

// ReadPortFile reads the port from the registry file.
// Returns the port, or an error if the file doesn't exist or is invalid.
func ReadPortFile() (int, error) {
	portPath, err := getPortFilePath()
	if err != nil {
		return 0, err
	}

	data, err := os.ReadFile(portPath)
	if err != nil {
		return 0, fmt.Errorf("no pprof server running (port file not found)")
	}

	port, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("invalid port file: %w", err)
	}

	return port, nil
}

// IsServerRunning checks if a pprof server appears to be running.
// It checks if the port file exists and the port is reachable.
func IsServerRunning() (int, bool) {
	port, err := ReadPortFile()
	if err != nil {
		return 0, false
	}

	// Try to connect to verify it's actually running
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return 0, false
	}
	conn.Close()
	return port, true
}
