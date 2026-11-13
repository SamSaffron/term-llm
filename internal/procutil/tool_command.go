package procutil

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"time"
)

// shellNonceEnvVar is the environment variable name used to tag every process
// spawned by a tool command so descendants that escape the process group (via
// setsid, daemonisation, etc.) can still be reliably reaped on cleanup.
const shellNonceEnvVar = "TERMLLM_SHELL_NONCE"

// PrepareToolCommand applies the non-interactive tool command policy: detached
// command ownership, a bounded pipe wait, and nonce-tagged cleanup of descendants
// that escape the process group. Close the returned owner on every execution path.
func PrepareToolCommand(cmd *exec.Cmd) (*Command, error) {
	owned, err := PrepareCommand(cmd)
	if err != nil {
		return nil, err
	}
	configureToolWait(cmd)
	nonce := tagCommandWithNonce(cmd)
	owned.SetCloseHook(func() {
		// Tools must leave the world clean: after the command returns (success,
		// failure, timeout, or cancellation) reap every descendant it spawned.
		// The owner has already killed the process group; this second pass walks
		// /proc for descendants that escaped it via setsid/daemonisation.
		KillTaggedDescendants(shellNonceEnvVar, nonce)
	})
	return owned, nil
}

// tagCommandWithNonce adds a unique TERMLLM_SHELL_NONCE=<hex> entry to cmd.Env
// so descendants can be identified later by scanning /proc. Returns the nonce
// value, or "" if it could not be generated (in which case tagging is silently
// skipped — the pgroup kill still runs).
func tagCommandWithNonce(cmd *exec.Cmd) string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return ""
	}
	nonce := hex.EncodeToString(buf[:])
	entry := shellNonceEnvVar + "=" + nonce
	if cmd.Env == nil {
		cmd.Env = append(os.Environ(), entry)
	} else {
		cmd.Env = append(cmd.Env, entry)
	}
	return nonce
}

func configureToolWait(cmd *exec.Cmd) {
	// WaitDelay unblocks cmd.Wait() when the main process has exited but a
	// backgrounded descendant still holds the stdout/stderr pipe open. After
	// the delay Go closes the pipes and returns from Wait, preventing the
	// tool from hanging indefinitely.
	cmd.WaitDelay = 2 * time.Second
}
