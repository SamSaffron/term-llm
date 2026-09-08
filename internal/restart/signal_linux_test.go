//go:build linux

package restart

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// All real signals in these tests target a child created by this test. The
// helper deliberately waits before installing the successor's Go listener.
func TestReloadSignalHelper(t *testing.T) {
	stage := os.Getenv("TERM_LLM_RELOAD_SIGNAL_HELPER")
	if stage == "" {
		return
	}
	c := &Coordinator{}
	if stage == "boot" {
		fmt.Println("boot")
		time.Sleep(150 * time.Millisecond)
		stop := c.Listen()
		defer stop()
		fmt.Println("ready")
		select {}
	}
	stop := c.Listen()
	defer stop()
	c.Observe = func(s Status) {
		if stage == "failure" && s.Phase == "ready" && s.Error != "" {
			fmt.Printf("failed %d\n", s.Attempt)
		}
	}
	unbind, err := c.Bind(context.Background(), func(context.Context) error {
		if stage == "failure" {
			err := c.Exec(func() error { return syscall.Exec("/no-such-reload-test-binary", os.Args, os.Environ()) })
			return err
		}
		return c.Exec(func() error {
			_ = os.Setenv("TERM_LLM_RELOAD_SIGNAL_HELPER", "boot")
			return syscall.Exec(os.Args[0], os.Args, os.Environ())
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unbind()
	fmt.Println("listening")
	select {}
}

func signalChild(t *testing.T, stage string) (*exec.Cmd, <-chan string) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestReloadSignalHelper$")
	child.Env = append(os.Environ(), "TERM_LLM_RELOAD_SIGNAL_HELPER="+stage)
	child.Stderr = os.Stderr
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	return child, lines
}

func expectSignalLine(t *testing.T, lines <-chan string, expected string) {
	t.Helper()
	select {
	case line, ok := <-lines:
		if !ok || line != expected {
			t.Fatalf("got %q (open=%v), want %q", line, ok, expected)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("waiting for %s", expected)
	}
}

func TestSignalsDuringReplacementBootAreNonfatal(t *testing.T) {
	child, lines := signalChild(t, "exec")
	expectSignalLine(t, lines, "listening")
	if err := child.Process.Signal(syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	expectSignalLine(t, lines, "boot")
	for range 10 {
		if err := child.Process.Signal(syscall.SIGUSR2); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	expectSignalLine(t, lines, "ready")
}

func TestFailedExecRestoresSignalListener(t *testing.T) {
	child, lines := signalChild(t, "failure")
	expectSignalLine(t, lines, "listening")
	if err := child.Process.Signal(syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	expectSignalLine(t, lines, "failed 1")
	if err := child.Process.Signal(syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	expectSignalLine(t, lines, "failed 2")
}
