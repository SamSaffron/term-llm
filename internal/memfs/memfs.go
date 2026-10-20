//go:build js && wasm

// Package memfs is the browser runtime's in-memory filesystem. It has no
// operating-system backing and its contents disappear with the page.
package memfs

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The browser runtime has no operating-system filesystem. This file implements
// the same small platform seam as platform_fs_native.go on top of an in-memory
// tree, so the real read/write/edit/glob tools run unchanged inside js/wasm.
// Nothing is persisted; the workspace lives for the page's lifetime only.

// TempFile is the subset of *os.File used by atomic file writes.
type TempFile interface {
	io.Writer
	io.StringWriter
	Sync() error
	Close() error
	Chmod(fs.FileMode) error
	Name() string
}

type memFS struct {
	mu   sync.Mutex
	root *memNode
}

type memNode struct {
	name     string
	dir      bool
	mode     fs.FileMode
	data     []byte
	mod      time.Time
	children map[string]*memNode
}

var workspace memFS

func init() {
	workspace.root = newMemDir("/")
	workspace.mustMkdirAllLocked("/workspace")
	workspace.mustMkdirAllLocked("/tmp")
}

func newMemDir(name string) *memNode {
	return &memNode{name: name, dir: true, mode: fs.ModeDir | 0o755, mod: time.Now(), children: map[string]*memNode{}}
}

func memError(op, name string) error {
	return &os.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
}

// normalizePath makes every tool path absolute. Relative paths resolve against
// the virtual workspace, mirroring the native base-dir contract.
func normalizePath(name string) string {
	if name == "" {
		return "/workspace"
	}
	name = filepath.ToSlash(name)
	if !path.IsAbs(name) {
		name = path.Join("/workspace", name)
	}
	return path.Clean(name)
}

func (m *memFS) lookupLocked(name string) (*memNode, error) {
	name = normalizePath(name)
	if name == "/" {
		return m.root, nil
	}
	current := m.root
	for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		if part == "" {
			continue
		}
		if current == nil || !current.dir {
			return nil, memError("open", name)
		}
		current = current.children[part]
	}
	if current == nil {
		return nil, memError("open", name)
	}
	return current, nil
}

func (m *memFS) parentLocked(name string) (*memNode, string, error) {
	name = normalizePath(name)
	if name == "/" {
		return nil, "", memError("open", name)
	}
	dir, base := path.Split(name)
	parent, err := m.lookupLocked(strings.TrimSuffix(dir, "/"))
	if err != nil {
		return nil, "", err
	}
	if !parent.dir {
		return nil, "", &os.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	return parent, base, nil
}

func (m *memFS) mustMkdirAllLocked(name string) *memNode {
	node, err := m.mkdirAllLocked(name)
	if err != nil {
		panic(err)
	}
	return node
}

func (m *memFS) mkdirAllLocked(name string) (*memNode, error) {
	name = normalizePath(name)
	current := m.root
	if name == "/" {
		return current, nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		if part == "" {
			continue
		}
		next := current.children[part]
		if next == nil {
			next = newMemDir(part)
			current.children[part] = next
		} else if !next.dir {
			return nil, &os.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
		}
		current = next
	}
	return current, nil
}

func (m *memFS) writeFileLocked(name string, data []byte, mode fs.FileMode) error {
	parent, base, err := m.parentLocked(name)
	if err != nil {
		return err
	}
	node := parent.children[base]
	if node == nil {
		node = &memNode{name: base, mode: mode.Perm(), mod: time.Now()}
		parent.children[base] = node
	} else if node.dir {
		return &os.PathError{Op: "write", Path: name, Err: fs.ErrInvalid}
	}
	node.data = append(node.data[:0], data...)
	node.mode = mode.Perm()
	node.mod = time.Now()
	return nil
}

func (m *memFS) removeLocked(name string) error {
	parent, base, err := m.parentLocked(name)
	if err != nil {
		return err
	}
	if parent.children[base] == nil {
		return memError("remove", name)
	}
	delete(parent.children, base)
	return nil
}

type memFileInfo struct {
	name string
	size int64
	mode fs.FileMode
	mod  time.Time
}

func (i memFileInfo) Name() string       { return i.name }
func (i memFileInfo) Size() int64        { return i.size }
func (i memFileInfo) Mode() fs.FileMode  { return i.mode }
func (i memFileInfo) ModTime() time.Time { return i.mod }
func (i memFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i memFileInfo) Sys() any           { return nil }

func fileInfoOf(name string, node *memNode) fs.FileInfo {
	info := memFileInfo{name: path.Base(name), mode: node.mode, mod: node.mod}
	if !node.dir {
		info.size = int64(len(node.data))
	}
	return info
}

func ReadFile(name string) ([]byte, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	node, err := workspace.lookupLocked(name)
	if err != nil {
		return nil, err
	}
	if node.dir {
		return nil, &os.PathError{Op: "read", Path: name, Err: fs.ErrInvalid}
	}
	return append([]byte(nil), node.data...), nil
}

// WriteFile writes data to name, creating missing parent directories. It is the
// browser counterpart of os.WriteFile for callers outside the tools package
// (for example the run_javascript workspace sync).
func WriteFile(name string, data []byte, perm fs.FileMode) error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	name = normalizePath(name)
	if _, err := workspace.mkdirAllLocked(path.Dir(name)); err != nil {
		return err
	}
	return workspace.writeFileLocked(name, data, perm)
}

func Open(name string) (io.ReadCloser, error) {
	data, err := ReadFile(name)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func Stat(name string) (fs.FileInfo, error) {
	return Lstat(name)
}

func Lstat(name string) (fs.FileInfo, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	node, err := workspace.lookupLocked(name)
	if err != nil {
		return nil, err
	}
	return fileInfoOf(name, node), nil
}

func MkdirAll(name string, perm fs.FileMode) error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	node, err := workspace.mkdirAllLocked(name)
	if err != nil {
		return err
	}
	if perm != 0 {
		node.mode = fs.ModeDir | perm.Perm()
	}
	return nil
}

func Remove(name string) error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	return workspace.removeLocked(name)
}

func Chmod(name string, mode fs.FileMode) error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	node, err := workspace.lookupLocked(name)
	if err != nil {
		return err
	}
	if node.dir {
		node.mode = fs.ModeDir | mode.Perm()
	} else {
		node.mode = mode.Perm()
	}
	return nil
}

func Rename(oldPath, newPath string) error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	node, err := workspace.lookupLocked(oldPath)
	if err != nil {
		return &os.PathError{Op: "rename", Path: oldPath, Err: fs.ErrNotExist}
	}
	parent, base, err := workspace.parentLocked(newPath)
	if err != nil {
		return err
	}
	if existing := parent.children[base]; existing != nil && existing.dir {
		return &os.PathError{Op: "rename", Path: newPath, Err: fs.ErrInvalid}
	}
	oldParent, oldBase, err := workspace.parentLocked(oldPath)
	if err != nil {
		return err
	}
	delete(oldParent.children, oldBase)
	node.name = base
	parent.children[base] = node
	return nil
}

func Readlink(name string) (string, error) {
	return "", memError("readlink", name)
}

func Getwd() (string, error) { return "/workspace", nil }

func DirFS(root string) fs.FS { return memDirFS{root: normalizePath(root)} }

func EvalSymlinks(name string) (string, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	if _, err := workspace.lookupLocked(name); err != nil {
		return "", err
	}
	return normalizePath(name), nil
}

type memTempFile struct {
	fs     *memFS
	path   string
	offset int
	closed bool
}

func (f *memTempFile) Name() string { return f.path }

func (f *memTempFile) Write(p []byte) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return 0, &os.PathError{Op: "write", Path: f.path, Err: fs.ErrClosed}
	}
	node, err := f.fs.lookupLocked(f.path)
	if err != nil {
		return 0, err
	}
	end := f.offset + len(p)
	if end > len(node.data) {
		node.data = append(node.data, make([]byte, end-len(node.data))...)
	}
	copy(node.data[f.offset:], p)
	f.offset = end
	node.mod = time.Now()
	return len(p), nil
}

func (f *memTempFile) WriteString(s string) (int, error) { return f.Write([]byte(s)) }

func (f *memTempFile) Sync() error { return nil }

func (f *memTempFile) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	f.closed = true
	return nil
}

func (f *memTempFile) Chmod(mode fs.FileMode) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	node, err := f.fs.lookupLocked(f.path)
	if err != nil {
		return err
	}
	node.mode = mode.Perm()
	return nil
}

func CreateTemp(dir, pattern string) (TempFile, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	dir = normalizePath(dir)
	if _, err := workspace.lookupLocked(dir); err != nil {
		return nil, &os.PathError{Op: "createtemp", Path: dir, Err: fs.ErrNotExist}
	}
	for attempt := 0; attempt < 100; attempt++ {
		suffix := memRandomSuffix()
		name := pattern
		if strings.Contains(name, "*") {
			name = strings.Replace(name, "*", suffix, 1)
		} else {
			name += suffix
		}
		full := path.Join(dir, name)
		if _, err := workspace.lookupLocked(full); err == nil {
			continue
		}
		if err := workspace.writeFileLocked(full, nil, 0o600); err != nil {
			return nil, err
		}
		return &memTempFile{fs: &workspace, path: full}, nil
	}
	return nil, &os.PathError{Op: "createtemp", Path: dir, Err: fs.ErrExist}
}

func memRandomSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "tmp"
	}
	return hex.EncodeToString(buf[:])
}

type memDirFS struct{ root string }

func (d memDirFS) Open(name string) (fs.File, error) {
	name = strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(name)), "/")
	full := d.root
	if name != "" && name != "." {
		full = path.Join(d.root, name)
	}
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	node, err := workspace.lookupLocked(full)
	if err != nil {
		return nil, err
	}
	if node.dir {
		entries := make([]fs.DirEntry, 0, len(node.children))
		names := make([]string, 0, len(node.children))
		for child := range node.children {
			names = append(names, child)
		}
		sort.Strings(names)
		for _, child := range names {
			childNode := node.children[child]
			entries = append(entries, fs.FileInfoToDirEntry(fileInfoOf(child, childNode)))
		}
		return &memDirFile{info: fileInfoOf(full, node), entries: entries}, nil
	}
	return &memReadFile{info: fileInfoOf(full, node), reader: bytes.NewReader(append([]byte(nil), node.data...))}, nil
}

func (d memDirFS) Stat(name string) (fs.FileInfo, error) { return Lstat(joinMemRoot(d.root, name)) }

func joinMemRoot(root, name string) string {
	name = strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(name)), "/")
	if name == "" || name == "." {
		return root
	}
	return path.Join(root, name)
}

type memDirFile struct {
	info    fs.FileInfo
	entries []fs.DirEntry
	offset  int
	closed  bool
}

func (f *memDirFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *memDirFile) Read([]byte) (int, error) {
	return 0, &os.PathError{Op: "read", Path: f.info.Name(), Err: fs.ErrInvalid}
}

func (f *memDirFile) Close() error {
	f.closed = true
	return nil
}

func (f *memDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.closed {
		return nil, &os.PathError{Op: "readdir", Path: f.info.Name(), Err: fs.ErrClosed}
	}
	if n <= 0 || n > len(f.entries)-f.offset {
		n = len(f.entries) - f.offset
	}
	entries := f.entries[f.offset : f.offset+n]
	f.offset += n
	return entries, nil
}

type memReadFile struct {
	info   fs.FileInfo
	reader *bytes.Reader
	closed bool
}

func (f *memReadFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *memReadFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, &os.PathError{Op: "read", Path: f.info.Name(), Err: fs.ErrClosed}
	}
	return f.reader.Read(p)
}

func (f *memReadFile) Close() error {
	f.closed = true
	return nil
}

// File describes one file in the workspace.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// SetWorkspace replaces the in-memory workspace with seed contents.
func SetWorkspace(root string, seed map[string]string) error {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	workspace.root = newMemDir("/")
	if _, err := workspace.mkdirAllLocked(root); err != nil {
		return err
	}
	if _, err := workspace.mkdirAllLocked("/tmp"); err != nil {
		return err
	}
	for name, content := range seed {
		full := path.Join(root, name)
		if _, err := workspace.mkdirAllLocked(path.Dir(full)); err != nil {
			return err
		}
		if err := workspace.writeFileLocked(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot lists the regular files under root.
func Snapshot(root string) ([]File, error) {
	workspace.mu.Lock()
	defer workspace.mu.Unlock()
	node, err := workspace.lookupLocked(root)
	if err != nil {
		return nil, err
	}
	var files []File
	var walk func(prefix string, n *memNode)
	walk = func(prefix string, n *memNode) {
		names := make([]string, 0, len(n.children))
		for name := range n.children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := n.children[name]
			rel := name
			if prefix != "" {
				rel = prefix + "/" + name
			}
			if child.dir {
				walk(rel, child)
				continue
			}
			files = append(files, File{Path: rel, Size: int64(len(child.data))})
		}
	}
	walk("", node)
	return files, nil
}

// isTextFileBusy is always false on WebAssembly: there are no subprocesses.
