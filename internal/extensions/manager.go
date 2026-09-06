// Package extensions discovers trusted local CSS/JavaScript web extensions.
// Reload captures assets in memory so a document never mixes partially edited files.
package extensions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const MaxBytes = 32 << 20
const MaxFiles = 512

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type Manifest struct {
	Title         string `yaml:"title" json:"title"`
	Description   string `yaml:"description" json:"description"`
	FormatVersion int    `yaml:"format_version" json:"format_version"`
	CSS           string `yaml:"css" json:"css,omitempty"`
	JS            string `yaml:"js" json:"js,omitempty"`
}
type Entry struct {
	Manifest
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
}
type Snapshot struct {
	Directory      string   `json:"directory"`
	Enabled        []string `json:"enabled"`
	Entries        []Entry  `json:"entries"`
	Errors         []string `json:"errors"`
	Generation     string   `json:"generation"`
	Source         string   `json:"source"`
	Disabled       bool     `json:"disabled"`
	ConfigRevision string   `json:"config_revision"`
	ConfigPath     string   `json:"config_path"`
	assets         map[string][]byte
}
type Manager struct {
	mu      sync.RWMutex
	current *Snapshot
}

func NewManager() *Manager   { return &Manager{} }
func ValidID(id string) bool { return validID.MatchString(id) }
func ValidateEnabled(ids []string) error {
	if len(ids) > 64 {
		return fmt.Errorf("at most 64 extensions may be enabled")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !ValidID(id) || seen[id] {
			return fmt.Errorf("invalid or duplicate extension ID %q", id)
		}
		seen[id] = true
	}
	return nil
}
func safePath(p string) bool {
	return p != "" && fs.ValidPath(p) && !strings.Contains(p, "\\") && !strings.HasPrefix(p, ".")
}
func supported(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".css", ".js", ".mjs", ".json", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".woff", ".woff2", ".ttf", ".otf":
		return true
	}
	return false
}
func scan(dir string, ids []string) (*Snapshot, error) {
	if err := ValidateEnabled(ids); err != nil {
		return nil, err
	}
	s := &Snapshot{Directory: dir, Enabled: append([]string{}, ids...), Entries: []Entry{}, Errors: []string{}, assets: map[string][]byte{}}
	root, err := os.OpenRoot(dir)
	if os.IsNotExist(err) {
		root = nil
	} else if err != nil {
		return nil, err
	}
	var entries []fs.DirEntry
	if root != nil {
		defer root.Close()
		entries, err = fs.ReadDir(root.FS(), ".")
	}
	if os.IsNotExist(err) {
		entries = nil
	} else if err != nil {
		return nil, err
	}
	total := 0
	for _, d := range entries {
		if !d.IsDir() || !ValidID(d.Name()) {
			continue
		}
		id := d.Name()
		e := Entry{ID: id}
		data, err := readBounded(root, path.Join(id, "extension.yaml"), 64<<10)
		if os.IsNotExist(err) {
			continue
		}
		if err == nil {
			err = yaml.Unmarshal(data, &e.Manifest)
		}
		if err == nil && (e.Title == "" || e.FormatVersion != 1 || (e.CSS == "" && e.JS == "")) {
			err = fmt.Errorf("title, format_version: 1 and a css or js entry are required")
		}
		if err == nil && ((e.CSS != "" && (!safePath(e.CSS) || path.Ext(e.CSS) != ".css")) || (e.JS != "" && (!safePath(e.JS) || (path.Ext(e.JS) != ".js" && path.Ext(e.JS) != ".mjs")))) {
			err = fmt.Errorf("entry points must be relative CSS/JS paths")
		}
		files := map[string][]byte{}
		if err == nil {
			count := 0
			size := 0
			err = fs.WalkDir(root.FS(), id, func(p string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.Type()&os.ModeSymlink != 0 {
					return fmt.Errorf("symlinks are not supported: %s", d.Name())
				}
				if d.IsDir() {
					return nil
				}
				rel := strings.TrimPrefix(p, id+"/")
				if !safePath(rel) || !supported(rel) {
					return nil
				}
				count++
				if count > MaxFiles {
					return fmt.Errorf("too many assets (limit %d)", MaxFiles)
				}
				b, err := readBounded(root, p, MaxBytes)
				if err != nil {
					return err
				}
				size += len(b)
				if size > MaxBytes {
					return fmt.Errorf("extension exceeds %d bytes", MaxBytes)
				}
				files[id+"/"+rel] = b
				return nil
			})
			for _, entry := range []string{e.CSS, e.JS} {
				if entry != "" && files[id+"/"+entry] == nil && err == nil {
					err = fmt.Errorf("entry point %s is missing", entry)
				}
			}
		}
		if err != nil {
			e.Error = err.Error()
		} else {
			// Only enabled assets consume snapshot memory or become web-accessible.
			for _, enabled := range ids {
				if enabled == id {
					for name, b := range files {
						total += len(b)
						if total > MaxBytes {
							return nil, fmt.Errorf("enabled extensions exceed %d bytes", MaxBytes)
						}
						s.assets[name] = b
					}
				}
			}
		}
		s.Entries = append(s.Entries, e)
	}
	for _, id := range ids {
		found := false
		for _, e := range s.Entries {
			if e.ID == id {
				found = true
				if e.Error != "" {
					s.Errors = append(s.Errors, id+": "+e.Error)
				}
			}
		}
		if !found {
			s.Errors = append(s.Errors, id+": extension not found")
		}
	}
	h := sha256.New()
	for _, id := range ids {
		fmt.Fprintf(h, "%s\x00", id)
		for _, entry := range s.Entries {
			if entry.ID == id {
				fmt.Fprintf(h, "%s\x00%s\x00%s\x00", entry.CSS, entry.JS, entry.Error)
			}
		}
	}
	keys := make([]string, 0, len(s.assets))
	for k := range s.assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%d\x00", k, len(s.assets[k]))
		h.Write(s.assets[k])
	}
	s.Generation = hex.EncodeToString(h.Sum(nil))[:20]
	return s, nil
}
func readBounded(root *os.Root, p string, limit int) ([]byte, error) {
	info, err := root.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", p)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("file exceeds size limit: %s", p)
	}
	f, err := root.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if len(b) > limit {
		return nil, fmt.Errorf("file grew past size limit: %s", p)
	}
	return b, err
}
func (m *Manager) Reload(dir string, ids []string, source string, disabled bool, revision, configPath string) error {
	var s *Snapshot
	var err error
	if disabled {
		// The process kill switch must work even when the directory is broken.
		s = &Snapshot{Directory: dir, Enabled: []string{}, Entries: []Entry{}, Errors: []string{}, Generation: "disabled", assets: map[string][]byte{}}
	} else {
		s, err = scan(dir, ids)
	}
	if err != nil {
		return err
	}
	s.Source = source
	s.Disabled = disabled
	s.ConfigRevision = revision
	s.ConfigPath = configPath
	m.mu.Lock()
	m.current = s
	m.mu.Unlock()
	return nil
}

// Status returns a detached copy; callers must not mutate the active snapshot.
func (m *Manager) Status() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return Snapshot{Enabled: []string{}, Entries: []Entry{}, Errors: []string{}}
	}
	s := *m.current
	s.Enabled = append([]string{}, s.Enabled...)
	s.Entries = append([]Entry{}, s.Entries...)
	s.Errors = append([]string{}, s.Errors...)
	s.assets = nil
	return s
}
func (m *Manager) Asset(generation, name string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil || m.current.Generation != generation {
		return nil, false
	}
	b, ok := m.current.assets[name]
	return b, ok
}
