//go:build windows

package procutil

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createSuspended = 0x00000004

type commandTree struct {
	mu            sync.Mutex
	job           windows.Handle
	createErr     error
	readyOnce     sync.Once
	ready         chan struct{}
	killOnce      sync.Once
	startOnce     sync.Once
	startErr      error
	explicitStart atomic.Bool
}

var commandTrees sync.Map // map[*exec.Cmd]*commandTree

func registerCommandTree(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if _, exists := commandTrees.Load(cmd); exists {
		return
	}
	tree := &commandTree{ready: make(chan struct{})}
	disableSuspended := func() {
		if cmd.SysProcAttr != nil {
			cmd.SysProcAttr.CreationFlags &^= createSuspended
		}
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		disableSuspended()
		tree.createErr = fmt.Errorf("create job object: %w", err)
		commandTrees.Store(cmd, tree)
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		disableSuspended()
		tree.createErr = fmt.Errorf("configure job object: %w", err)
		commandTrees.Store(cmd, tree)
		return
	}
	tree.job = job
	commandTrees.Store(cmd, tree)
	go tree.watchSuspendedStart(cmd)
}

// watchSuspendedStart is a compatibility path for callers that use
// exec.Cmd.Start/Run after ConfigureDetachedCommand instead of StartCommand.
// Configure creates the child suspended; this watcher assigns the job and
// resumes it as soon as the process handle is published. StartCommand disables
// the watcher because it performs the same transition synchronously.
func (t *commandTree) watchSuspendedStart(cmd *exec.Cmd) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		if t.explicitStart.Load() {
			return
		}
		if cmd.Process != nil {
			if err := t.completeStart(cmd); err != nil {
				releaseCommandTree(cmd, t)
			}
			return
		}
		select {
		case <-t.ready:
			return
		case <-deadline.C:
			return
		case <-ticker.C:
		}
	}
}

func (t *commandTree) completeStart(cmd *exec.Cmd) error {
	t.startOnce.Do(func() {
		t.startErr = t.assignAndResume(cmd)
		t.readyOnce.Do(func() { close(t.ready) })
	})
	return t.startErr
}

func (t *commandTree) assignAndResume(cmd *exec.Cmd) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		_ = cmd.Process.Kill()
		return errors.New("assign process tree: job was already closed")
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION,
		false,
		uint32(cmd.Process.Pid),
	)
	if err == nil {
		err = windows.AssignProcessToJobObject(t.job, process)
		_ = windows.CloseHandle(process)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("assign process tree to job: %w", err)
	}
	if err := resumeProcess(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return nil
}

func lookupCommandTree(cmd *exec.Cmd) *commandTree {
	if cmd == nil {
		return nil
	}
	value, ok := commandTrees.Load(cmd)
	if !ok {
		return nil
	}
	tree, _ := value.(*commandTree)
	return tree
}

func startCommandTree(cmd *exec.Cmd) error {
	if cmd == nil {
		return errors.New("start command: nil command")
	}
	tree := lookupCommandTree(cmd)
	if tree == nil {
		return errors.New("start command: command was not prepared with procutil.ConfigureDetachedCommand or ConfigureCommandProcessGroup")
	}
	if tree.createErr != nil {
		releaseCommandTree(cmd, tree)
		return tree.createErr
	}

	tree.explicitStart.Store(true)
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= createSuspended
	if err := cmd.Start(); err != nil {
		releaseCommandTree(cmd, tree)
		return err
	}
	if err := tree.completeStart(cmd); err != nil {
		releaseCommandTree(cmd, tree)
		return err
	}
	return nil
}

// resumeProcess resumes the primary thread of a process created suspended.
// The process has exactly one thread before it starts running, so matching by
// owner PID is deterministic.
func resumeProcess(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot threads: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return fmt.Errorf("enumerate threads: %w", err)
	}
	for {
		if entry.OwnerProcessID == uint32(pid) {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return fmt.Errorf("open primary thread: %w", err)
			}
			_, resumeErr := windows.ResumeThread(thread)
			_ = windows.CloseHandle(thread)
			if resumeErr != nil {
				return fmt.Errorf("resume primary thread: %w", resumeErr)
			}
			return nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return errors.New("resume process: primary thread not found")
			}
			return fmt.Errorf("enumerate threads: %w", err)
		}
	}
}

func terminateCommandTree(cmd *exec.Cmd) error {
	tree := lookupCommandTree(cmd)
	if tree == nil {
		return nil
	}
	releaseCommandTree(cmd, tree)
	return nil
}

func releaseCommandTree(cmd *exec.Cmd, tree *commandTree) {
	if tree == nil {
		return
	}
	tree.readyOnce.Do(func() { close(tree.ready) })
	tree.killOnce.Do(func() {
		tree.mu.Lock()
		if tree.job != 0 {
			_ = windows.TerminateJobObject(tree.job, 1)
			_ = windows.CloseHandle(tree.job)
			tree.job = 0
		}
		tree.mu.Unlock()
		commandTrees.Delete(cmd)
	})
}

func outputCommandTree(cmd *exec.Cmd) ([]byte, error) {
	if cmd == nil {
		return nil, errors.New("output command: nil command")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := RunCommand(cmd)
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitErr.Stderr = stderr.Bytes()
	}
	return stdout.Bytes(), err
}
