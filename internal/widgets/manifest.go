package widgets

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var mountRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Manifest is the parsed contents of a widget.yaml file.
type Manifest struct {
	Title       string   `yaml:"title"`
	Command     []string `yaml:"command"`
	Mount       string   `yaml:"mount"`
	Description string   `yaml:"description"`

	// Set by ScanDir, not from YAML.
	ID  string
	Dir string
}

// PlaceholderMode returns "socket" or "port" based on which placeholder appears
// in Command. Returns an error if both or neither are present.
func (m *Manifest) PlaceholderMode() (string, error) {
	hasSocket, hasPort := false, false
	for _, arg := range m.Command {
		if strings.Contains(arg, "$SOCKET") {
			hasSocket = true
		}
		if strings.Contains(arg, "$PORT") {
			hasPort = true
		}
	}
	switch {
	case hasSocket && hasPort:
		return "", fmt.Errorf("command must contain $SOCKET or $PORT, not both")
	case !hasSocket && !hasPort:
		return "", fmt.Errorf("command must contain exactly one of $SOCKET or $PORT")
	case hasSocket:
		return "socket", nil
	default:
		return "port", nil
	}
}

// SubstArgs returns a copy of argv with all occurrences of placeholder replaced by value.
func SubstArgs(argv []string, placeholder, value string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, placeholder, value)
	}
	return out
}

// ScanDir scans widgetsDir for widget.yaml manifests.
// Directories without widget.yaml are silently skipped.
// Returns valid manifests (sorted by directory name) and any per-widget errors.
// A missing widgetsDir is treated as no widgets (no error).
func ScanDir(widgetsDir string) ([]*Manifest, []error) {
	entries, err := os.ReadDir(widgetsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("scan widgets dir: %w", err)}
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var manifests []*Manifest
	var errs []error
	seenMounts := make(map[string]string)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		dir := filepath.Join(widgetsDir, id)
		yamlPath := filepath.Join(dir, "widget.yaml")

		data, err := os.ReadFile(yamlPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("widget %s: read widget.yaml: %w", id, err))
			continue
		}

		var m Manifest
		if err := yaml.Unmarshal(data, &m); err != nil {
			errs = append(errs, fmt.Errorf("widget %s: parse widget.yaml: %w", id, err))
			continue
		}

		m.ID = id
		m.Dir = dir

		if err := validateManifest(&m); err != nil {
			errs = append(errs, fmt.Errorf("widget %s: %w", id, err))
			continue
		}

		if prev, ok := seenMounts[m.Mount]; ok {
			errs = append(errs, fmt.Errorf("widget %s: duplicate mount %q (already used by %s), skipping", id, m.Mount, prev))
			continue
		}
		seenMounts[m.Mount] = id
		manifests = append(manifests, &m)
	}
	return manifests, errs
}

func validateManifest(m *Manifest) error {
	if strings.TrimSpace(m.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if len(m.Command) == 0 {
		return fmt.Errorf("command is required and must be non-empty")
	}
	if m.Mount == "" {
		m.Mount = m.ID
	}
	if strings.Contains(m.Mount, "/") {
		return fmt.Errorf("mount %q must not contain a slash", m.Mount)
	}
	if !mountRe.MatchString(m.Mount) {
		return fmt.Errorf("mount %q does not match required pattern ^[a-z0-9][a-z0-9-]{0,63}$", m.Mount)
	}
	if _, err := m.PlaceholderMode(); err != nil {
		return err
	}
	return nil
}
