//go:build js && wasm

package tools

import (
	"io"
	"io/fs"
	"os/exec"

	"github.com/samsaffron/term-llm/internal/memfs"
)

// The browser runtime has no operating-system filesystem. Every file tool call
// goes through this seam into internal/memfs, so the real read/write/edit/glob
// implementations run unchanged against the ephemeral workspace.

type toolfsTempFile = memfs.TempFile

func toolfsOpen(name string) (io.ReadCloser, error) { return memfs.Open(name) }

func toolfsReadFile(name string) ([]byte, error) { return memfs.ReadFile(name) }

func toolfsStat(name string) (fs.FileInfo, error) { return memfs.Stat(name) }

func toolfsLstat(name string) (fs.FileInfo, error) { return memfs.Lstat(name) }

func toolfsMkdirAll(name string, perm fs.FileMode) error { return memfs.MkdirAll(name, perm) }

func toolfsCreateTemp(dir, pattern string) (toolfsTempFile, error) {
	return memfs.CreateTemp(dir, pattern)
}

func toolfsRemove(name string) error { return memfs.Remove(name) }

func toolfsChmod(name string, mode fs.FileMode) error { return memfs.Chmod(name, mode) }

func toolfsRename(oldPath, newPath string) error { return memfs.Rename(oldPath, newPath) }

func toolfsReadlink(name string) (string, error) { return memfs.Readlink(name) }

func toolfsGetwd() (string, error) { return memfs.Getwd() }

func toolfsDirFS(root string) fs.FS { return memfs.DirFS(root) }

func toolfsEvalSymlinks(path string) (string, error) { return memfs.EvalSymlinks(path) }

// WorkspaceFile describes one file in the session workspace.
type WorkspaceFile = memfs.File

// SetWorkspaceFS replaces the in-memory workspace with seed contents.
func SetWorkspaceFS(root string, seed map[string]string) error {
	return memfs.SetWorkspace(root, seed)
}

// WorkspaceSnapshot lists the regular files under root.
func WorkspaceSnapshot(root string) ([]WorkspaceFile, error) { return memfs.Snapshot(root) }

// isTextFileBusy is always false on WebAssembly: there are no subprocesses.
func isTextFileBusy(error) bool { return false }

func platformKillProcessGroup(*exec.Cmd) {}

func platformKillPid(int) {}

// platformFindPidsWithEnv is always empty on WebAssembly: there are no
// subprocesses and no /proc.
func platformFindPidsWithEnv([]byte) []int { return nil }
