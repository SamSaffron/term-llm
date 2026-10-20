//go:build !js && !wasip1

package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// toolfsTempFile is the subset of *os.File the atomic-write paths in the file
// tools use. The js/wasm implementation returns an in-memory equivalent.
type toolfsTempFile interface {
	io.Writer
	io.StringWriter
	Sync() error
	Close() error
	Chmod(fs.FileMode) error
	Name() string
}

// This file keeps the file tools' operating-system access behind a tiny
// platform seam. Native builds delegate straight to os/filepath.

func toolfsOpen(name string) (io.ReadCloser, error) { return os.Open(name) }

func toolfsReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func toolfsStat(name string) (fs.FileInfo, error) { return os.Stat(name) }

func toolfsLstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }

func toolfsMkdirAll(name string, perm fs.FileMode) error { return os.MkdirAll(name, perm) }

func toolfsCreateTemp(dir, pattern string) (toolfsTempFile, error) {
	return os.CreateTemp(dir, pattern)
}

func toolfsRemove(name string) error { return os.Remove(name) }

func toolfsChmod(name string, mode fs.FileMode) error { return os.Chmod(name, mode) }

func toolfsRename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func toolfsReadlink(name string) (string, error) { return os.Readlink(name) }

func toolfsGetwd() (string, error) { return os.Getwd() }

func toolfsDirFS(root string) fs.FS { return os.DirFS(root) }

func toolfsEvalSymlinks(path string) (string, error) { return filepath.EvalSymlinks(path) }

// SetWorkspaceFS is intentionally browser-only. Native tools must never reset
// an arbitrary host directory as a side effect of booting a browser experiment.
func SetWorkspaceFS(root string, seed map[string]string) error {
	return fmt.Errorf("in-memory workspace is only supported in browser WASM")
}

func isTextFileBusy(err error) bool { return errors.Is(err, syscall.ETXTBSY) }

func platformKillProcessGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

func platformKillPid(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }

// platformFindPidsWithEnv returns PIDs whose /proc/<pid>/environ contains
// needle. On systems without /proc it returns nil.
func platformFindPidsWithEnv(needle []byte) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	selfPid := os.Getpid()
	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == selfPid {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		if bytes.Contains(data, needle) {
			pids = append(pids, pid)
		}
	}
	return pids
}
