// Package servepath resolves filesystem paths for served media without HTTP state.
package servepath

import (
	"os"
	"path/filepath"
	"strings"
)

func RoutePath(baseRoute, baseDir, servedPath string) string {
	absDir, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		absDir, err = filepath.Abs(baseDir)
		if err != nil {
			return baseRoute + filepath.Base(servedPath)
		}
	}
	absServed, err := filepath.EvalSymlinks(servedPath)
	if err != nil {
		absServed, err = filepath.Abs(servedPath)
		if err != nil {
			return baseRoute + filepath.Base(servedPath)
		}
	}

	relPath, err := filepath.Rel(absDir, absServed)
	if err != nil || relPath == "." || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return baseRoute + filepath.Base(servedPath)
	}

	return baseRoute + filepath.ToSlash(relPath)
}

func CanonicalizeExistingPath(p string) (string, error) {
	absPath, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func CanonicalizeDirForWrite(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absDir)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}

	parent := filepath.Dir(absDir)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Clean(absDir), nil
		}
		return "", err
	}
	return filepath.Join(filepath.Clean(resolvedParent), filepath.Base(absDir)), nil
}
