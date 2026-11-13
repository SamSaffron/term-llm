package procutil

import (
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPrepareToolCommandPreservesEnvironmentAndAddsUniqueCleanupTag(t *testing.T) {
	seen := map[string]bool{}
	for _, customEnv := range []bool{false, true} {
		cmd := exec.Command("not-started")
		if customEnv {
			cmd.Env = []string{"FIXTURE=value"}
		}
		owner, err := PrepareToolCommand(cmd)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		if cmd.WaitDelay != 2*time.Second {
			t.Fatalf("pipe wait delay = %s", cmd.WaitDelay)
		}
		last := cmd.Env[len(cmd.Env)-1]
		nonce, ok := strings.CutPrefix(last, shellNonceEnvVar+"=")
		decoded, err := hex.DecodeString(nonce)
		if !ok || err != nil || len(decoded) != 16 || seen[nonce] {
			t.Fatalf("invalid or reused cleanup tag %q", last)
		}
		seen[nonce] = true
		if customEnv {
			if len(cmd.Env) != 2 || cmd.Env[0] != "FIXTURE=value" {
				t.Fatalf("explicit environment changed: %v", cmd.Env)
			}
		} else if len(cmd.Env) != len(os.Environ())+1 {
			t.Fatal("ambient environment was not inherited")
		}
	}
}

func TestPrepareToolCommandNilFailsWithoutMutation(t *testing.T) {
	if owner, err := PrepareToolCommand(nil); err == nil || owner != nil {
		t.Fatalf("nil preparation = %v, %v", owner, err)
	}
}
