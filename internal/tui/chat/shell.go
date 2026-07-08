package chat

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
)

func (m *Model) cmdShell(args []string) (tea.Model, tea.Cmd) {
	m.setTextareaValue("")
	if m.streaming {
		return m.showFooterWarning("Cannot open a shell while a response is streaming.")
	}
	opts, err := parseShellArgs(args)
	if err != nil {
		return m.showFooterError(err.Error())
	}

	cmd, dir, err := m.interactiveShellCommand(opts.NoRC)
	if err != nil {
		return m.showFooterError(err.Error())
	}

	m.clearFooterMessage()
	m.pausedForExternalUI = true
	if m.completions != nil {
		m.completions.Hide()
	}
	m.selection = Selection{}

	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return shellExitMessage(dir, err)
	})
}

type shellOptions struct {
	NoRC bool
}

func parseShellArgs(args []string) (shellOptions, error) {
	var opts shellOptions
	for _, arg := range args {
		switch arg {
		case "--no-rc":
			opts.NoRC = true
		default:
			return opts, fmt.Errorf("unknown /shell option %q; usage: /shell [--no-rc]", arg)
		}
	}
	return opts, nil
}

func (m *Model) interactiveShellCommand(noRC bool) (*exec.Cmd, string, error) {
	dir, err := m.interactiveShellDir()
	if err != nil {
		return nil, "", err
	}
	shellPath := interactiveShellPath()
	shellArgs, err := interactiveShellArgs(shellPath, noRC)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.Command(shellPath, shellArgs...)
	cmd.Dir = dir
	cmd.Env = interactiveShellEnv(os.Environ(), dir, m.boundWorktreeForShellEnv(), noRC)
	return cmd, dir, nil
}

func (m *Model) interactiveShellDir() (string, error) {
	dir := ""
	if m != nil {
		if m.sess != nil {
			dir = strings.TrimSpace(m.sess.WorktreeDir)
		}
		if dir == "" && m.toolMgr != nil {
			dir = strings.TrimSpace(m.toolMgr.BaseDir())
		}
		if dir == "" && m.sess != nil {
			dir = strings.TrimSpace(m.sess.CWD)
		}
	}
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve shell working directory: %w", err)
		}
		dir = cwd
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve shell working directory: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("shell working directory is not accessible: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("shell working path is not a directory: %s", abs)
	}
	return abs, nil
}

func (m *Model) boundWorktreeForShellEnv() string {
	if m == nil || m.sess == nil {
		return ""
	}
	return strings.TrimSpace(m.sess.WorktreeDir)
}

func interactiveShellPath() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		return shell
	}
	return "sh"
}

func interactiveShellArgs(shellPath string, noRC bool) ([]string, error) {
	if !noRC {
		return nil, nil
	}
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(shellPath)), ".exe")
	switch name {
	case "zsh":
		return []string{"-f"}, nil
	case "bash":
		return []string{"--noprofile", "--norc"}, nil
	case "fish":
		return []string{"--no-config"}, nil
	case "csh", "tcsh":
		return []string{"-f"}, nil
	case "nu", "nushell":
		return []string{"--no-config-file"}, nil
	case "sh", "dash", "ash", "ksh", "mksh", "pdksh":
		// POSIX-ish shells commonly use ENV for interactive startup. There is
		// no portable no-rc flag, so interactiveShellEnv removes ENV below.
		return nil, nil
	default:
		return nil, fmt.Errorf("/shell --no-rc is not supported for shell %q", shellPath)
	}
}

func interactiveShellEnv(environ []string, dir string, worktreeDir string, noRC bool) []string {
	out := make([]string, 0, len(environ)+3)
	for _, entry := range environ {
		if shouldDropInteractiveShellEnv(entry, noRC) {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "PWD="+dir, "TERM_LLM_BASE_DIR="+dir)
	if worktreeDir = strings.TrimSpace(worktreeDir); worktreeDir != "" {
		out = append(out, "TERM_LLM_WORKTREE_DIR="+worktreeDir)
	}
	return out
}

func shouldDropInteractiveShellEnv(entry string, noRC bool) bool {
	key, _, _ := strings.Cut(entry, "=")
	switch key {
	case "PWD", "TERM_LLM_BASE_DIR", "TERM_LLM_WORKTREE_DIR":
		return true
	case "ENV", "BASH_ENV", "ZDOTDIR":
		return noRC
	default:
		return false
	}
}

func shellCompletionItems(query string) ([]Command, bool) {
	query = strings.TrimPrefix(query, "/")
	if strings.TrimSpace(query) == "" {
		return nil, false
	}
	trailingSpace := strings.HasSuffix(query, " ")
	parts := strings.Fields(query)
	if len(parts) == 0 {
		return nil, false
	}
	cmd := strings.ToLower(parts[0])
	if cmd != "shell" && cmd != "sh" {
		return nil, false
	}
	if len(parts) == 1 && !trailingSpace {
		return nil, false
	}

	prefixParts := append([]string{}, parts...)
	partial := ""
	if len(parts) > 1 && !trailingSpace {
		prefixParts = append([]string{}, parts[:len(parts)-1]...)
		partial = parts[len(parts)-1]
	}
	if partial != "" && !strings.HasPrefix(partial, "-") {
		return nil, true
	}
	for _, part := range parts[1:] {
		if part == "--no-rc" {
			return nil, true
		}
	}
	if partial != "" && !strings.HasPrefix("--no-rc", strings.ToLower(partial)) {
		return nil, true
	}
	return []Command{{
		Name:        strings.Join(append(prefixParts, "--no-rc"), " "),
		Description: "Start shell without user rc/config files",
	}}, true
}

func shellExitMessage(dir string, err error) shellExitedMsg {
	msg := shellExitedMsg{dir: dir}
	if err == nil {
		return msg
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		msg.exitCode = exitErr.ExitCode()
		return msg
	}
	msg.err = err
	return msg
}
