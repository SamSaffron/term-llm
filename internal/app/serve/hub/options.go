package hub

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	appserver "github.com/samsaffron/term-llm/internal/app/serve/server"
	"github.com/samsaffron/term-llm/internal/appdata"
)

// RegistrationTokenEnv is the private reverse-node registration handoff key.
const RegistrationTokenEnv = "TERM_LLM_HUB_REGISTRATION_TOKEN"

// Options captures the Hub invocation's flag values.
type Options struct {
	Host                  string
	Port                  int
	Config                string
	Contain               bool
	NodesFile             string
	AuthMode              string
	Token                 string
	RegistrationToken     string
	BasePath              string
	PublicURL             string
	PasskeyAuthFile       string
	BootstrapTokenFile    string
	RecoveryTokenFile     string
	PrintBootstrapToken   bool
	PasskeyTrustedProxies []string
}

// ValidateBind rejects unauthenticated public binds. A Hub with bearer auth
// may bind publicly for use behind a reverse proxy, but --auth none stays
// loopback-only because the Hub injects node tokens server-side.
func ValidateBind(host string, port int, requireAuth bool) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("invalid --port %d (must be 1-65535)", port)
	}
	if !requireAuth && !appserver.IsLoopbackHost(host) {
		return fmt.Errorf("--auth none is only allowed on loopback hosts (got %q)", host)
	}
	return nil
}

func NormalizeBasePath(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" || p == "/" {
		return "", nil
	}
	if strings.Contains(p, "://") || strings.ContainsAny(p, "?#") {
		return "", fmt.Errorf("--base-path must be a URL path such as /hub, not %q", raw)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if appserver.HasDotDotSegment(p) {
		return "", fmt.Errorf("--base-path must not contain .. segments")
	}
	p = path.Clean(p)
	if p == "/" || p == "." {
		return "", nil
	}
	return p, nil
}

// DefaultNodesFile is where dashboard-added nodes persist when
// --nodes-file is not given.
func DefaultNodesFile() (string, error) {
	dir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub", "nodes.json"), nil
}

func DefaultAuthFile() (string, error) {
	dir, err := appdata.GetDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "hub", "auth.json"), nil
}

func ResolveAuthMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		mode = "bearer"
	}
	if mode != "bearer" && mode != "none" && mode != "passkey" {
		return "", fmt.Errorf("invalid --auth %q (must be bearer, passkey, or none)", raw)
	}
	return mode, nil
}
