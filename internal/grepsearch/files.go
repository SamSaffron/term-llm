package grepsearch

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// CollectFiles collects files to search.
func CollectFiles(searchPath, include, exclude string) ([]string, error) {
	info, err := os.Stat(searchPath)
	if err != nil {
		return nil, err
	}

	// Single file
	if !info.IsDir() {
		return []string{searchPath}, nil
	}

	// Directory - walk and collect files
	var files []string
	err = filepath.WalkDir(searchPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Skip errors
		}

		relPath, _ := filepath.Rel(searchPath, path)

		// Skip hidden directories
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}

		if d.IsDir() {
			// Skip excluded directories early
			if exclude != "" {
				if matched, _ := doublestar.Match(exclude, relPath); matched {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Apply include filter against relative path
		if include != "" {
			if matched, _ := doublestar.Match(include, relPath); !matched {
				return nil
			}
		}

		// Apply exclude filter against relative path
		if exclude != "" {
			if matched, _ := doublestar.Match(exclude, relPath); matched {
				return nil
			}
		}

		files = append(files, path)
		return nil
	})

	return files, err
}

// SortFilesByMtime sorts files by modification time (newest first).
func SortFilesByMtime(files []string) {
	type fileInfo struct {
		path  string
		mtime int64
	}

	infos := make([]fileInfo, 0, len(files))
	for _, f := range files {
		info, err := os.Stat(f)
		if err != nil {
			infos = append(infos, fileInfo{path: f, mtime: 0})
			continue
		}
		infos = append(infos, fileInfo{path: f, mtime: info.ModTime().Unix()})
	}

	sort.Slice(infos, func(i, j int) bool {
		return infos[i].mtime > infos[j].mtime
	})

	for i, info := range infos {
		files[i] = info.path
	}
}
