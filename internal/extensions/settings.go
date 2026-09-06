package extensions

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// SettingsFile is deliberately inside the extension directory, not the main
// term-llm configuration directory. Builders need no credential/config access.
const SettingsFile = "extensions.yaml"
const maxSettingsBytes = 64 << 10

type Settings struct {
	Enabled []string `yaml:"enabled"`
}

func ReadSettings(dir string) (data []byte, exists bool, err error) {
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	info, err := root.Lstat(SettingsFile)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%s must be a regular file, not a symlink", SettingsFile)
	}
	f, err := root.Open(SettingsFile)
	if err != nil {
		return nil, true, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSettingsBytes+1))
	if len(b) > maxSettingsBytes {
		return nil, true, fmt.Errorf("%s exceeds %d bytes", SettingsFile, maxSettingsBytes)
	}
	return b, true, err
}
func ParseSettings(data []byte) (Settings, error) {
	var s Settings
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&s); err != nil {
		return s, fmt.Errorf("parse %s: %w", SettingsFile, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return s, fmt.Errorf("%s must contain one YAML document", SettingsFile)
	}
	var keys map[string]any
	if err := yaml.Unmarshal(data, &keys); err != nil {
		return s, err
	}
	if _, ok := keys["enabled"]; !ok {
		return s, fmt.Errorf("%s requires an enabled list", SettingsFile)
	}
	if err := ValidateEnabled(s.Enabled); err != nil {
		return s, err
	}
	s.Enabled = append([]string{}, s.Enabled...)
	return s, nil
}

// SettingsBytes updates only the enabled list, preserving comments/other YAML
// presentation without ever loading or rewriting the main config file.
func SettingsBytes(previous []byte, ids []string) ([]byte, error) {
	if err := ValidateEnabled(ids); err != nil {
		return nil, err
	}
	var doc yaml.Node
	if len(bytes.TrimSpace(previous)) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	} else if err := yaml.Unmarshal(previous, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", SettingsFile)
	}
	sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, id := range ids {
		sequence.Content = append(sequence.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: id})
	}
	mapping := doc.Content[0]
	found := false
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "enabled" {
			old := mapping.Content[i+1]
			sequence.HeadComment = old.HeadComment
			sequence.LineComment = old.LineComment
			sequence.FootComment = old.FootComment
			mapping.Content[i+1] = sequence
			found = true
			break
		}
	}
	if !found {
		mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"}, sequence)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteSettings atomically replaces the directory entry via os.Root. In
// particular, even a racing symlink cannot redirect this write into main config.
func WriteSettings(dir string, data []byte) error {
	if len(data) > maxSettingsBytes {
		return fmt.Errorf("settings exceed size limit")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	if info, err := root.Lstat(SettingsFile); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s must not be a symlink", SettingsFile)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	tmp := ".extensions-" + hex.EncodeToString(nonce[:]) + ".tmp"
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return root.Rename(tmp, SettingsFile)
}
