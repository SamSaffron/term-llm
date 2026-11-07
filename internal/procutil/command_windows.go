//go:build windows

package procutil

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
	createSuspended       = 0x00000004
)

type windowsPlatformCommand struct {
	mu       sync.Mutex
	job      windows.Handle
	cmd      *exec.Cmd
	started  bool
	closed   bool
	closeOne sync.Once
	closeErr error
}

// Injectable seams keep forced job-assignment/resume failure tests local and
// serial; production uses the real Windows calls.
var (
	assignProcessToJobObject = windows.AssignProcessToJobObject
	resumeProcessThread      = resumeProcess
	beforeAssignProcess      func()
)

func configurePlatformCommand(cmd *exec.Cmd, mode CommandMode, owner *Command) (platformCommand, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if mode == DetachedMode {
		cmd.SysProcAttr.CreationFlags |= createNewProcessGroup | createNoWindow
	} else {
		cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	}
	p := &windowsPlatformCommand{cmd: cmd}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}
	p.job = job
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return owner.Close() }
	}
	return p, nil
}

func (p *windowsPlatformCommand) start(*Command) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrCommandClosed
	}
	if p.started {
		p.mu.Unlock()
		return errCommandAlreadyStarted
	}
	p.started = true
	cmd := p.cmd
	cmd.SysProcAttr.CreationFlags |= createSuspended
	err := cmd.Start()
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if beforeAssignProcess != nil {
		beforeAssignProcess()
	}
	return p.assignAndResume()
}

func (p *windowsPlatformCommand) assignAndResume() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cmd := p.cmd
	if cmd == nil || cmd.Process == nil {
		return errors.New("assign process tree: command has no process")
	}
	if p.job == 0 || p.closed {
		// Close may have released the job between process creation and
		// assignment. The direct process must still be terminated.
		_ = cmd.Process.Kill()
		return errors.New("assign process tree: job was closed before assignment")
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = assignProcessToJobObject(p.job, process)
		_ = windows.CloseHandle(process)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("assign process tree to job: %w", err)
	}
	if err := resumeProcessThread(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	return nil
}

func (p *windowsPlatformCommand) signal(syscall.Signal) error { return p.close(nil) }

func (p *windowsPlatformCommand) close(*Command) error {
	p.closeOne.Do(func() {
		p.mu.Lock()
		p.closed = true
		if p.job != 0 {
			_ = windows.TerminateJobObject(p.job, 1)
			_ = windows.CloseHandle(p.job)
			p.job = 0
		}
		p.mu.Unlock()
	})
	return p.closeErr
}

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
