package contain

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// defaultWebBasePath is the fallback URL prefix when a workspace .env has no
// WEB_BASE_PATH. The fallback web port reuses webPortBase (see webport.go) so
// there is a single source for the base port; note that the dynamic free-port
// picker defaultWebPort() is for *creating* workspaces, not for reading an
// existing one's config.
const defaultWebBasePath = "/chat"

// EnvPath returns the .env path for a named contain workspace. The file is
// written with 0600 permissions and holds the workspace's web UI settings
// (WEB_PORT, WEB_TOKEN, WEB_BASE_PATH) among other secrets.
func EnvPath(name string) (string, error) {
	dir, err := ContainerDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".env"), nil
}

// ReadEnvFile parses a KEY=VALUE .env file into a map. Blank lines and lines
// beginning with '#' are ignored, surrounding whitespace is trimmed, and one
// layer of surrounding single or double quotes is stripped from values. Lines
// without an '=' are skipped.
func ReadEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key != "" {
			values[key] = value
		}
	}
	return values, nil
}

// WebConfig captures the web UI connection settings for a contain workspace,
// resolved from its .env file. It is the discovery surface tooling (such as
// the hub) needs to reach a workspace's serve without parsing the 0600 .env
// ad hoc.
type WebConfig struct {
	// Port is the host port the workspace's web UI is published on.
	Port string
	// Token is the bearer token guarding the web UI. Empty when not yet
	// provisioned (e.g. a freshly templated workspace).
	Token string
	// BasePath is the URL prefix the web UI is mounted under (always rooted
	// with a leading slash, no trailing slash).
	BasePath string
}

// ReadWebConfig reads the workspace .env and returns the resolved web UI
// settings. Port and BasePath fall back to their template defaults when unset
// or still holding an unrendered placeholder; Token has no default and is
// returned empty when not yet provisioned. The error reports a missing or
// unreadable workspace .env.
func ReadWebConfig(name string) (WebConfig, error) {
	if err := ValidateName(name); err != nil {
		return WebConfig{}, err
	}
	envPath, err := EnvPath(name)
	if err != nil {
		return WebConfig{}, err
	}
	values, err := ReadEnvFile(envPath)
	if err != nil {
		return WebConfig{}, err
	}
	cfg := WebConfig{
		Port:     resolveEnvValue(values["WEB_PORT"], strconv.Itoa(webPortBase)),
		Token:    cleanEnvValue(values["WEB_TOKEN"]),
		BasePath: resolveEnvValue(values["WEB_BASE_PATH"], defaultWebBasePath),
	}
	if err := validateWebPort(cfg.Port); err != nil {
		return WebConfig{}, fmt.Errorf("workspace %q: %w", name, err)
	}
	if !strings.HasPrefix(cfg.BasePath, "/") {
		cfg.BasePath = "/" + cfg.BasePath
	}
	// Mirror the serve's normalizeBasePath: drop trailing slashes so a workspace
	// with WEB_BASE_PATH="/chat/" yields the same canonical "/chat" the serve
	// bakes into its HTML. Guard against collapsing "/" to "".
	if trimmed := strings.TrimRight(cfg.BasePath, "/"); trimmed != "" {
		cfg.BasePath = trimmed
	}
	return cfg, nil
}

// validateWebPort rejects a WEB_PORT that is not a plain decimal TCP port in
// the 1-65535 range. The default port is itself valid, so this also guards the
// fallback. strconv.Atoi rejects signs-with-junk and trailing characters, so
// "80x"/"-1" fail here rather than being silently truncated.
func validateWebPort(port string) error {
	n, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("invalid WEB_PORT %q: must be a number 1-65535", port)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("invalid WEB_PORT %q: must be 1-65535", port)
	}
	return nil
}

// cleanEnvValue trims a raw .env value and drops unrendered template
// placeholders ({{...}}) left by a workspace template that was never rendered.
func cleanEnvValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "{{") {
		return ""
	}
	return value
}

// resolveEnvValue returns the cleaned value, or fallback when the value is
// empty or an unrendered placeholder.
func resolveEnvValue(value, fallback string) string {
	if cleaned := cleanEnvValue(value); cleaned != "" {
		return cleaned
	}
	return fallback
}
