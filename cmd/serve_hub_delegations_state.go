package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/samsaffron/term-llm/internal/hub"
	"github.com/samsaffron/term-llm/internal/restart"
)

var (
	hubDelegationCapRefreshBudget          = 3 * time.Second
	hubDelegationCapRefreshTimeout         = 2 * time.Second
	hubDelegationCapRefreshParallelism     = 8
	hubDelegationCapRefreshErrorStaleAfter = 24 * time.Hour

	errDelegationTargetNotRegistered = errors.New("target node is not present in the hub registry")
)

// checkDelegationCaps enforces hub-wide, per-origin, and per-target in-flight
// limits. Ledger statuses only advance when polled, so before rejecting it
// refreshes active records once on a short best-effort path and recounts.
func (s *hubServer) checkDelegationCaps(ctx context.Context, origin, target hub.Node) error {
	over := func() (error, bool) {
		total, byOrigin, byTarget, err := s.delegations.ActiveCounts()
		if err != nil {
			return err, false
		}
		if byTarget[target.ID] >= target.DelegationMaxInFlight() {
			return fmt.Errorf("node %q already has %d delegations in flight (max %d)", target.ID, byTarget[target.ID], target.DelegationMaxInFlight()), true
		}
		if byOrigin[origin.ID] >= hubDelegationOriginMaxInFlight {
			return fmt.Errorf("node %q already originates %d delegations in flight (max %d)", origin.ID, byOrigin[origin.ID], hubDelegationOriginMaxInFlight), true
		}
		if total >= hubDelegationHubMaxInFlight {
			return fmt.Errorf("hub already has %d delegations in flight (max %d)", total, hubDelegationHubMaxInFlight), true
		}
		return nil, false
	}
	err, capped := over()
	if err == nil || !capped {
		return err
	}
	s.refreshActiveDelegations(ctx, origin.ID, target.ID)
	err, _ = over()
	return err
}

// refreshActiveDelegations re-polls active records in priority order so stale
// entries can stop pinning in-flight caps, but keeps the cap-recovery path
// bounded with a small worker pool and short child deadlines.
func (s *hubServer) refreshActiveDelegations(ctx context.Context, originID, targetID string) {
	ctx, release, reloadErr := restart.Default.Activity(ctx)
	if reloadErr != nil {
		return
	}
	defer release()
	records, err := s.delegations.List()
	if err != nil {
		return
	}
	active := prioritizedActiveDelegations(records, originID, targetID)
	if len(active) == 0 {
		return
	}
	refreshCtx := ctx
	cancel := func() {}
	if hubDelegationCapRefreshBudget > 0 {
		refreshCtx, cancel = context.WithTimeout(ctx, hubDelegationCapRefreshBudget)
	}
	defer cancel()

	workers := hubDelegationCapRefreshParallelism
	if workers <= 0 {
		workers = 1
	}
	if workers > len(active) {
		workers = len(active)
	}

	jobs := make(chan hub.Delegation)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for d := range jobs {
				if refreshCtx.Err() != nil {
					return
				}
				reqCtx := refreshCtx
				reqCancel := func() {}
				if hubDelegationCapRefreshTimeout > 0 {
					reqCtx, reqCancel = context.WithTimeout(refreshCtx, hubDelegationCapRefreshTimeout)
				}
				err := s.refreshDelegation(reqCtx, &d)
				reqCancel()
				if err != nil {
					s.maybeTerminalizeDelegationRefreshError(d, err)
				}
			}
		}()
	}

outer:
	for _, d := range active {
		select {
		case <-refreshCtx.Done():
			break outer
		case jobs <- d:
		}
	}
	close(jobs)
	wg.Wait()
}

func prioritizedActiveDelegations(records []hub.Delegation, originID, targetID string) []hub.Delegation {
	target := make([]hub.Delegation, 0, len(records))
	origin := make([]hub.Delegation, 0, len(records))
	other := make([]hub.Delegation, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		d := records[i]
		if hub.DelegationStatusTerminal(d.Status) {
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
	active := make([]hub.Delegation, 0, len(target)+len(origin)+len(other))
	active = append(active, target...)
	active = append(active, origin...)
	active = append(active, other...)
	return active
}

func (s *hubServer) maybeTerminalizeDelegationRefreshError(d hub.Delegation, err error) {
	if err == nil || d.ID == "" || hub.DelegationStatusTerminal(d.Status) {
		return
	}
	if !delegationRefreshErrorTerminal(d, err) {
		return
	}
	_, _ = s.delegations.Update(d.ID, func(rec *hub.Delegation) {
		if hub.DelegationStatusTerminal(rec.Status) {
			return
		}
		rec.Status = hub.DelegationStatusError
		rec.Error = err.Error()
	})
}

func delegationRefreshErrorTerminal(d hub.Delegation, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errDelegationTargetNotRegistered) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if hubDelegationCapRefreshErrorStaleAfter <= 0 || d.UpdatedAt.IsZero() {
		return false
	}
	return time.Since(d.UpdatedAt) >= hubDelegationCapRefreshErrorStaleAfter
}

// refreshDelegation polls the target node for the delegation's latest run and
// folds any status change back into the ledger and *d.
func (s *hubServer) refreshDelegation(ctx context.Context, d *hub.Delegation) error {
	if hub.DelegationStatusTerminal(d.Status) || d.JobID == "" {
		return nil
	}
	target, ok := s.registry.Lookup(d.TargetNode)
	if !ok {
		return fmt.Errorf("target node %q: %w", d.TargetNode, errDelegationTargetNotRegistered)
	}
	var run hubNodeJobsRun
	if d.RunID != "" {
		// Poll the exact run the delegation triggered: the job's newest run
		// could be a different (e.g. manually re-triggered) one.
		if err := s.doNodeJSON(ctx, target, http.MethodGet,
			"/v2/runs/"+url.PathEscape(d.RunID), nil, &run); err != nil {
			return fmt.Errorf("poll node %q: %w", target.ID, err)
		}
	} else {
		var runs struct {
			Data []hubNodeJobsRun `json:"data"`
		}
		if err := s.doNodeJSON(ctx, target, http.MethodGet,
			"/v2/runs?limit=1&offset=0&job_id="+url.QueryEscape(d.JobID), nil, &runs); err != nil {
			return fmt.Errorf("poll node %q: %w", target.ID, err)
		}
		if len(runs.Data) == 0 {
			return nil
		}
		run = runs.Data[0]
	}
	status := delegationStatusFromRun(run.Status)
	if status == d.Status && run.ID == d.RunID {
		return nil
	}
	updated, err := s.delegations.Update(d.ID, func(rec *hub.Delegation) {
		rec.Status = status
		if run.ID != "" {
			rec.RunID = run.ID
		}
		rec.Error = ""
		if hub.DelegationStatusTerminal(status) {
			rec.Response = run.Response
			rec.Error = run.Error
		}
	})
	if err != nil {
		return err
	}
	*d = updated
	return nil
}

// delegationStatusFromRun maps a jobs-v2 run status onto a delegation status.
func delegationStatusFromRun(runStatus string) string {
	switch runStatus {
	case "succeeded":
		return hub.DelegationStatusSucceeded
	case "failed", "skipped":
		return hub.DelegationStatusFailed
	case "cancelled":
		return hub.DelegationStatusCancelled
	case "cancel_requested":
		return hub.DelegationStatusCancelRequested
	case "timed_out":
		return hub.DelegationStatusTimedOut
	default:
		return hub.DelegationStatusRunning
	}
}

// resolveDelegationCwd defaults to the target's delegation workdir and
// confines an explicit cwd inside it: the workdir is the target operator's
// consent boundary for delegated work. The workdir itself is canonicalized
// (absolute, "."/".." resolved) so prefix containment cannot be confused by
// an un-normalized configured path.
func resolveDelegationCwd(workdir, requested string) (string, error) {
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

// hubDelegationInstructions returns the delegated prompt as the target node's
// user-visible task text. Delegation provenance is trusted metadata carried in
// hub-written job labels and tool context; it should not pollute the target
// session transcript or change what the origin asked the target to do.
func hubDelegationInstructions(_ hub.Delegation, prompt string) string {
	return prompt
}
