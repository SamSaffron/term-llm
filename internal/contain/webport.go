package contain

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// webPortBase is the first port term-llm tries to assign to a workspace Web UI.
const webPortBase = 8081

// webPortScanLimit caps how many candidate ports we probe before giving up and
// falling back to the base port.
const webPortScanLimit = 200

// defaultWebPort returns the lowest free Web UI port at or above webPortBase,
// skipping ports already claimed by existing workspaces and ports that cannot
// currently be bound on the host. It is best-effort: on any failure it returns
// the base port so creation can still proceed.
func defaultWebPort() string {
	used, err := usedWorkspaceWebPorts()
	if err != nil {
		used = map[int]bool{}
	}
	return nextWebPort(webPortBase, used, hostPortAvailable)
}

// nextWebPort walks upward from base and returns the first port that is neither
// claimed by an existing workspace nor reported unavailable on the host. If no
// free port is found within webPortScanLimit candidates it returns base.
func nextWebPort(base int, used map[int]bool, available func(int) bool) string {
	for port := base; port < base+webPortScanLimit; port++ {
		if used[port] {
			continue
		}
		if available != nil && !available(port) {
			continue
		}
		return strconv.Itoa(port)
	}
	return strconv.Itoa(base)
}

// usedWorkspaceWebPorts scans existing workspace .env files for WEB_PORT values
// so newly created workspaces avoid colliding with ones already configured
// (even when those workspaces are not currently running).
func usedWorkspaceWebPorts() (map[int]bool, error) {
	root, err := ContainersRoot()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", ".env"))
	if err != nil {
		return nil, err
	}
	used := map[int]bool{}
	for _, envPath := range matches {
		if port, ok := readEnvWebPort(envPath); ok {
			used[port] = true
		}
	}
	return used, nil
}

// readEnvWebPort extracts the WEB_PORT value from a workspace .env file.
func readEnvWebPort(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "WEB_PORT=") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "WEB_PORT="))
		port, err := strconv.Atoi(value)
		if err != nil {
			return 0, false
		}
		return port, true
	}
	return 0, false
}

// hostPortAvailable reports whether the given TCP port can currently be bound on
// loopback and all interfaces, mirroring how Docker publishes the Web UI port.
func hostPortAvailable(port int) bool {
	for _, host := range []string{"127.0.0.1", "0.0.0.0"} {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
		if err != nil {
			return false
		}
		_ = ln.Close()
	}
	return true
}
