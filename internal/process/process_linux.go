//go:build linux

package process

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var errDead = errors.New("dead process")

func identity(pid int) (string, error) {
	info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return "", err
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return "", errors.New("process has different owner")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", errors.New("invalid proc stat")
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return "", errDead
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(boot)) + ":" + f[19], nil
}

// ELF's Go build ID identifies the actual executable, independent of linker
// version labels. Reading a small note avoids hashing the entire binary at boot.
func BuildID(path string) (string, error) {
	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := f.Section(".note.go.buildid")
	if s == nil || s.Size > 4096 {
		return "", errors.New("missing Go build ID")
	}
	b, err := s.Data()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func registry() (*os.Root, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return nil, err
		}
	}
	path := filepath.Join(base, "term-llm-processes")
	if err := os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	// Reject even a symlink to a private directory. Validate the opened directory
	// too, so replacement between lookup and open cannot weaken ownership checks.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("registry is a symlink")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	info, err = root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		root.Close()
		return nil, errors.New("registry must be owner-private (0700)")
	}
	return root, nil
}

func filename(r Record) string { return strconv.Itoa(r.PID) + ".json" }
func read(root *os.Root, name string) (Record, error) {
	var r Record
	info, err := root.Lstat(name)
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return r, errors.New("record is not a private regular file")
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return r, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return r, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 || info.Size() > 16384 {
		return r, errors.New("invalid record owner/size/mode")
	}
	err = json.NewDecoder(io.LimitReader(f, 16385)).Decode(&r)
	if err == nil && (r.PID <= 0 || filename(r) != name || r.Instance == "" || r.Start == "" || r.BuildID == "" || !filepath.IsAbs(r.Executable)) {
		err = errors.New("invalid record identity")
	}
	return r, err
}

func write(root *os.Root, r Record) error {
	lock, err := root.Open(".")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tmp := "." + r.Instance + ".tmp"
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return root.Rename(tmp, filename(r)) // advisory: deliberately no fsync
}

func (p *Publisher) run(ctx context.Context) {
	defer close(p.done)
	// Short commands should exit without even opening procfs/the registry. This
	// is advisory discovery only; signal handling and durable handoffs don't wait.
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	p.mu.Lock()
	if ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	p.publishing = true
	p.mu.Unlock()
	start, err := identity(os.Getpid())
	if err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	id, err := BuildID("/proc/self/exe")
	if err != nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	root, err := registry()
	if err != nil {
		return
	}
	defer root.Close()
	p.mu.Lock()
	p.record.Start, p.record.BuildID = start, id
	if p.record.Executable == "" {
		p.record.Executable = exe
	}
	p.mu.Unlock()
	defer func() {
		lock, e := root.Open(".")
		if e != nil {
			return
		}
		defer lock.Close()
		if unix.Flock(int(lock.Fd()), unix.LOCK_EX) != nil {
			return
		}
		defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		r, e := read(root, strconv.Itoa(os.Getpid())+".json")
		if e == nil && r.Instance == p.record.Instance {
			_ = root.Remove(filename(r))
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		r := p.record
		p.mu.Unlock()
		_ = write(root, r)
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		}
	}
}

// List never trusts a PID alone. Cleanup is restricted to records whose OS
// identity is dead/mismatched. Publishers and cleaners hold the same directory
// lock, so cleanup cannot delete a newly published replacement record.
func List() ([]Record, error) {
	root, err := registry()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if err = unix.Flock(int(dir.Fd()), unix.LOCK_EX); err != nil {
		return nil, err
	}
	defer unix.Flock(int(dir.Fd()), unix.LOCK_UN)
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	result := make([]Record, 0)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		r, err := read(root, e.Name())
		if err != nil {
			continue
		}
		start, err := identity(r.PID)
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errDead) {
			continue
		}
		if err != nil || start != r.Start {
			// Lock also coordinates replacement, not only other cleaners.
			_ = root.Remove(e.Name())
			continue
		}
		result = append(result, r)
	}
	return result, nil
}

// Restart opens a pidfd before revalidating the snapshot. No numeric kill
// fallback: kernels/sandboxes without pidfd fail closed rather than risk PID reuse.
func Restart(ctx context.Context, r Record, wait bool) (Record, error) {
	return RestartWithProgress(ctx, r, wait, nil)
}

// RestartWithProgress reports advisory lifecycle phases while retaining the same
// identity/build/readiness checks as Restart. The callback runs synchronously and
// must return promptly. Only the return value confirms replacement success.
func RestartWithProgress(ctx context.Context, r Record, wait bool, progress func(string)) (Record, error) {
	lastPhase := ""
	report := func(phase string) {
		if progress != nil && phase != lastPhase {
			progress(phase)
			lastPhase = phase
		}
	}
	report("checking")
	if r.PID == os.Getpid() {
		return r, errors.New("refusing to restart self")
	}
	if r.Phase != "ready" {
		return r, fmt.Errorf("%s: %s", r.Phase, r.Detail)
	}
	fd, err := unix.PidfdOpen(r.PID, 0)
	if err != nil {
		return r, fmt.Errorf("pidfd: %w", err)
	}
	defer unix.Close(fd)
	start, err := identity(r.PID)
	if err != nil || start != r.Start {
		return r, errors.New("process identity changed")
	}
	root, err := registry()
	if err != nil {
		return r, err
	}
	defer root.Close()
	live, err := read(root, filename(r))
	if err != nil || live.Instance != r.Instance || live.Start != r.Start || live.Phase != "ready" {
		return r, errors.New("process instance or readiness changed")
	}
	// pidfd protects process identity, not executable identity: exec of an
	// unrelated program keeps the PID/start time and can leave stale metadata.
	loaded, err := BuildID(fmt.Sprintf("/proc/%d/exe", r.PID))
	if err != nil || loaded != live.BuildID {
		return r, errors.New("running executable no longer matches process record")
	}
	expected, err := BuildID(r.Executable)
	if err != nil {
		return r, fmt.Errorf("installed build: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if err = unix.PidfdSendSignal(fd, unix.SIGUSR2, nil, 0); err != nil {
		return r, err
	}
	report("waiting")
	if !wait {
		return r, nil
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return r, fmt.Errorf("%w: %w", ErrWaitStopped, ctx.Err())
		case <-tick.C:
		}
		// pidfd readiness is authoritative for process exit. A transient procfs
		// read failure during replacement is not proof that the target died.
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, 0); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return r, err
		}
		if poll[0].Revents != 0 {
			return r, errors.New("process exited before restart completed")
		}
		start, err = identity(r.PID)
		if err != nil {
			continue
		}
		if start != r.Start {
			return r, errors.New("process identity changed")
		}
		next, e := read(root, filename(r))
		if e != nil {
			continue
		}
		if next.Start != r.Start {
			return r, errors.New("registry identity changed")
		}
		// Ignore stale ready/error metadata from before this signal. Readiness is
		// displayed as verification until the checks below actually succeed.
		if next.Instance != r.Instance || next.Attempt > live.Attempt {
			phase := next.Phase
			if phase == "ready" {
				phase = "verifying"
			}
			report(phase)
		}
		if next.Instance == r.Instance && next.Attempt > live.Attempt && next.LastError != "" {
			return next, errors.New(next.LastError)
		}
		if next.Instance != r.Instance {
			if next.BuildID != expected {
				return next, errors.New("restarted into unexpected build")
			}
			if next.Phase == "ready" {
				if next.Mode != r.Mode {
					return next, errors.New("restarted into unexpected mode")
				}
				return next, nil
			}
		}
		if next.Phase == "failed" || next.Phase == "unsupported" || (next.Instance == r.Instance && next.Phase == "deferred") {
			return next, fmt.Errorf("%s: %s", next.Phase, next.Detail)
		}
	}
}

func selfStart() (string, error) { return identity(os.Getpid()) }
