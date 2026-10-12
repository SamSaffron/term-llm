package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// editConfigFile applies edit to the YAML tree of config.yaml and writes the
// result back atomically, preserving comments, key order, and key case. A
// missing or empty file starts as an empty mapping. When edit reports no
// change, the file is left untouched.
func editConfigFile(edit func(root *yaml.Node) (bool, error)) error {
	path, err := GetConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	doc, err := readConfigDocument(path)
	if err != nil {
		return err
	}
	changed, err := edit(doc.Content[0])
	if err != nil || !changed {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := WriteFileAtomically(path, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// readConfigDocument parses path into a document node whose root is a
// mapping.
func readConfigDocument(path string) (*yaml.Node, error) {
	doc := &yaml.Node{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if doc.Kind == 0 {
		doc.Kind = yaml.DocumentNode
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		*root = yaml.Node{Kind: yaml.MappingNode, HeadComment: root.HeadComment}
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("parse config: top level of %s is not a mapping", path)
	}
	return doc, nil
}

// mappingValueIndex returns the index of key's value in mapping.Content, or
// -1. Keys match case-insensitively, as they do when the config is loaded.
func mappingValueIndex(mapping *yaml.Node, key string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if strings.EqualFold(mapping.Content[i].Value, key) {
			return i + 1
		}
	}
	return -1
}

// childMapping returns the mapping stored under key, creating it (or
// replacing a non-mapping value) as needed.
func childMapping(mapping *yaml.Node, key string) *yaml.Node {
	if i := mappingValueIndex(mapping, key); i >= 0 {
		child := mapping.Content[i]
		if child.Kind != yaml.MappingNode {
			*child = yaml.Node{Kind: yaml.MappingNode, LineComment: child.LineComment}
		}
		if len(child.Content) == 0 {
			// Write new keys in block style rather than inside "{}".
			child.Style &^= yaml.FlowStyle
		}
		return child
	}
	child := &yaml.Node{Kind: yaml.MappingNode}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, child)
	return child
}

// setConfigValue sets the dotted key path below mapping to value, keeping the
// line comment of a value it replaces.
func setConfigValue(mapping *yaml.Node, key string, value any) error {
	parts := strings.Split(key, ".")
	for _, part := range parts[:len(parts)-1] {
		mapping = childMapping(mapping, part)
	}
	node := &yaml.Node{}
	if err := node.Encode(value); err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	last := parts[len(parts)-1]
	if i := mappingValueIndex(mapping, last); i >= 0 {
		node.LineComment = mapping.Content[i].LineComment
		mapping.Content[i] = node
		return nil
	}
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: last}, node)
	return nil
}

// configValue is one key/value pair to write with setConfigValues.
type configValue struct {
	key   string
	value any
}

// setConfigValues sets each value below mapping and reports a change.
func setConfigValues(mapping *yaml.Node, values []configValue) (bool, error) {
	for _, v := range values {
		if err := setConfigValue(mapping, v.key, v.value); err != nil {
			return false, err
		}
	}
	return true, nil
}

// nonNilSlice returns s, or an empty slice when s is nil, so it encodes as []
// rather than null.
func nonNilSlice[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// lookupMapping returns the mapping at the dotted key path below mapping, or
// nil if any part is missing or not a mapping.
func lookupMapping(mapping *yaml.Node, key string) *yaml.Node {
	for _, part := range strings.Split(key, ".") {
		i := mappingValueIndex(mapping, part)
		if i < 0 || mapping.Content[i].Kind != yaml.MappingNode {
			return nil
		}
		mapping = mapping.Content[i]
	}
	return mapping
}

// deleteMappingKey removes key from mapping and reports whether it existed.
func deleteMappingKey(mapping *yaml.Node, key string) bool {
	i := mappingValueIndex(mapping, key)
	if i < 0 {
		return false
	}
	mapping.Content = append(mapping.Content[:i-1], mapping.Content[i+1:]...)
	return true
}
