package skills

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatterData holds the raw frontmatter fields for parsing.
type frontmatterData struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	AllowedTools  any               `yaml:"allowed-tools,omitempty"` // Can be string, list, or nil
	Metadata      map[string]string `yaml:"metadata,omitempty"`
}

// ParseSkillMD parses a SKILL.md file and returns a Skill.
// The loadBody parameter controls whether to load the full Markdown body.
func ParseSkillMD(path string, loadBody bool) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read SKILL.md: %w", err)
	}

	return ParseSkillMDContent(string(data), loadBody)
}

// ParseSkillMDContent parses SKILL.md content from a string.
func ParseSkillMDContent(content string, loadBody bool) (*Skill, error) {
	// Split frontmatter and body
	frontmatter, body, err := splitFrontmatter(content)
	if err != nil {
		return nil, err
	}

	// Parse frontmatter into known fields
	var fm frontmatterData
	if err := yaml.Unmarshal([]byte(frontmatter), &fm); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	// Parse again into a generic map to capture unknown fields
	var rawMap map[string]any
	if err := yaml.Unmarshal([]byte(frontmatter), &rawMap); err != nil {
		return nil, fmt.Errorf("parse frontmatter extras: %w", err)
	}

	// Build the skill
	skill := &Skill{
		Name:          fm.Name,
		Description:   fm.Description,
		License:       fm.License,
		Compatibility: fm.Compatibility,
		Metadata:      fm.Metadata,
		Extras:        extractExtras(rawMap),
		loaded:        loadBody,
	}

	// Parse allowed-tools from various formats
	skill.AllowedTools = parseAllowedTools(fm.AllowedTools)

	// Load body if requested
	if loadBody {
		skill.Body = strings.TrimSpace(body)
	}

	return skill, nil
}

// splitFrontmatter extracts YAML frontmatter and Markdown body.
// Frontmatter must be delimited by --- on its own lines.
func splitFrontmatter(content string) (frontmatter, body string, err error) {
	scanner := bufio.NewScanner(strings.NewReader(content))

	// Look for opening ---
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		// Skip any leading whitespace/blank lines before frontmatter
		if strings.TrimSpace(line) != "" {
			return "", "", fmt.Errorf("SKILL.md must start with YAML frontmatter (---)")
		}
	}

	// Read frontmatter until closing ---
	var fmLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		fmLines = append(fmLines, line)
	}

	if len(fmLines) == 0 {
		return "", "", fmt.Errorf("empty frontmatter in SKILL.md")
	}

	// Rest is body
	var bodyLines []string
	for scanner.Scan() {
		bodyLines = append(bodyLines, scanner.Text())
	}

	return strings.Join(fmLines, "\n"), strings.Join(bodyLines, "\n"), nil
}

// parseAllowedTools handles the various formats for allowed-tools:
// - Space-delimited string (spec): "read write shell"
// - Comma-delimited string (Claude Code): "read, write, shell"
// - YAML list: ["read", "write", "shell"]
func parseAllowedTools(v any) []string {
	if v == nil {
		return nil
	}

	switch val := v.(type) {
	case string:
		// Try comma-delimited first (Claude Code format)
		if strings.Contains(val, ",") {
			parts := strings.Split(val, ",")
			var tools []string
			for _, p := range parts {
				if t := strings.TrimSpace(p); t != "" {
					tools = append(tools, t)
				}
			}
			return tools
		}
		// Fall back to space-delimited (spec format)
		return strings.Fields(val)

	case []any:
		var tools []string
		for _, item := range val {
			if s, ok := item.(string); ok {
				tools = append(tools, s)
			}
		}
		return tools

	case []string:
		return val

	default:
		return nil
	}
}

// extractExtras returns frontmatter keys that are not standard fields.
func extractExtras(raw map[string]any) map[string]any {
	standardKeys := map[string]bool{
		"name":          true,
		"description":   true,
		"license":       true,
		"compatibility": true,
		"allowed-tools": true,
		"metadata":      true,
	}

	extras := make(map[string]any)
	for k, v := range raw {
		if !standardKeys[k] {
			extras[k] = v
		}
	}

	if len(extras) == 0 {
		return nil
	}
	return extras
}

// LoadFromDir loads a skill from a directory containing SKILL.md.
// If loadBody is false, only metadata is loaded (for discovery).
func LoadFromDir(dir string, source SkillSource, loadBody bool) (*Skill, error) {
	// Try SKILL.md first, then skill.md (with warning)
	skillPath := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		lowerPath := filepath.Join(dir, "skill.md")
		if _, err := os.Stat(lowerPath); err == nil {
			fmt.Fprintf(os.Stderr, "warning: skill.md should be SKILL.md: %s\n", lowerPath)
			skillPath = lowerPath
		} else {
			return nil, fmt.Errorf("SKILL.md not found in %s", dir)
		}
	}

	skill, err := ParseSkillMD(skillPath, loadBody)
	if err != nil {
		return nil, err
	}

	// Set source info
	skill.Source = source
	skill.SourcePath = dir

	// Validate that name matches directory (if name is set)
	dirName := filepath.Base(dir)
	if skill.Name == "" {
		// Derive name from directory if not set
		skill.Name = dirName
	} else if skill.Name != dirName {
		return nil, fmt.Errorf("skill name %q must match directory name %q", skill.Name, dirName)
	}

	// Discover resources if loading full content
	if loadBody {
		skill.References = discoverFiles(dir, "references")
		skill.Scripts = discoverFiles(dir, "scripts")
		skill.Assets = discoverFiles(dir, "assets")
	}

	return skill, nil
}

// discoverFiles returns files in a subdirectory of the skill root.
func discoverFiles(skillDir, subdir string) []string {
	dir := filepath.Join(skillDir, subdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}
	return files
}

// IsSkillDir checks if a directory contains a SKILL.md file.
func IsSkillDir(dir string) bool {
	// Check for SKILL.md (preferred)
	if info, err := os.Stat(filepath.Join(dir, "SKILL.md")); err == nil && !info.IsDir() {
		return true
	}
	// Check for skill.md (lowercase, allowed with warning)
	if info, err := os.Stat(filepath.Join(dir, "skill.md")); err == nil && !info.IsDir() {
		return true
	}
	return false
}
