package hub

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	corehub "github.com/samsaffron/term-llm/internal/hub"
)

func PrioritizedActiveDelegations(records []corehub.Delegation, originID, targetID string) []corehub.Delegation {
	target := make([]corehub.Delegation, 0, len(records))
	origin := make([]corehub.Delegation, 0, len(records))
	other := make([]corehub.Delegation, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		d := records[i]
		if corehub.DelegationStatusTerminal(d.Status) {
			continue
		}
		switch {
		case targetID != "" && d.TargetNode == targetID:
			target = append(target, d)
		case originID != "" && d.OriginNode == originID:
			origin = append(origin, d)
		default:
			other = append(other, d)
		}
	}
	active := make([]corehub.Delegation, 0, len(target)+len(origin)+len(other))
	active = append(active, target...)
	active = append(active, origin...)
	active = append(active, other...)
	return active
}

// DelegationStatusFromRun maps a jobs-v2 run status onto a delegation status.
func DelegationStatusFromRun(runStatus string) string {
	switch runStatus {
	case "succeeded":
		return corehub.DelegationStatusSucceeded
	case "failed", "skipped":
		return corehub.DelegationStatusFailed
	case "cancelled":
		return corehub.DelegationStatusCancelled
	case "cancel_requested":
		return corehub.DelegationStatusCancelRequested
	case "timed_out":
		return corehub.DelegationStatusTimedOut
	default:
		return corehub.DelegationStatusRunning
	}
}

// ResolveDelegationCwd defaults to the target's delegation workdir and
// confines an explicit cwd inside it: the workdir is the target operator's
// consent boundary for delegated work. The workdir itself is canonicalized
// (absolute, "."/".." resolved) so prefix containment cannot be confused by
// an un-normalized configured path.
func ResolveDelegationCwd(workdir, requested string) (string, error) {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return "", errors.New("target node has no delegation workdir")
	}
	if hasControlRune(workdir) {
		return "", fmt.Errorf("target node's delegation workdir contains control characters")
	}
	if hasControlRune(requested) {
		return "", fmt.Errorf("cwd contains control characters")
	}
	if !strings.HasPrefix(workdir, "/") {
		return "", fmt.Errorf("target node's delegation workdir %q is not an absolute path", workdir)
	}
	workdir = pathClean(workdir)
	if workdir == "" {
		return "", errors.New("target node's delegation workdir resolves to the filesystem root; refusing")
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return workdir, nil
	}
	if !strings.HasPrefix(requested, "/") {
		requested = workdir + "/" + requested
	}
	cwd := pathClean(requested)
	if cwd == workdir || strings.HasPrefix(cwd, workdir+"/") {
		return cwd, nil
	}
	return "", fmt.Errorf("cwd %q is outside the target node's delegation workdir %q", requested, workdir)
}

func hasControlRune(s string) bool {
	for _, r := range s {
		if r == 0 || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// pathClean is filepath.Clean with forward slashes: delegation workdirs are
// paths on the (POSIX) target node, not on the hub host.
func pathClean(p string) string {
	cleaned := strings.Builder{}
	segments := strings.Split(p, "/")
	stack := make([]string, 0, len(segments))
	for _, seg := range segments {
		switch seg {
		case "", ".":
		case "..":
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			stack = append(stack, seg)
		}
	}
	cleaned.WriteString("/" + strings.Join(stack, "/"))
	return strings.TrimRight(cleaned.String(), "/")
}
