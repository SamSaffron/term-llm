package gitcommit

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func publishRepoTest(t *testing.T) (*Repository, string) {
	t.Helper()
	r := repoTest(t)
	gitTest(t, r.root, "branch", "-M", "main")
	remote := t.TempDir()
	gitTest(t, remote, "init", "--bare", "-q")
	gitTest(t, r.root, "remote", "add", "origin", remote)
	gitTest(t, r.root, "push", "-q", "origin", "main")
	gitTest(t, r.root, "commit", "--allow-empty", "-qm", "A new commit")
	return r, remote
}

func publishPlanTest(t *testing.T, r *Repository, pr bool) PublishPlan {
	t.Helper()
	plan, err := r.PublishPlan(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func fakePublishGH(t *testing.T, list, create string, fail bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$@" >> "$PUBLISH_GH_LOG"
case "$1 $2" in
 'repo view') printf '%s\n' '{"url":"https://github.com/test/repo","defaultBranchRef":{"name":"main"}}' ;;
 'pr list') printf '%s\n' "$PUBLISH_GH_LIST" ;;
 'pr create') if [ "$PUBLISH_GH_FAIL" = 1 ]; then echo 'authentication failed' >&2; exit 1; fi; printf '%s\n' "$PUBLISH_GH_CREATE" ;;
 *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PUBLISH_GH_LOG", log)
	t.Setenv("PUBLISH_GH_LIST", list)
	t.Setenv("PUBLISH_GH_CREATE", create)
	if fail {
		t.Setenv("PUBLISH_GH_FAIL", "1")
	}
	return log
}

func TestPublishPushSingleReviewedRef(t *testing.T) {
	r, remote := publishRepoTest(t)
	gitTest(t, r.root, "branch", "unrelated")
	gitTest(t, r.root, "tag", "v1")
	gitTest(t, r.root, "config", "remote.origin.push", "+refs/heads/*:refs/heads/*")
	gitTest(t, r.root, "config", "remote.origin.mirror", "true")
	gitTest(t, r.root, "config", "push.followTags", "true")
	plan := publishPlanTest(t, r, false)
	result, err := r.Publish(context.Background(), "push", PublishRequest{Plan: plan, Branch: plan.Target})
	if err != nil || !result.Pushed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := gitTest(t, remote, "rev-parse", "refs/heads/main"); got != plan.HeadOID {
		t.Fatalf("remote HEAD=%s", got)
	}
	refs := gitTest(t, remote, "for-each-ref", "--format=%(refname)")
	if refs != "refs/heads/main" {
		t.Fatalf("unexpected refs: %s", refs)
	}
}

func TestPublishRejectsStaleAndNonFastForward(t *testing.T) {
	r, remote := publishRepoTest(t)
	plan := publishPlanTest(t, r, false)
	gitTest(t, r.root, "commit", "--allow-empty", "-qm", "newer")
	if _, err := r.Publish(context.Background(), "push", PublishRequest{Plan: plan, Branch: plan.Target}); !IsKind(err, ErrStale) {
		t.Fatalf("stale err=%v", err)
	}
	gitTest(t, r.root, "push", "-q", "origin", "main")
	remoteHead := gitTest(t, remote, "rev-parse", "refs/heads/main")
	gitTest(t, r.root, "reset", "--hard", plan.HeadOID)
	gitTest(t, r.root, "commit", "--allow-empty", "-qm", "divergent")
	plan = publishPlanTest(t, r, false)
	result, err := r.Publish(context.Background(), "push", PublishRequest{Plan: plan, Branch: plan.Target})
	if err == nil || result.Pushed {
		t.Fatalf("non-FF accepted: %+v %v", result, err)
	}
	if got := gitTest(t, remote, "rev-parse", "refs/heads/main"); got != remoteHead {
		t.Fatal("rewrote remote history")
	}
}

func TestPublishPlanRemoteSelectionAndDetached(t *testing.T) {
	r, _ := publishRepoTest(t)
	gitTest(t, r.root, "config", "branch.main.remote", "origin")
	gitTest(t, r.root, "config", "branch.main.merge", "refs/heads/release")
	if p := publishPlanTest(t, r, false); p.Target != "release" {
		t.Fatalf("plan=%+v", p)
	}
	fork := t.TempDir()
	gitTest(t, fork, "init", "--bare", "-q")
	gitTest(t, r.root, "remote", "add", "fork", fork)
	gitTest(t, r.root, "config", "branch.main.pushRemote", "fork")
	if p := publishPlanTest(t, r, false); p.Remote != "fork" || p.Target != "main" || p.URL != fork {
		t.Fatalf("plan=%+v", p)
	}
	gitTest(t, r.root, "remote", "set-url", "--add", "--push", "fork", fork)
	gitTest(t, r.root, "remote", "set-url", "--add", "--push", "fork", t.TempDir())
	if _, err := r.PublishPlan(context.Background(), false); err == nil {
		t.Fatal("accepted multiple push URLs")
	}
	gitTest(t, r.root, "checkout", "--detach", "-q")
	if _, err := r.PublishPlan(context.Background(), false); err == nil {
		t.Fatal("accepted detached HEAD")
	}
}

func TestPublishPRFromMainAndReuse(t *testing.T) {
	log := fakePublishGH(t, `[]`, "https://github.com/test/repo/pull/42", false)
	r, remote := publishRepoTest(t)
	plan := publishPlanTest(t, r, true)
	req := PublishRequest{Plan: plan, Branch: "pr/new-work", Base: "main", Title: "Title; not a shell", Body: "Body\nwith details", Draft: true}
	result, err := r.Publish(context.Background(), "pr", req)
	if err != nil || !result.Pushed || result.PRURL != "https://github.com/test/repo/pull/42" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if gitTest(t, r.root, "symbolic-ref", "--short", "HEAD") != "main" || gitTest(t, r.root, "rev-parse", "HEAD") != plan.HeadOID {
		t.Fatal("changed local branch")
	}
	if gitTest(t, remote, "rev-parse", "refs/heads/pr/new-work") != plan.HeadOID {
		t.Fatal("wrong PR branch commit")
	}
	calls, _ := os.ReadFile(log)
	for _, want := range []string{"--draft", "--repo\nhttps://github.com/test/repo", "--head\npr/new-work", "--base\nmain", "Title; not a shell", "Body\nwith details"} {
		if !strings.Contains(string(calls), want) {
			t.Fatalf("missing %q in %s", want, calls)
		}
	}
	t.Setenv("PUBLISH_GH_LIST", `[{"url":"https://github.com/test/repo/pull/99","isCrossRepository":true},{"url":"https://github.com/test/repo/pull/42","isCrossRepository":false}]`)
	if err := os.WriteFile(log, nil, 0644); err != nil {
		t.Fatal(err)
	}
	result, err = r.Publish(context.Background(), "pr", req)
	if err != nil || !result.Existing || result.PRURL != "https://github.com/test/repo/pull/42" {
		t.Fatalf("reuse=%+v err=%v", result, err)
	}
	calls, _ = os.ReadFile(log)
	if strings.Contains(string(calls), "create") {
		t.Fatalf("created duplicate PR: %s", calls)
	}
}

func TestPublishPRValidationAndPartialFailure(t *testing.T) {
	fakePublishGH(t, `[]`, "", true)
	r, remote := publishRepoTest(t)
	plan := publishPlanTest(t, r, true)
	req := PublishRequest{Plan: plan, Branch: "pr/test", Base: "main", Title: "Title"}
	for _, change := range []func(*PublishRequest){
		func(q *PublishRequest) { q.Branch = "main" },
		func(q *PublishRequest) { q.Branch = "../bad" },
		func(q *PublishRequest) { q.Base = "missing" },
		func(q *PublishRequest) { q.Title = "" },
	} {
		bad := req
		change(&bad)
		if result, err := r.Publish(context.Background(), "pr", bad); err == nil || result.Pushed {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	result, err := r.Publish(context.Background(), "pr", req)
	if !IsKind(err, ErrUncertain) || !result.Pushed || result.PRURL != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if gitTest(t, remote, "rev-parse", "refs/heads/pr/test") != plan.HeadOID {
		t.Fatal("lost pushed branch after gh failure")
	}
	gitTest(t, r.root, "commit", "--allow-empty", "-qm", "next")
	req.Plan = publishPlanTest(t, r, true)
	if _, err := r.Publish(context.Background(), "pr", req); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("replaced existing branch: %v", err)
	}
}

func TestPublishUsesPushURLNotFetchURL(t *testing.T) {
	fakePublishGH(t, `[]`, "https://github.com/test/repo/pull/42", false)
	r, pushRemote := publishRepoTest(t)
	fetchRemote := t.TempDir()
	gitTest(t, fetchRemote, "init", "--bare", "-q")
	gitTest(t, r.root, "remote", "set-url", "origin", fetchRemote)
	gitTest(t, r.root, "remote", "set-url", "--push", "origin", pushRemote)
	plan := publishPlanTest(t, r, true)
	if plan.URL != pushRemote {
		t.Fatalf("plan=%+v", plan)
	}
	result, err := r.Publish(context.Background(), "pr", PublishRequest{Plan: plan, Branch: "pr/push-url", Base: "main", Title: "Title"})
	if err != nil || !result.Pushed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := gitTest(t, fetchRemote, "for-each-ref", "--format=%(refname)"); got != "" {
		t.Fatalf("modified fetch repository: %s", got)
	}
	if got := gitTest(t, pushRemote, "rev-parse", "refs/heads/pr/push-url"); got != plan.HeadOID {
		t.Fatalf("wrong push: %s", got)
	}
}

func TestGitHubRemoteURL(t *testing.T) {
	for input, want := range map[string]string{
		"git@github.com:owner/repo.git":               "https://github.com/owner/repo",
		"ssh://git@ssh.github.com:443/owner/repo.git": "https://github.com/owner/repo",
		"https://github.com/owner/repo.git":           "https://github.com/owner/repo.git",
	} {
		if got := githubRemoteURL(input); got != want {
			t.Errorf("%s: %s != %s", input, got, want)
		}
	}
}
