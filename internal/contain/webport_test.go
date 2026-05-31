package contain

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// envWebPortValue extracts the integer WEB_PORT value from rendered .env text.
func envWebPortValue(t *testing.T, envText string) int {
	t.Helper()
	for _, line := range strings.Split(envText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "WEB_PORT=") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "WEB_PORT="))
		port, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("WEB_PORT value %q is not numeric: %v", value, err)
		}
		return port
	}
	t.Fatalf("no WEB_PORT line in env:\n%s", envText)
	return 0
}

func TestNextWebPortSkipsUsedAndUnavailable(t *testing.T) {
	cases := []struct {
		name      string
		used      map[int]bool
		available func(int) bool
		want      string
	}{
		{
			name:      "base free",
			used:      map[int]bool{},
			available: func(int) bool { return true },
			want:      "8081",
		},
		{
			name:      "skips workspace-claimed port",
			used:      map[int]bool{8081: true},
			available: func(int) bool { return true },
			want:      "8082",
		},
		{
			name:      "skips port in use on host",
			used:      map[int]bool{},
			available: func(p int) bool { return p != 8081 },
			want:      "8082",
		},
		{
			name:      "skips multiple consecutive collisions",
			used:      map[int]bool{8081: true, 8083: true},
			available: func(p int) bool { return p != 8082 },
			want:      "8084",
		},
		{
			name:      "falls back to base when nothing free",
			used:      map[int]bool{},
			available: func(int) bool { return false },
			want:      "8081",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextWebPort(webPortBase, tc.used, tc.available); got != tc.want {
				t.Fatalf("nextWebPort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUsedWorkspaceWebPortsScansEnvFiles(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root, err := ContainersRoot()
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceEnv(t, root, "alpha", "WEB_PORT=8081\nWEB_TOKEN=abc\n")
	writeWorkspaceEnv(t, root, "beta", "WEB_TOKEN=def\nWEB_PORT=8090\n")
	writeWorkspaceEnv(t, root, "gamma", "WEB_TOKEN=ghi\n") // no port

	used, err := usedWorkspaceWebPorts()
	if err != nil {
		t.Fatal(err)
	}
	if !used[8081] || !used[8090] {
		t.Fatalf("used = %v, want 8081 and 8090 present", used)
	}
	if len(used) != 2 {
		t.Fatalf("used = %v, want exactly 2 entries", used)
	}
}

func writeWorkspaceEnv(t *testing.T, root, name, contents string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
