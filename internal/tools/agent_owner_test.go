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
		return
	}
	if !ownerTerminated(fmt.Sprintf("%s:%d:wrong-start", host, os.Getpid())) {
		t.Fatal("reused process ID must be interrupted")
	}
	if ownerTerminated(processAgentOwner()) {
		t.Fatal("live owner is not terminated")
	}
}
