package gitcommit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/procutil"
)

// PublishPlan is a reviewable, single-branch destination. URL is the effective
// push URL, not the fetch URL (which can point at a different repository).
type PublishPlan struct {
	CheckoutID    string `json:"checkout_id"`
	HeadOID       string `json:"head_oid"`
	Branch        string `json:"branch"`
	Remote        string `json:"remote"`
	URL           string `json:"url"`
	Target        string `json:"target"`
	Repository    string `json:"repository,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

type PublishRequest struct {
	Plan   PublishPlan `json:"plan"`
	Branch string      `json:"branch"`
	Base   string      `json:"base,omitempty"`
	Title  string      `json:"title,omitempty"`
	Body   string      `json:"body,omitempty"`
	Draft  bool        `json:"draft,omitempty"`
}

type PublishResult struct {
	Pushed   bool   `json:"pushed"`
	Branch   string `json:"branch"`
	PRURL    string `json:"pr_url,omitempty"`
	Existing bool   `json:"existing,omitempty"`
}

// runPublishCommand bounds both network calls and their subprocesses. No shell,
// browser, terminal prompts, or implicit gh repository selection is used.
func (r *Repository) runPublishCommand(ctx context.Context, program string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = r.root
	cmd.Env = gitEnvironment(map[string]string{"GH_PROMPT_DISABLED": "1", "GH_PAGER": "cat"})
	cmd.WaitDelay = 2 * time.Second
	cleanup, err := procutil.PrepareCommandProcessGroup(cmd)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	out, stderr := &limitBuffer{max: maxCommitOutput}, &limitBuffer{max: maxCommitOutput}
	cmd.Stdout, cmd.Stderr = out, stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, typed(ErrUncertain, "Publishing timed out; check the remote before retrying", ctx.Err())
	}
	if out.truncated || stderr.truncated {
		return nil, typed(ErrUncertain, "Publishing output exceeded the limit; check the remote before retrying", err)
	}
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w: %s", program, err, strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

func (r *Repository) PublishPlan(ctx context.Context, pr bool) (PublishPlan, error) {
	release, err := r.acquire(ctx)
	if err != nil {
		return PublishPlan{}, err
	}
	defer release()
	return r.publishPlan(ctx, pr)
}

func (r *Repository) publishPlan(ctx context.Context, pr bool) (PublishPlan, error) {
	plan := PublishPlan{CheckoutID: r.checkoutID}
	branch, err := r.git(ctx, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return plan, typed(ErrUnsupportedOperation, "Publishing requires a named branch, not detached HEAD", err)
	}
	plan.Branch = strings.TrimSpace(string(branch))
	head, err := r.git(ctx, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return plan, err
	}
	plan.HeadOID = strings.TrimSpace(string(head))
	config := func(key string) string {
		out, _ := r.git(ctx, nil, "config", "--get", key)
		return strings.TrimSpace(string(out))
	}
	tracking := config("branch." + plan.Branch + ".remote")
	plan.Remote = config("branch." + plan.Branch + ".pushRemote")
	if plan.Remote == "" {
		plan.Remote = config("remote.pushDefault")
	}
	if plan.Remote == "" {
		plan.Remote = tracking
	}
	remotes, err := r.git(ctx, nil, "remote")
	if err != nil {
		return plan, err
	}
	names := strings.Fields(string(remotes))
	if plan.Remote == "" {
		for _, name := range names {
			if name == "origin" {
				plan.Remote = name
			}
		}
		if plan.Remote == "" && len(names) == 1 {
			plan.Remote = names[0]
		}
	}
	found := false
	for _, name := range names {
		if name == plan.Remote {
			found = true
		}
	}
	if !found || strings.HasPrefix(plan.Remote, "-") {
		return plan, typed(ErrUnsupportedOperation, "Configure an unambiguous push remote for this branch first", nil)
	}
	urls, err := r.git(ctx, nil, "remote", "get-url", "--push", "--all", plan.Remote)
	if err != nil {
		return plan, err
	}
	destinations := strings.Split(strings.TrimSpace(string(urls)), "\n")
	if len(destinations) != 1 || destinations[0] == "" {
		return plan, typed(ErrUnsupportedOperation, "Publishing requires exactly one push URL", nil)
	}
	plan.URL = destinations[0]
	plan.Target = plan.Branch
	if tracking == plan.Remote {
		merge := config("branch." + plan.Branch + ".merge")
		if strings.HasPrefix(merge, "refs/heads/") {
			plan.Target = strings.TrimPrefix(merge, "refs/heads/")
		}
	}
	if pr {
		out, err := r.runPublishCommand(ctx, "gh", "repo", "view", "--json", "url,defaultBranchRef", "--", githubRemoteURL(plan.URL))
		if err != nil {
			return plan, err
		}
		var repo struct {
			URL              string `json:"url"`
			DefaultBranchRef struct {
				Name string `json:"name"`
			} `json:"defaultBranchRef"`
		}
		if err := json.Unmarshal(out, &repo); err != nil {
			return plan, err
		}
		if !validPRURL(repo.URL) || repo.DefaultBranchRef.Name == "" {
			return plan, fmt.Errorf("gh did not return a repository and default branch")
		}
		plan.Repository, plan.DefaultBranch = repo.URL, repo.DefaultBranchRef.Name
	}
	return plan, nil
}

// gh accepts HTTPS repository URLs, while Git remotes also commonly use SCP or SSH syntax.
func githubRemoteURL(remote string) string {
	if strings.HasPrefix(remote, "git@") && !strings.Contains(remote, "://") {
		if host, path, ok := strings.Cut(strings.TrimPrefix(remote, "git@"), ":"); ok {
			return "https://" + host + "/" + strings.TrimSuffix(path, ".git")
		}
	}
	if u, err := url.Parse(remote); err == nil && u.Scheme == "ssh" {
		host := u.Hostname()
		if host == "ssh.github.com" {
			host = "github.com"
		}
		return "https://" + host + strings.TrimSuffix(u.Path, ".git")
	}
	return remote
}

func validPRURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

func (r *Repository) validPublishBranch(ctx context.Context, branch string) bool {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return false
	}
	_, err := r.git(ctx, nil, "check-ref-format", "refs/heads/"+branch)
	return err == nil
}

// Publish never rewinds or switches the local checkout. A different PR branch
// is created remotely from the reviewed OID, only if absent (or already at that
// OID after a retry). Explicit refspecs bypass push.default and configured
// multi-ref/forced pushes, and normal pushes remain fast-forward-only.
func (r *Repository) Publish(ctx context.Context, kind string, req PublishRequest) (PublishResult, error) {
	result := PublishResult{Branch: req.Branch}
	if kind != "push" && kind != "pr" {
		return result, fmt.Errorf("invalid publish kind")
	}
	release, err := r.acquire(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	plan, err := r.publishPlan(ctx, kind == "pr")
	if err != nil {
		return result, err
	}
	if plan != req.Plan {
		return result, typed(ErrStale, "The checkout, commit, or destination changed; review the publishing destination again", nil)
	}
	if !r.validPublishBranch(ctx, req.Branch) {
		return result, fmt.Errorf("invalid destination branch")
	}
	if kind == "push" && req.Branch != plan.Target {
		return result, fmt.Errorf("push destination must match the reviewed branch")
	}
	if kind == "pr" {
		if !r.validPublishBranch(ctx, req.Base) || req.Base == req.Branch {
			return result, fmt.Errorf("choose a PR branch different from the base branch")
		}
		if req.Branch == plan.DefaultBranch {
			return result, fmt.Errorf("use a new PR branch instead of publishing to the default branch")
		}
		if strings.TrimSpace(req.Title) == "" {
			return result, fmt.Errorf("PR title is required")
		}
		base, err := r.runPublishCommand(ctx, "git", "ls-remote", "--heads", "--", plan.URL, "refs/heads/"+req.Base)
		if err != nil {
			return result, err
		}
		fields := strings.Fields(string(base))
		if len(fields) == 0 {
			return result, fmt.Errorf("PR base branch does not exist on the push remote")
		}
		if fields[0] == plan.HeadOID {
			return result, fmt.Errorf("this commit is already the tip of the base branch; there is nothing to propose")
		}
	}
	newBranchLease := ""
	if req.Branch != plan.Target {
		out, err := r.runPublishCommand(ctx, "git", "ls-remote", "--heads", "--", plan.URL, "refs/heads/"+req.Branch)
		if err != nil {
			return result, err
		}
		expected := ""
		if fields := strings.Fields(string(out)); len(fields) > 0 {
			if fields[0] != plan.HeadOID {
				return result, fmt.Errorf("destination branch already exists; choose a new PR branch")
			}
			expected = fields[0]
		}
		// This lease only permits creation or a no-op at the same reviewed OID.
		// It prevents racing another publisher into an existing new branch.
		newBranchLease = "--force-with-lease=refs/heads/" + req.Branch + ":" + expected
	}
	// Recheck HEAD after network discovery. Always push the reviewed object, never
	// HEAD or a local branch name that another process could advance meanwhile.
	head, err := r.git(ctx, nil, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return result, err
	}
	branch, branchErr := r.git(ctx, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if strings.TrimSpace(string(head)) != plan.HeadOID || branchErr != nil || strings.TrimSpace(string(branch)) != plan.Branch {
		return result, typed(ErrStale, "HEAD or branch changed before publishing", branchErr)
	}
	args := []string{"-c", "remote." + plan.Remote + ".mirror=false", "push", "--porcelain", "--no-force", "--no-follow-tags", "--recurse-submodules=no"}
	if newBranchLease != "" {
		args = append(args, newBranchLease)
	}
	args = append(args, "--", plan.URL, plan.HeadOID+":refs/heads/"+req.Branch)
	if _, err := r.runPublishCommand(ctx, "git", args...); err != nil {
		// A disconnected receive-pack may have updated the remote before the
		// client saw its acknowledgement. Never report a definite rollback.
		return result, typed(ErrUncertain, "Push could not be confirmed; check the remote before retrying. "+err.Error(), err)
	}
	result.Pushed = true
	if kind == "push" {
		return result, nil
	}
	// Scope both lookups and creation to the exact push repository and head/base.
	out, err := r.runPublishCommand(ctx, "gh", "pr", "list", "--repo", plan.Repository, "--head", req.Branch, "--base", req.Base, "--state", "open", "--json", "url,isCrossRepository")
	if err != nil {
		return result, fmt.Errorf("branch pushed, but PR lookup failed: %w", err)
	}
	var prs []struct {
		URL               string `json:"url"`
		IsCrossRepository bool   `json:"isCrossRepository"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return result, fmt.Errorf("branch pushed, but PR lookup returned invalid JSON: %w", err)
	}
	for _, pr := range prs {
		if pr.IsCrossRepository {
			continue
		}
		if !validPRURL(pr.URL) {
			return result, fmt.Errorf("gh returned an invalid PR URL")
		}
		result.PRURL, result.Existing = pr.URL, true
		return result, nil
	}
	args = []string{"pr", "create", "--repo", plan.Repository, "--head", req.Branch, "--base", req.Base, "--title", req.Title, "--body", req.Body}
	if req.Draft {
		args = append(args, "--draft")
	}
	out, err = r.runPublishCommand(ctx, "gh", args...)
	if err != nil {
		return result, typed(ErrUncertain, "Branch pushed, but PR creation could not be confirmed. Check GitHub before retrying. "+err.Error(), err)
	}
	result.PRURL = strings.TrimSpace(string(out))
	if !validPRURL(result.PRURL) {
		result.PRURL = ""
		return result, typed(ErrUncertain, "Branch pushed, but gh returned no valid PR URL; check GitHub before retrying", nil)
	}
	return result, nil
}
