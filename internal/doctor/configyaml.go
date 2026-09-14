package doctor

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/config"

	"gopkg.in/yaml.v3"
)

type unknownKey struct {
	path string
	line int
}

func documentMapping(root *yaml.Node) *yaml.Node {
	if root == nil {
		return nil
	}
	node := root
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil
		}
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

// collectUnknownKeys returns the outermost keys the config loader ignores.
// Nested keys under an unknown parent are omitted because they are ignored for
// the same reason, and reporting each one only adds noise.
func collectUnknownKeys(root *yaml.Node) []unknownKey {
	var out []unknownKey
	var walk func(node *yaml.Node, prefix string)
	walk = func(node *yaml.Node, prefix string) {
		mapping := node
		if mapping.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(mapping.Content); i += 2 {
			keyNode := mapping.Content[i]
			valueNode := mapping.Content[i+1]
			path := keyNode.Value
			if prefix != "" {
				path = prefix + "." + keyNode.Value
			}
			if !config.IsKnownKey(path) {
				out = append(out, unknownKey{path: path, line: keyNode.Line})
				continue
			}
			if valueNode.Kind == yaml.MappingNode {
				walk(valueNode, path)
			}
		}
	}
	if mapping := documentMapping(root); mapping != nil {
		walk(mapping, "")
	}
	return out
}

// suggestKey returns the closest known key for an unknown key path, or an empty
// string when nothing is close enough to be useful.
func suggestKey(path string) string {
	parts := strings.Split(path, ".")
	target := path
	prefix := ""
	var candidates []string

	switch {
	case parts[0] == "providers" && len(parts) >= 3:
		target = strings.Join(parts[2:], ".")
		prefix = strings.Join(parts[:2], ".") + "."
		candidates = sortedKeys(config.KnownProviderKeys)
	case path != "agents.preferences" && strings.HasPrefix(path, "agents.preferences.") && len(parts) == 4:
		target = parts[3]
		prefix = strings.Join(parts[:3], ".") + "."
		candidates = sortedKeys(config.KnownAgentPreferenceKeys)
	default:
		candidates = config.KnownKeyPaths()
	}

	best := ""
	bestDistance := -1
	for _, candidate := range candidates {
		distance := levenshtein(target, candidate)
		if bestDistance == -1 || distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	if best == "" {
		return ""
	}
	limit := len(target) / 3
	if limit < 2 {
		limit = 2
	}
	if bestDistance > limit {
		return ""
	}
	return prefix + best
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func levenshtein(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	previous := make([]int, len(br)+1)
	current := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		current[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			current[j] = min3(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// commentOutConfigKey comments out the block belonging to keyPath, preserving
// every other byte of the file including comments. The original file is copied
// to a timestamped backup first.
func commentOutConfigKey(path, keyPath string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}

	updated, err := commentOutYAMLKey(data, keyPath)
	if err != nil {
		return err
	}
	if updated == nil {
		return nil
	}

	backup := fmt.Sprintf("%s.doctor-bak-%s", path, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backup, data, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write backup: %w", err)
	}
	if err := os.WriteFile(path, updated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// commentOutYAMLKey returns source with the block for keyPath commented out, or
// nil when the key is not present.
func commentOutYAMLKey(source []byte, keyPath string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(source, &root); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	keyNode := findKeyNode(documentMapping(&root), strings.Split(keyPath, "."))
	if keyNode == nil {
		return nil, nil
	}

	lines := strings.Split(string(source), "\n")
	start := keyNode.Line - 1
	if start < 0 || start >= len(lines) {
		return nil, fmt.Errorf("config key %q is outside the file", keyPath)
	}
	indent := keyNode.Column - 1
	end := start
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(stripCR(lines[i])) == "" {
			continue
		}
		if leadingSpaces(lines[i]) <= indent {
			break
		}
		end = i
	}

	pad := strings.Repeat(" ", indent)
	for i := start; i <= end; i++ {
		line := lines[i]
		if strings.TrimSpace(stripCR(line)) == "" {
			continue
		}
		carriage := ""
		if strings.HasSuffix(line, "\r") {
			line, carriage = strings.TrimSuffix(line, "\r"), "\r"
		}
		lines[i] = pad + "# " + strings.TrimPrefix(line, pad) + carriage
	}

	note := pad + "# unknown key commented out by term-llm doctor"
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:start]...)
	out = append(out, note)
	out = append(out, lines[start:]...)
	return []byte(strings.Join(out, "\n")), nil
}

func findKeyNode(mapping *yaml.Node, segments []string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode || len(segments) == 0 {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		keyNode := mapping.Content[i]
		if keyNode.Value != segments[0] {
			continue
		}
		if len(segments) == 1 {
			return keyNode
		}
		return findKeyNode(mapping.Content[i+1], segments[1:])
	}
	return nil
}

func leadingSpaces(line string) int {
	count := 0
	for _, r := range line {
		if r != ' ' {
			break
		}
		count++
	}
	return count
}

func stripCR(line string) string { return strings.TrimSuffix(line, "\r") }
