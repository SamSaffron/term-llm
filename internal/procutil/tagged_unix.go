//go:build !windows && !js

package procutil

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// KillTaggedDescendants terminates every process whose environment contains
// nonceEnvVar=nonce. Descendants that escaped their process group (setsid,
// double-fork, explicit setpgid) still inherit the environment, so this is the
// second cleanup pass after the process-group kill.
func KillTaggedDescendants(nonceEnvVar, nonce string) {
	if nonce == "" {
		return
	}
	needle := []byte(nonceEnvVar + "=" + nonce)
	for attempt := 0; attempt < 3; attempt++ {
		pids := findPidsWithEnv(needle)
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			_ = syscallKill(pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// findPidsWithEnv returns PIDs whose /proc/<pid>/environ contains needle.
// On systems without /proc, returns nil.
func findPidsWithEnv(needle []byte) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	selfPid := os.Getpid()
	var pids []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 || pid == selfPid {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		if bytes.Contains(data, needle) {
			pids = append(pids, pid)
		}
	}
	return pids
}
