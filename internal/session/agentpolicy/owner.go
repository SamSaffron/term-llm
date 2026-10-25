package agentpolicy

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// OwnerTerminated proves death only for a locally named process. On Linux the
// PID start time must also differ, preventing PID reuse from authorizing work.
// The callback supplies the platform-specific ESRCH check for other Unix hosts.
func OwnerTerminated(owner string, pidTerminated func(int) bool) bool {
	host, _ := os.Hostname()
	parts := strings.Split(owner, ":")
	if len(parts) != 3 || parts[0] != host {
		return false
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 0 {
		return false
	}
	if runtime.GOOS != "linux" {
		return pidTerminated != nil && pidTerminated(pid)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(stat), ')')
	if i < 0 {
		return false
	}
	fields := strings.Fields(string(stat)[i+1:])
	return len(fields) > 19 && fields[19] != parts[2]
}
