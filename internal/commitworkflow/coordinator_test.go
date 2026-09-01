package commitworkflow

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/gitcommit"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

type fakeRunner struct {
	output  string
	err     error
	request runpkg.ChildRunRequest
}

func (f *fakeRunner) ResolveChildAgent(name string) (runpkg.ChildAgentMetadata, error) {
	return runpkg.ChildAgentMetadata{Name: name, Source: "test"}, nil
}
func (f *fakeRunner) RunChild(_ context.Context, request runpkg.ChildRunRequest, _ runpkg.ChildRunEventCallback) (runpkg.ChildRunResult, error) {
	f.request = request
	return runpkg.ChildRunResult{RunID: request.RunID, Output: f.output, Provider: "test", Model: "model", StartedAt: time.Now(), CompletedAt: time.Now()}, f.err
}
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
func setup(t *testing.T) (string, gitcommit.RepositoryState) {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitcommit.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := repo.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return dir, state
}

func TestPlanScopeUsesHostOverlayAndValidatesPaths(t *testing.T) {
	dir, state := setup(t)
	payload, _ := json.Marshal(ScopeProposal{Mode: ScopeSelected, IncludePaths: []string{"base.txt"}, Summary: "base only"})
	runner := &fakeRunner{output: string(payload)}
	proposal, meta, err := New().PlanScope(context.Background(), Request{CheckoutDir: dir, AgentName: "custom", Intent: "only base", ExpectedFingerprint: state.Fingerprint, ExpectedStatusToken: state.StatusToken, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Mode != ScopeSelected || meta.AgentSource != "test" {
		t.Fatalf("proposal=%+v meta=%+v", proposal, meta)
	}
	if runner.request.Kind != runpkg.ChildRunCommitScope || !runner.request.SkipOnComplete || !runner.request.DisableTools || runner.request.OutputTool.Name != "propose_commit_scope" || runner.request.MaxTurnsOverride == 0 {
		t.Fatalf("request=%+v", runner.request)
	}
	runner.output = `{"mode":"selected","include_paths":["invented.txt"],"summary":"bad"}`
	_, _, err = New().PlanScope(context.Background(), Request{CheckoutDir: dir, Intent: "only invented", ExpectedFingerprint: state.Fingerprint, ExpectedStatusToken: state.StatusToken, Runner: runner})
	if err == nil {
		t.Fatal("hallucinated path accepted")
	}
}

func TestDraftMessageIsStagedOnlyAndEmptyFails(t *testing.T) {
	dir, state := setup(t)
	repo, _ := gitcommit.Open(context.Background(), dir)
	staged, err := repo.Stage(context.Background(), gitcommit.StageRequest{Mode: gitcommit.StageAll, StatusToken: state.StatusToken}, state.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{output: "Change base behavior\n\nExplain it."}
	message, _, err := New().DraftMessage(context.Background(), Request{CheckoutDir: dir, ExpectedFingerprint: staged.Fingerprint, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if message != runner.output || runner.request.Kind != runpkg.ChildRunCommitDraft || runner.request.OutputTool.Param != "message" || !runner.request.SkipOnComplete {
		t.Fatalf("message=%q request=%+v", message, runner.request)
	}
	runner.output = " \n"
	if _, _, err := New().DraftMessage(context.Background(), Request{CheckoutDir: dir, ExpectedFingerprint: staged.Fingerprint, Runner: runner}); err == nil {
		t.Fatal("empty message accepted")
	}
}
