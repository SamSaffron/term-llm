package hostfacts

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/samsaffron/term-llm/internal/config"
)

const maxNotesBytes = 64 << 10

var unsafeHostKey = regexp.MustCompile(`[^a-z0-9_-]+`)

func HostKey(hostname string) string {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if i := strings.IndexByte(hostname, '.'); i >= 0 {
		hostname = hostname[:i]
	}
	hostname = unsafeHostKey.ReplaceAllString(hostname, "-")
	hostname = strings.Trim(hostname, "-")
	if hostname == "" {
		return "unknown-host"
	}
	return hostname
}

func Notes() string {
	hostname, _ := os.Hostname()
	configDir, err := config.GetConfigDir()
	if err != nil {
		return "Host notes unavailable: " + err.Error()
	}
	path := filepath.Join(configDir, "hosts", HostKey(hostname)+".md")
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("No host notes found. User-maintained notes can be added at %s.", path)
	}
	defer f.Close()
	buf := make([]byte, maxNotesBytes+1)
	n, _ := f.Read(buf)
	body := string(buf[:min(n, maxNotesBytes)])
	if n > maxNotesBytes {
		body += "\n\n[host notes truncated]"
	}
	return "# Host notes (user-maintained)\n\n" + body
}
