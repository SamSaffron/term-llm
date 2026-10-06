package tools

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func TestAgentOwnerTerminationRequiresProcessProof(t *testing.T) {
	host, _ := os.Hostname()
	if ownerTerminated("different-host:1:1") {
		t.Fatal("foreign host cannot be declared dead")
	}
	if runtime.GOOS != "linux" {
		if runtime.GOOS != "windows" {
			if !ownerTerminated(fmt.Sprintf("%s:%d:unknown", host, 1<<30)) {
				t.Fatal("missing Unix PID should be interrupted")
			}
			if ownerTerminated(fmt.Sprintf("%s:%d:unknown", host, os.Getpid())) {
				t.Fatal("live Unix PID must not be interrupted even with unknown start time")
			}
		}
		return
	}
	if !ownerTerminated(fmt.Sprintf("%s:%d:wrong-start", host, os.Getpid())) {
		t.Fatal("reused process ID must be interrupted")
	}
	if ownerTerminated(processAgentOwner()) {
		t.Fatal("live owner is not terminated")
	}
}
