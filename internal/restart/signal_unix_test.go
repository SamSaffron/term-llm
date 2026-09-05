//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

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

func TestSignalProcess(t *testing.T) {
	if os.Getenv("TERM_LLM_TEST_RESTART_HELPER") == "1" {
		d := New(func(phase string) { fmt.Println(phase) })
		stop := d.Listen()
		fmt.Println("listening")
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			switch scanner.Text() {
			case "bind":
				unbind := d.Bind(func() { fmt.Println("owner") })
				defer unbind()
				fmt.Println("bound")
			case "finish":
				stop()
				return
			}
		}
		stop()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalProcess$")
	cmd.Env = append(os.Environ(), "TERM_LLM_TEST_RESTART_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	expect := func(want string) {
		t.Helper()
		if !scanner.Scan() || scanner.Text() != want {
			t.Fatalf("want %q, got %q (%v)", want, scanner.Text(), scanner.Err())
		}
	}
	signal := func() {
		t.Helper()
		if err := cmd.Process.Signal(syscall.SIGUSR2); err != nil {
			t.Fatal(err)
		}
	}
	expect("listening")
	signal()
	expect("deferred")
	fmt.Fprintln(stdin, "bind")
	expect("bound")
	signal()
	expect("owner")
	signal()
	expect("owner")
	fmt.Fprintln(stdin, "finish")
	expect("finished")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
