//go:build !js && !wasip1

// Package memfs is the browser runtime's in-memory filesystem. This native
// stub exists only so the package participates in `go build ./...`; every
// operation reports that the virtual workspace is unavailable.
package memfs

import (
	"errors"
	"io"
	"io/fs"
)

// TempFile is the subset of *os.File used by atomic file writes.
type TempFile interface {
	io.Writer
	io.StringWriter
	Sync() error
	Close() error
	Chmod(fs.FileMode) error
	Name() string
}

// File describes one file in the workspace.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ErrUnavailable reports that the in-memory workspace is browser-only.
var ErrUnavailable = errors.New("in-memory workspace is only available in js/wasm builds")

func ReadFile(string) ([]byte, error)              { return nil, ErrUnavailable }
func WriteFile(string, []byte, fs.FileMode) error  { return ErrUnavailable }
func Open(string) (io.ReadCloser, error)           { return nil, ErrUnavailable }
func Stat(string) (fs.FileInfo, error)             { return nil, ErrUnavailable }
func Lstat(string) (fs.FileInfo, error)            { return nil, ErrUnavailable }
func MkdirAll(string, fs.FileMode) error           { return ErrUnavailable }
func CreateTemp(string, string) (TempFile, error)  { return nil, ErrUnavailable }
func Remove(string) error                          { return ErrUnavailable }
func Chmod(string, fs.FileMode) error              { return ErrUnavailable }
func Rename(string, string) error                  { return ErrUnavailable }
func Readlink(string) (string, error)              { return "", ErrUnavailable }
func Getwd() (string, error)                       { return "", ErrUnavailable }
func DirFS(string) fs.FS                           { return unavailableFS{} }
func EvalSymlinks(string) (string, error)          { return "", ErrUnavailable }
func SetWorkspace(string, map[string]string) error { return ErrUnavailable }
func Snapshot(string) ([]File, error)              { return nil, ErrUnavailable }

type unavailableFS struct{}

func (unavailableFS) Open(string) (fs.File, error) { return nil, ErrUnavailable }
