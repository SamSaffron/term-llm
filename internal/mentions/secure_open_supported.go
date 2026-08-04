//go:build unix || windows || wasip1

package mentions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// openSecureMentionRoot opens path by walking from its filesystem or volume
// root one descriptor at a time. The lstat/open/stat identity check rejects a
// component replaced while it is being opened, including the final component
// of the configured root. Once returned, os.Root performs subsequent path
// resolution relative to the retained directory descriptor/handle.
func openSecureMentionRoot(path string) (secureMentionRoot, error) {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("secure mention root is not absolute")
	}

	volume := filepath.VolumeName(path)
	anchorPath := string(filepath.Separator)
	if volume != "" {
		anchorPath = volume + string(filepath.Separator)
	}
	relative, err := filepath.Rel(anchorPath, path)
	if err != nil || !filepath.IsLocal(relative) {
		return nil, errors.New("secure mention root is outside its volume anchor")
	}

	current, err := os.OpenRoot(anchorPath)
	if err != nil {
		return nil, fmt.Errorf("open mention root anchor: %w", err)
	}
	if relative == "." {
		return current, nil
	}

	components := strings.Split(relative, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			current.Close()
			return nil, errors.New("invalid secure mention root component")
		}
		expected, err := current.Lstat(component)
		if err != nil || expected.Mode()&os.ModeSymlink != 0 || !expected.IsDir() {
			current.Close()
			return nil, errors.New("mention root component is not a stable directory")
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			current.Close()
			return nil, fmt.Errorf("open mention root component: %w", err)
		}
		actual, statErr := next.Stat(".")
		if statErr != nil || !os.SameFile(expected, actual) {
			next.Close()
			current.Close()
			return nil, errors.New("mention root component changed while opening")
		}
		current.Close()
		current = next
	}
	return current, nil
}
