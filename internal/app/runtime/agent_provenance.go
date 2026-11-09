package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// legacySearchIdentityAmbiguous scans definitions directly: registry listing
// deduplicates and can hide a search-path definition behind a trusted namesake.
// Old sessions persisted YAML names as well as directory names. Any unreadable
// or malformed definition prevents proving that a bare reference is trusted.
func legacySearchIdentityAmbiguous(name string, roots []string) (bool, error) {
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("scan legacy agent provenance %s: %w", root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			dir := filepath.Join(root, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				info, err := os.Stat(dir)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					return false, fmt.Errorf("stat legacy agent provenance %s: %w", dir, err)
				}
				if !info.IsDir() {
					continue
				}
			}
			path := filepath.Join(dir, "agent.yaml")
			data, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return false, fmt.Errorf("read legacy agent provenance %s: %w", path, err)
			}
			var definition struct {
				Name string `yaml:"name"`
			}
			if err := yaml.Unmarshal(data, &definition); err != nil {
				return false, fmt.Errorf("parse legacy agent provenance %s: %w", path, err)
			}
			if entry.Name() == name || definition.Name == name {
				return true, nil
			}
		}
	}
	return false, nil
}
