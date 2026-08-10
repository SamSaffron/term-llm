package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

type failingWorkspaceTrustStore struct {
	checkErr    error
	rememberErr error
}

func (s failingWorkspaceTrustStore) IsTrusted(context.Context, string) (bool, error) {
	return false, s.checkErr
}

func (s failingWorkspaceTrustStore) Remember(string) error {
	return s.rememberErr
}

type legacyYoloDeleteFailureStore struct {
	session.Store
	grants []session.WorkspaceGrant
}

func (s *legacyYoloDeleteFailureStore) ListWorkspaceGrants(context.Context, string) ([]session.WorkspaceGrant, error) {
	return append([]session.WorkspaceGrant(nil), s.grants...), nil
}

func (s *legacyYoloDeleteFailureStore) SaveWorkspaceGrant(context.Context, string, session.WorkspaceGrant) error {
	return nil
}

func (s *legacyYoloDeleteFailureStore) DeleteWorkspaceGrant(context.Context, string, string) error {
	return errors.New("delete unavailable")
}

type blockingInheritedWorkspaceStore struct {
	session.Store
	grants  session.WorkspaceGrantStore
	entered chan struct{}
	release chan struct{}
	blocked atomic.Bool
}

func (s *blockingInheritedWorkspaceStore) ListWorkspaceGrants(ctx context.Context, sessionID string) ([]session.WorkspaceGrant, error) {
	return s.grants.ListWorkspaceGrants(ctx, sessionID)
}

func (s *blockingInheritedWorkspaceStore) SaveWorkspaceGrant(ctx context.Context, sessionID string, grant session.WorkspaceGrant) error {
	if grant.Provenance == primaryWorkspaceProvenanceMainInherited && s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.grants.SaveWorkspaceGrant(ctx, sessionID, grant)
}

func (s *blockingInheritedWorkspaceStore) DeleteWorkspaceGrant(ctx context.Context, sessionID, grantID string) error {
	return s.grants.DeleteWorkspaceGrant(ctx, sessionID, grantID)
}

type failingInheritedWorkspaceStore struct {
	session.Store
	grants session.WorkspaceGrantStore
}

func (s *failingInheritedWorkspaceStore) ListWorkspaceGrants(ctx context.Context, sessionID string) ([]session.WorkspaceGrant, error) {
	return s.grants.ListWorkspaceGrants(ctx, sessionID)
}

func (s *failingInheritedWorkspaceStore) SaveWorkspaceGrant(ctx context.Context, sessionID string, grant session.WorkspaceGrant) error {
	if grant.Provenance == primaryWorkspaceProvenanceMainInherited {
		return errors.New("inherited save failed")
	}
	return s.grants.SaveWorkspaceGrant(ctx, sessionID, grant)
}

func (s *failingInheritedWorkspaceStore) DeleteWorkspaceGrant(ctx context.Context, sessionID, grantID string) error {
	return s.grants.DeleteWorkspaceGrant(ctx, sessionID, grantID)
}

func TestEnsurePrimaryWorkspaceAccessPromptsOnceUpFront(t *testing.T) {
	workspace := t.TempDir()
	manager := NewApprovalManager(NewToolPermissions())
	if err := manager.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	manager.WorkspacePromptFunc = func(got string) (WorkspaceApprovalResult, error) {
		prompts++
		if got != workspace {
			t.Fatalf("workspace prompt = %q, want %q", got, workspace)
		}
		return WorkspaceApprovalResult{Approved: true}, nil
	}

	if err := manager.EnsurePrimaryWorkspaceAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.EnsurePrimaryWorkspaceAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Fatalf("workspace prompts = %d, want one", prompts)
	}
	if !manager.IsWorkspacePathAllowed(filepath.Join(workspace, "file.txt"), true) {
		t.Fatal("proactive confirmation did not grant primary workspace access")
	}
}

func TestEnsurePrimaryWorkspaceAccessCancellationDefersDecision(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewApprovalManager(NewToolPermissions())
	if err := manager.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	manager.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		prompts++
		if prompts == 1 {
			return WorkspaceApprovalResult{Cancelled: true}, nil
		}
		return WorkspaceApprovalResult{Approved: true}, nil
	}

	if err := manager.EnsurePrimaryWorkspaceAccess(context.Background()); !errors.Is(err, ErrWorkspaceApprovalCancelled) {
		t.Fatalf("proactive cancellation error = %v, want cancellation sentinel", err)
	}
	if outcome, err := manager.CheckPathApproval(ReadFileToolName, path, path, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("first access after cancellation = %v, %v", outcome, err)
	}
	if prompts != 2 {
		t.Fatalf("workspace prompts = %d, want startup plus first access", prompts)
	}
}

func TestEnsurePrimaryWorkspaceAccessLatchesUpfrontDenial(t *testing.T) {
	manager := NewApprovalManager(NewToolPermissions())
	if err := manager.SetPrimaryWorkspace(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	manager.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		prompts++
		return WorkspaceApprovalResult{}, nil
	}
	if err := manager.EnsurePrimaryWorkspaceAccess(context.Background()); err == nil {
		t.Fatal("upfront workspace denial returned no error")
	}
	if err := manager.EnsurePrimaryWorkspaceAccess(context.Background()); err == nil {
		t.Fatal("latched workspace denial returned no error")
	}
	if prompts != 1 {
		t.Fatalf("workspace prompts after denial = %d, want one", prompts)
	}
}

func TestEnsurePrimaryWorkspaceAccessSkipsYolo(t *testing.T) {
	manager := NewApprovalManager(NewToolPermissions())
	if err := manager.SetPrimaryWorkspace(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	manager.SetApprovalMode(ModeYolo)
	manager.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		t.Fatal("yolo proactively prompted for workspace access")
		return WorkspaceApprovalResult{}, nil
	}
	if err := manager.EnsurePrimaryWorkspaceAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrimaryWorkspaceYoloBypassesConfirmationUntilModeChanges(t *testing.T) {
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	sibling := filepath.Join(parent, "workspace-other")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(workspace, "file.txt")
	outside := filepath.Join(sibling, "file.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	perms := NewToolPermissions()
	if err := perms.AddReadDir(workspace); err != nil {
		t.Fatal(err)
	}
	mgr := NewApprovalManager(perms)
	if err := mgr.SetPrimaryWorkspace(alias); err != nil {
		t.Fatal(err)
	}
	if mgr.IsWorkspacePathAllowed(inside, false) || mgr.IsWorkspacePathAllowed(inside, true) {
		t.Fatal("proposed primary workspace had initial file authority")
	}
	caps := mgr.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].Path != workspace || caps[0].Status != primaryWorkspaceStatusProposed {
		t.Fatalf("proposed capabilities = %#v", caps)
	}

	mgr.SetApprovalMode(ModeYolo)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		t.Fatal("primary workspace reached Guardian")
		return PolicyDecision{}, nil
	}, nil)
	prompts := 0
	mgr.WorkspacePromptFunc = func(path string) (WorkspaceApprovalResult, error) {
		prompts++
		if path != workspace {
			t.Fatalf("workspace prompt path = %q, want %q", path, workspace)
		}
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	if outcome, err := mgr.CheckPathApproval(ReadFileToolName, inside, inside, false); err != nil || outcome != ProceedOnce {
		t.Fatalf("yolo primary read approval = %v, %v", outcome, err)
	}
	if prompts != 0 {
		t.Fatalf("yolo workspace prompts = %d, want 0", prompts)
	}
	caps = mgr.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].Status != primaryWorkspaceStatusProposed || mgr.IsWorkspacePathAllowed(inside, true) {
		t.Fatalf("yolo persisted primary authority: capabilities=%#v allowed=%v", caps, mgr.IsWorkspacePathAllowed(inside, true))
	}

	mgr.SetApprovalMode(ModePrompt)
	if outcome, err := mgr.CheckPathApproval(ReadFileToolName, inside, inside, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("first non-yolo primary read approval = %v, %v", outcome, err)
	}
	if prompts != 1 {
		t.Fatalf("non-yolo workspace prompts = %d, want 1", prompts)
	}
	if outcome, err := mgr.CheckPathApproval(WriteFileToolName, filepath.Join(workspace, "new.txt"), "", true); err != nil || outcome != ProceedAlways {
		t.Fatalf("confirmed primary write approval = %v, %v", outcome, err)
	}
	if prompts != 1 || !mgr.IsWorkspacePathAllowed(inside, true) || mgr.IsWorkspacePathAllowed(outside, false) {
		t.Fatalf("prompts=%d inside=%v outside=%v", prompts, mgr.IsWorkspacePathAllowed(inside, true), mgr.IsWorkspacePathAllowed(outside, false))
	}
	caps = mgr.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].Status != primaryWorkspaceStatusConfirmed || caps[0].Provenance != primaryWorkspaceProvenanceConfirmed {
		t.Fatalf("confirmed capabilities = %#v", caps)
	}
}

func TestAdditionalWorkspaceReadGrantAndWriteElevationAreSeparateReviews(t *testing.T) {
	reference := t.TempDir()
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	var calls atomic.Int32
	mgr.SetPolicyReviewFunc(func(_ context.Context, req PolicyReviewRequest) (PolicyDecision, error) {
		calls.Add(1)
		if req.ToolName != ManageWorkspaceToolName || req.Path != reference || !req.IsDirectory || req.Reason == "" || req.ScopeID != "session-1" {
			t.Fatalf("workspace review request = %#v", req)
		}
		if calls.Load() == 1 && (req.WorkspaceAccess != "read" || req.IsWrite) {
			t.Fatalf("read review = %#v", req)
		}
		if calls.Load() == 2 && (req.WorkspaceAccess != "write" || !req.IsWrite) {
			t.Fatalf("write review = %#v", req)
		}
		if !strings.Contains(req.ApprovalContext, "workspace_scope") {
			t.Fatalf("approval context missing workspace scope: %q", req.ApprovalContext)
		}
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Rationale: "explicitly requested"}, nil
	}, nil)
	ctx := llm.ContextWithSessionID(context.Background(), "session-1")
	ctx = llm.ContextWithApprovalTranscript(ctx, []llm.Message{llm.UserText("compare against the reference repository")})

	readResult, err := mgr.GrantWorkspace(ctx, reference, session.WorkspaceAccessRead, "reference requested by user")
	if err != nil {
		t.Fatal(err)
	}
	if !readResult.Changed || readResult.Capability.Access != session.WorkspaceAccessRead {
		t.Fatalf("read result = %#v", readResult)
	}
	readPath := filepath.Join(reference, "input.txt")
	if err := os.WriteFile(readPath, []byte("reference"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mgr.IsWorkspacePathAllowed(readPath, false) {
		t.Fatal("read grant did not allow read")
	}
	if mgr.IsWorkspacePathAllowed(filepath.Join(reference, "output.txt"), true) {
		t.Fatal("read grant allowed write")
	}

	duplicate, err := mgr.GrantWorkspace(ctx, reference, session.WorkspaceAccessRead, "same reference")
	if err != nil || duplicate.Changed {
		t.Fatalf("duplicate result = %#v, %v", duplicate, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate triggered %d reviews", calls.Load())
	}

	writeResult, err := mgr.GrantWorkspace(ctx, reference, session.WorkspaceAccessWrite, "user requested edits")
	if err != nil {
		t.Fatal(err)
	}
	if !writeResult.Changed || writeResult.Capability.ID != readResult.Capability.ID {
		t.Fatalf("write elevation = %#v", writeResult)
	}
	if calls.Load() != 2 || !mgr.IsWorkspacePathAllowed(filepath.Join(reference, "output.txt"), true) {
		t.Fatalf("write elevation reviews=%d allowed=%v", calls.Load(), mgr.IsWorkspacePathAllowed(filepath.Join(reference, "output.txt"), true))
	}
}

func TestWorkspaceGuardianDenialInstallsNothingAndDoesNotPrompt(t *testing.T) {
	target := t.TempDir()
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, RiskLevel: "high", UserAuthorization: "low", Rationale: "not authorized"}, nil
	}, nil)
	prompted := false
	mgr.PromptUIFunc = func(string, bool, bool, string) (ApprovalResult, error) {
		prompted = true
		return ApprovalResult{}, nil
	}

	_, err := mgr.GrantWorkspace(context.Background(), target, session.WorkspaceAccessRead, "inspect reference")
	if err == nil || !strings.Contains(err.Error(), guardianSafeNextStep) {
		t.Fatalf("denial error = %v", err)
	}
	if prompted || len(mgr.WorkspaceCapabilities()) != 0 {
		t.Fatalf("prompted=%v capabilities=%#v", prompted, mgr.WorkspaceCapabilities())
	}
}

func TestCanonicalWorkspaceRootNarrowsToEnclosingRepository(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	sibling := filepath.Join(parent, "sibling")
	if err := os.MkdirAll(filepath.Join(repo, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", repo)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v: %s", err, output)
	}
	root, err := CanonicalWorkspaceRoot(filepath.Join(repo, "nested", "deep"), "")
	if err != nil {
		t.Fatal(err)
	}
	if root != repo {
		t.Fatalf("root = %q, want %q", root, repo)
	}
	if pathWithinWorkspace(sibling, root) {
		t.Fatalf("repository root %q broadened to sibling %q", root, sibling)
	}
	if _, err := CanonicalWorkspaceRoot(filepath.Join(parent, "missing"), ""); err == nil {
		t.Fatal("missing workspace accepted")
	}
}

func TestWorkspaceRebindingPreservesDynamicGrantsAndNeverApprovesShell(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	reference := t.TempDir()
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeYolo)
	if err := mgr.SetPrimaryWorkspace(first); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.GrantWorkspace(context.Background(), reference, session.WorkspaceAccessRead, "reference"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetPrimaryWorkspace(second); err != nil {
		t.Fatal(err)
	}
	caps := mgr.WorkspaceCapabilities()
	if len(caps) != 2 || caps[0].Path != second {
		t.Fatalf("capabilities after rebind = %#v", caps)
	}
	if _, _, err := mgr.RevokeWorkspace(context.Background(), primaryWorkspaceID); err == nil {
		t.Fatal("primary workspace was revocable")
	}
	mgr.SetApprovalMode(ModePrompt)
	if outcome, err := mgr.CheckShellApproval("cat file.txt", reference); outcome != Cancel || err == nil {
		t.Fatalf("workspace approved shell: outcome=%v err=%v", outcome, err)
	}
}

func TestWorkspaceGrantsAreRootSharedAndRevocationIsImmediate(t *testing.T) {
	target := t.TempDir()
	root := NewApprovalManager(NewToolPermissions())
	root.SetApprovalMode(ModeYolo)
	child := NewApprovalManager(NewToolPermissions())
	if err := child.SetParent(root); err != nil {
		t.Fatal(err)
	}
	result, err := child.GrantWorkspace(context.Background(), target, session.WorkspaceAccessRead, "reference")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(target, "file.txt")
	if err := os.WriteFile(path, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !root.IsWorkspacePathAllowed(path, false) || !child.IsWorkspacePathAllowed(path, false) {
		t.Fatal("root/child did not share grant")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = child.IsWorkspacePathAllowed(path, false)
			}
		}()
	}
	if _, changed, err := root.RevokeWorkspace(context.Background(), result.Capability.ID); err != nil || !changed {
		t.Fatalf("revoke changed=%v err=%v", changed, err)
	}
	wg.Wait()
	if root.IsWorkspacePathAllowed(path, false) || child.IsWorkspacePathAllowed(path, false) {
		t.Fatal("revoked grant remained visible")
	}
}

func TestYoloWorkspaceGrantsAreEphemeralRootSharedOverlays(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.Create(ctx, &session.Session{ID: "yolo-ephemeral", Provider: "mock", Model: "model", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}

	for _, access := range []session.WorkspaceAccess{session.WorkspaceAccessRead, session.WorkspaceAccessWrite} {
		t.Run(string(access), func(t *testing.T) {
			target := t.TempDir()
			path := filepath.Join(target, "file.txt")
			if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
				t.Fatal(err)
			}
			root := NewApprovalManager(NewToolPermissions())
			root.SetApprovalMode(ModeYolo)
			if err := root.ConfigureWorkspacePersistence(ctx, store, "yolo-ephemeral"); err != nil {
				t.Fatal(err)
			}
			child := NewApprovalManager(NewToolPermissions())
			if err := child.SetParent(root); err != nil {
				t.Fatal(err)
			}

			granted, err := child.GrantWorkspace(ctx, target, access, "temporary yolo workspace")
			if err != nil {
				t.Fatal(err)
			}
			if !granted.Changed || granted.Persisted || granted.Capability.Provenance != workspaceProvenanceYolo {
				t.Fatalf("yolo grant = %#v", granted)
			}
			duplicate, err := root.GrantWorkspace(ctx, target, access, "redundant yolo workspace")
			if err != nil || duplicate.Changed || duplicate.Persisted || duplicate.Capability.ID != granted.Capability.ID {
				t.Fatalf("duplicate yolo grant = %#v, %v", duplicate, err)
			}
			if !root.IsWorkspacePathAllowed(path, false) || !child.IsWorkspacePathAllowed(path, false) {
				t.Fatal("root/child did not share yolo grant")
			}
			wantWrite := access == session.WorkspaceAccessWrite
			if root.IsWorkspacePathAllowed(path, true) != wantWrite || child.IsWorkspacePathAllowed(path, true) != wantWrite {
				t.Fatalf("yolo write access = root:%v child:%v, want %v", root.IsWorkspacePathAllowed(path, true), child.IsWorkspacePathAllowed(path, true), wantWrite)
			}
			stored, err := store.ListWorkspaceGrants(ctx, "yolo-ephemeral")
			if err != nil || len(stored) != 0 {
				t.Fatalf("persisted yolo rows = %#v, %v", stored, err)
			}

			root.SetApprovalMode(ModePrompt)
			if len(root.WorkspaceCapabilities()) != 0 || root.IsWorkspacePathAllowed(path, false) || child.IsWorkspacePathAllowed(path, false) {
				t.Fatalf("yolo grant survived mode exit: %#v", root.WorkspaceCapabilities())
			}
			root.SetApprovalMode(ModeYolo)
			if len(root.WorkspaceCapabilities()) != 0 || root.IsWorkspacePathAllowed(path, false) {
				t.Fatalf("yolo grant resurrected on re-entry: %#v", root.WorkspaceCapabilities())
			}
		})
	}
}

func TestYoloWorkspaceWriteElevationRestoresDurableReadBaseline(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.Create(ctx, &session.Session{ID: "yolo-elevation", Provider: "mock", Model: "model", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	path := filepath.Join(target, "file.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Rationale: "authorized read"}, nil
	}, nil)
	if err := mgr.ConfigureWorkspacePersistence(ctx, store, "yolo-elevation"); err != nil {
		t.Fatal(err)
	}
	baseline, err := mgr.GrantWorkspace(ctx, target, session.WorkspaceAccessRead, "durable reference")
	if err != nil || !baseline.Persisted {
		t.Fatalf("baseline grant = %#v, %v", baseline, err)
	}

	mgr.SetApprovalMode(ModeYolo)
	elevated, err := mgr.GrantWorkspace(ctx, target, session.WorkspaceAccessWrite, "temporary edits")
	if err != nil {
		t.Fatal(err)
	}
	if !elevated.Changed || elevated.Persisted || elevated.Capability.ID == baseline.Capability.ID || elevated.Capability.Provenance != workspaceProvenanceYolo {
		t.Fatalf("yolo elevation = %#v over %#v", elevated, baseline)
	}
	duplicate, err := mgr.GrantWorkspace(ctx, target, session.WorkspaceAccessWrite, "redundant temporary edits")
	if err != nil || duplicate.Changed || duplicate.Persisted || duplicate.Capability.ID != elevated.Capability.ID {
		t.Fatalf("duplicate elevation = %#v, %v", duplicate, err)
	}
	if !mgr.IsWorkspacePathAllowed(path, true) {
		t.Fatal("yolo elevation did not allow write")
	}
	stored, err := store.ListWorkspaceGrants(ctx, "yolo-elevation")
	if err != nil || len(stored) != 1 || stored[0].ID != baseline.Capability.ID || stored[0].Access != session.WorkspaceAccessRead || stored[0].Provenance != "guardian" {
		t.Fatalf("durable baseline changed by yolo elevation: %#v, %v", stored, err)
	}

	mgr.SetApprovalMode(ModePrompt)
	caps := mgr.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].ID != baseline.Capability.ID || caps[0].Access != session.WorkspaceAccessRead || caps[0].Provenance != "guardian" {
		t.Fatalf("baseline after leaving yolo = %#v", caps)
	}
	if !mgr.IsWorkspacePathAllowed(path, false) || mgr.IsWorkspacePathAllowed(path, true) {
		t.Fatalf("restored baseline access read=%v write=%v", mgr.IsWorkspacePathAllowed(path, false), mgr.IsWorkspacePathAllowed(path, true))
	}
	mgr.SetApprovalMode(ModeYolo)
	caps = mgr.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].Access != session.WorkspaceAccessRead || mgr.IsWorkspacePathAllowed(path, true) {
		t.Fatalf("elevation resurrected after yolo re-entry: %#v", caps)
	}
}

func TestYoloWorkspaceGrantExitIsRaceSafe(t *testing.T) {
	root := NewApprovalManager(NewToolPermissions())
	root.SetApprovalMode(ModeYolo)
	child := NewApprovalManager(NewToolPermissions())
	if err := child.SetParent(root); err != nil {
		t.Fatal(err)
	}
	targets := make([]string, 8)
	for i := range targets {
		targets[i] = t.TempDir()
	}
	if _, err := root.GrantWorkspace(context.Background(), targets[0], session.WorkspaceAccessRead, "seed overlay"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = child.GrantWorkspace(context.Background(), targets[i], session.WorkspaceAccessRead, "concurrent overlay")
				_ = root.IsWorkspacePathAllowed(filepath.Join(targets[i], "file.txt"), false)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			root.SetApprovalMode(ModePrompt)
			root.SetApprovalMode(ModeYolo)
		}
	}()
	wg.Wait()
	root.SetApprovalMode(ModePrompt)
	if caps := root.WorkspaceCapabilities(); len(caps) != 0 {
		t.Fatalf("concurrent yolo grants survived final exit: %#v", caps)
	}
	for _, target := range targets {
		if root.IsWorkspacePathAllowed(filepath.Join(target, "file.txt"), false) || child.IsWorkspacePathAllowed(filepath.Join(target, "file.txt"), false) {
			t.Fatalf("concurrent yolo grant survived for %s", target)
		}
	}
}

func TestWorkspacePersistenceRehydratesBeforePathChecks(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	sess := &session.Session{ID: "session-persist", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginTUI, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	first := NewApprovalManager(NewToolPermissions())
	first.SetApprovalMode(ModeAuto)
	first.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Rationale: "authorized reference"}, nil
	}, nil)
	if err := first.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	granted, err := first.GrantWorkspace(ctx, target, session.WorkspaceAccessRead, "reference")
	if err != nil || !granted.Persisted {
		t.Fatalf("grant = %#v, %v", granted, err)
	}

	resumed := NewApprovalManager(NewToolPermissions())
	if err := resumed.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	readPath := filepath.Join(target, "file.txt")
	if err := os.WriteFile(readPath, []byte("persisted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !resumed.IsWorkspacePathAllowed(readPath, false) || resumed.IsWorkspacePathAllowed(readPath, true) {
		t.Fatal("rehydrated read grant has wrong access")
	}
}

func TestChildWorkspaceGrantPersistsToRootSession(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	for _, id := range []string{"parent-session", "child-session"} {
		if err := store.Create(ctx, &session.Session{ID: id, Provider: "mock", Model: "model", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	root := NewApprovalManager(NewToolPermissions())
	root.SetApprovalMode(ModeAuto)
	root.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Rationale: "authorized reference"}, nil
	}, nil)
	if err := root.ConfigureWorkspacePersistence(ctx, store, "parent-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.GrantWorkspace(ctx, t.TempDir(), session.WorkspaceAccessRead, "parent reference"); err != nil {
		t.Fatal(err)
	}
	child := NewApprovalManager(NewToolPermissions())
	if err := child.SetParent(root); err != nil {
		t.Fatal(err)
	}
	if err := child.ConfigureWorkspacePersistence(ctx, store, "child-session"); err != nil {
		t.Fatal(err)
	}
	childCtx := llm.ContextWithSessionID(ctx, "child-session")
	if _, err := child.GrantWorkspace(childCtx, t.TempDir(), session.WorkspaceAccessRead, "reference"); err != nil {
		t.Fatal(err)
	}
	parentGrants, err := store.ListWorkspaceGrants(ctx, "parent-session")
	if err != nil || len(parentGrants) != 2 {
		t.Fatalf("parent grants = %#v, %v", parentGrants, err)
	}
	childGrants, err := store.ListWorkspaceGrants(ctx, "child-session")
	if err != nil || len(childGrants) != 0 {
		t.Fatalf("child persisted independent grants = %#v, %v", childGrants, err)
	}
}

func TestWorkspacePersistenceFailureDoesNotInstallGrant(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sess := &session.Session{ID: "session-failure", Provider: "mock", Model: "model", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Rationale: "authorized reference"}, nil
	}, nil)
	if err := mgr.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.GrantWorkspace(ctx, t.TempDir(), session.WorkspaceAccessRead, "reference"); err == nil {
		t.Fatal("grant succeeded after persistence failure")
	}
	if capabilities := mgr.WorkspaceCapabilities(); len(capabilities) != 0 {
		t.Fatalf("failed durable grant installed in memory: %#v", capabilities)
	}
}

func TestWorkspacePersistenceIgnoresAndDeletesLegacyYoloRows(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	if err := store.Create(ctx, &session.Session{ID: "legacy-yolo", Provider: "mock", Model: "model", Mode: session.ModeChat, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}); err != nil {
		t.Fatal(err)
	}
	durablePath := t.TempDir()
	yoloPath := t.TempDir()
	for _, grant := range []session.WorkspaceGrant{
		{ID: "durable-read", Path: durablePath, Access: session.WorkspaceAccessRead, Provenance: "guardian", Rationale: "authorized reference", CreatedAt: now, UpdatedAt: now},
		{ID: "legacy-yolo-write", Path: yoloPath, Access: session.WorkspaceAccessWrite, Provenance: workspaceProvenanceYolo, Rationale: "legacy bug", CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.SaveWorkspaceGrant(ctx, "legacy-yolo", grant); err != nil {
			t.Fatal(err)
		}
	}

	resumed := NewApprovalManager(NewToolPermissions())
	if err := resumed.ConfigureWorkspacePersistence(ctx, store, "legacy-yolo"); err != nil {
		t.Fatal(err)
	}
	caps := resumed.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].ID != "durable-read" || caps[0].Path != durablePath {
		t.Fatalf("resumed capabilities = %#v", caps)
	}
	if resumed.IsWorkspacePathAllowed(filepath.Join(yoloPath, "file.txt"), false) {
		t.Fatal("legacy yolo row became runtime authority")
	}
	stored, err := store.ListWorkspaceGrants(ctx, "legacy-yolo")
	if err != nil || len(stored) != 1 || stored[0].ID != "durable-read" {
		t.Fatalf("legacy yolo cleanup = %#v, %v", stored, err)
	}

	deleteFailure := &legacyYoloDeleteFailureStore{
		Store: &session.NoopStore{},
		grants: []session.WorkspaceGrant{{
			ID: "undeletable-yolo", Path: yoloPath, Access: session.WorkspaceAccessWrite,
			Provenance: workspaceProvenanceYolo, Rationale: "legacy bug", CreatedAt: now, UpdatedAt: now,
		}},
	}
	bestEffort := NewApprovalManager(NewToolPermissions())
	if err := bestEffort.ConfigureWorkspacePersistence(ctx, deleteFailure, "legacy-yolo-delete-failure"); err != nil {
		t.Fatalf("best-effort legacy cleanup blocked resume: %v", err)
	}
	if caps := bestEffort.WorkspaceCapabilities(); len(caps) != 0 {
		t.Fatalf("undeletable legacy yolo row became authority: %#v", caps)
	}
}

func TestMissingOptionalWorkspaceStoreRemainsRuntimeOnly(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeYolo)
	logging := session.NewLoggingStore(&session.NoopStore{}, nil)
	if _, falselySupported := any(logging).(session.WorkspaceGrantStore); falselySupported {
		t.Fatal("logging wrapper exposed unsupported workspace persistence")
	}
	if err := mgr.ConfigureWorkspacePersistence(context.Background(), logging, "session-no-capability"); err != nil {
		t.Fatal(err)
	}
	result, err := mgr.GrantWorkspace(context.Background(), t.TempDir(), session.WorkspaceAccessRead, "reference")
	if err != nil {
		t.Fatal(err)
	}
	if result.Persisted || !result.Changed {
		t.Fatalf("runtime-only grant = %#v", result)
	}
	if err := mgr.ConfigureWorkspacePersistence(context.Background(), logging, "another-session"); err != nil {
		t.Fatal(err)
	}
	if capabilities := mgr.WorkspaceCapabilities(); len(capabilities) != 0 {
		t.Fatalf("runtime-only grant leaked across session scopes: %#v", capabilities)
	}
}

func TestPrimaryWorkspaceNoTransportAndHumanDenialFailClosed(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := NewApprovalManager(NewToolPermissions())
	if err := mgr.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		t.Fatal("Guardian cannot establish primary authority")
		return PolicyDecision{}, nil
	}, nil)
	if _, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false); err == nil || !strings.Contains(err.Error(), "no workspace approval transport") {
		t.Fatalf("missing transport error = %v", err)
	}

	prompts := 0
	mgr.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		prompts++
		return WorkspaceApprovalResult{}, nil
	}
	if _, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false); err == nil || !strings.Contains(err.Error(), "human denied") {
		t.Fatalf("human denial error = %v", err)
	}
	if _, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false); err == nil || !strings.Contains(err.Error(), "was denied by the human") {
		t.Fatalf("latched denial error = %v", err)
	}
	if prompts != 1 {
		t.Fatalf("workspace prompts = %d, want one", prompts)
	}
}

func TestPrimaryWorkspaceTrustLookupFailureFallsBackToHumanPrompt(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.workspaceTrustStore = failingWorkspaceTrustStore{checkErr: errors.New("malformed ledger")}
	if err := mgr.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	mgr.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		prompts++
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	if outcome, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("lookup fallback confirmation = %v, %v", outcome, err)
	}
	if prompts != 1 {
		t.Fatalf("workspace prompts = %d, want 1", prompts)
	}
}

func TestRememberedPrimaryWorkspaceAppliesToFutureSessions(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	trustPath := filepath.Join(t.TempDir(), "remembered-workspaces.yaml")
	trustStore := &fileWorkspaceTrustStore{path: trustPath}

	first := NewApprovalManager(NewToolPermissions())
	first.workspaceTrustStore = trustStore
	if err := first.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	first.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		return WorkspaceApprovalResult{Approved: true, Remember: true}, nil
	}
	if outcome, err := first.CheckPathApproval(ReadFileToolName, path, path, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("remembered confirmation = %v, %v", outcome, err)
	}

	future := NewApprovalManager(NewToolPermissions())
	future.workspaceTrustStore = trustStore
	if err := future.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if outcome, err := future.CheckPathApproval(ReadFileToolName, path, path, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("future session remembered confirmation = %v, %v", outcome, err)
	}

	sessionOnly := NewApprovalManager(NewToolPermissions())
	sessionOnly.workspaceTrustStore = &fileWorkspaceTrustStore{path: filepath.Join(t.TempDir(), "empty.yaml")}
	if err := sessionOnly.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	sessionOnly.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		prompts++
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	if outcome, err := sessionOnly.CheckPathApproval(ReadFileToolName, path, path, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("session-only confirmation = %v, %v", outcome, err)
	}
	if prompts != 1 {
		t.Fatalf("session-only prompts = %d, want 1", prompts)
	}
}

func TestApprovedMainWorktreeAutomaticallyConfirmsLinkedWorktrees(t *testing.T) {
	main, linked, sibling := newWorkspaceTrustWorktrees(t)
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.workspaceTrustStore = &fileWorkspaceTrustStore{path: filepath.Join(t.TempDir(), "empty.yaml")}
	if err := mgr.SetPrimaryWorkspace(main); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	mgr.WorkspacePromptFunc = func(workspace string) (WorkspaceApprovalResult, error) {
		prompts++
		if workspace != main {
			t.Fatalf("unexpected workspace prompt for %q", workspace)
		}
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	if outcome, err := mgr.CheckPathApproval(ReadFileToolName, filepath.Join(main, "README.md"), filepath.Join(main, "README.md"), false); err != nil || outcome != ProceedAlways {
		t.Fatalf("main worktree confirmation = %v, %v", outcome, err)
	}

	for _, workspace := range []string{linked, sibling, main} {
		if err := mgr.SetPrimaryWorkspace(workspace); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(workspace, "README.md")
		if !mgr.IsWorkspacePathAllowed(path, true) {
			t.Fatalf("workspace %q did not inherit main worktree approval", workspace)
		}
		if outcome, err := mgr.CheckPathApproval(WriteFileToolName, path, path, true); err != nil || outcome != ProceedAlways {
			t.Fatalf("inherited worktree confirmation for %q = %v, %v", workspace, outcome, err)
		}
	}
	if prompts != 1 {
		t.Fatalf("workspace prompts = %d, want only the main worktree prompt", prompts)
	}
}

func TestUnapprovedMainWorktreeDoesNotConfirmLinkedWorktree(t *testing.T) {
	main, linked, _ := newWorkspaceTrustWorktrees(t)
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.workspaceTrustStore = &fileWorkspaceTrustStore{path: filepath.Join(t.TempDir(), "empty.yaml")}
	if err := mgr.SetPrimaryWorkspace(main); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetPrimaryWorkspace(linked); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linked, "README.md")
	if mgr.IsWorkspacePathAllowed(path, false) {
		t.Fatal("linked worktree inherited an unapproved main worktree proposal")
	}
	prompts := 0
	mgr.WorkspacePromptFunc = func(workspace string) (WorkspaceApprovalResult, error) {
		prompts++
		if workspace != linked {
			t.Fatalf("workspace prompt = %q, want %q", workspace, linked)
		}
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	if outcome, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("linked worktree confirmation = %v, %v", outcome, err)
	}
	if prompts != 1 {
		t.Fatalf("workspace prompts = %d, want 1", prompts)
	}
}

func TestMainWorktreeApprovalDoesNotCrossWorkspaceBoundaries(t *testing.T) {
	main, _, _ := newWorkspaceTrustWorktrees(t)
	otherMain, otherLinked, _ := newWorkspaceTrustWorktrees(t)
	plain := t.TempDir()

	for _, candidate := range []string{otherMain, otherLinked, plain} {
		mgr := NewApprovalManager(NewToolPermissions())
		mgr.workspaceTrustStore = &fileWorkspaceTrustStore{path: filepath.Join(t.TempDir(), "empty.yaml")}
		if err := mgr.SetPrimaryWorkspace(main); err != nil {
			t.Fatal(err)
		}
		mgr.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
			return WorkspaceApprovalResult{Approved: true}, nil
		}
		mainPath := filepath.Join(main, "README.md")
		if _, err := mgr.CheckPathApproval(ReadFileToolName, mainPath, mainPath, false); err != nil {
			t.Fatal(err)
		}
		if err := mgr.SetPrimaryWorkspace(candidate); err != nil {
			t.Fatal(err)
		}
		candidatePath := filepath.Join(candidate, "README.md")
		if candidate == plain {
			candidatePath = filepath.Join(candidate, "file.txt")
			if err := os.WriteFile(candidatePath, []byte("fixture"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if mgr.IsWorkspacePathAllowed(candidatePath, false) {
			t.Fatalf("main worktree approval crossed into %q", candidate)
		}
	}
}

func TestInheritedMainWorktreeApprovalPersistsAndResumes(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess := &session.Session{ID: "inherited-resume", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginTUI, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	main, linked, _ := newWorkspaceTrustWorktrees(t)
	mgr := NewApprovalManager(NewToolPermissions())
	if err := mgr.SetPrimaryWorkspace(main); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	mgr.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	mainPath := filepath.Join(main, "README.md")
	if _, err := mgr.CheckPathApproval(ReadFileToolName, mainPath, mainPath, false); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetPrimaryWorkspaceWithContext(ctx, linked); err != nil {
		t.Fatal(err)
	}

	resumed := NewApprovalManager(NewToolPermissions())
	if err := resumed.SetPrimaryWorkspace(linked); err != nil {
		t.Fatal(err)
	}
	if err := resumed.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(linked, "README.md")
	if !resumed.IsWorkspacePathAllowed(linkedPath, true) {
		t.Fatal("persisted inherited worktree approval was not restored")
	}
	caps := resumed.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].Status != primaryWorkspaceStatusConfirmed || caps[0].Provenance != primaryWorkspaceProvenanceMainInherited || caps[0].Rationale != primaryWorkspaceRationaleInheritedMain {
		t.Fatalf("restored inherited capability = %#v", caps)
	}
}

func TestInheritedMainWorktreePersistenceFailureInstallsNoAuthority(t *testing.T) {
	ctx := context.Background()
	sqliteStore, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteStore.Close()
	sess := &session.Session{ID: "inherited-failure", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginTUI, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive}
	if err := sqliteStore.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	store := &failingInheritedWorkspaceStore{Store: sqliteStore, grants: sqliteStore}
	main, linked, _ := newWorkspaceTrustWorktrees(t)
	mgr := NewApprovalManager(NewToolPermissions())
	if err := mgr.SetPrimaryWorkspace(main); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	mgr.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	mainPath := filepath.Join(main, "README.md")
	if _, err := mgr.CheckPathApproval(ReadFileToolName, mainPath, mainPath, false); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SetPrimaryWorkspaceWithContext(ctx, linked); err == nil || !strings.Contains(err.Error(), "persist inherited primary workspace confirmation") {
		t.Fatalf("inherited persistence error = %v", err)
	}
	if mgr.IsWorkspacePathAllowed(filepath.Join(linked, "README.md"), true) {
		t.Fatal("failed inherited persistence installed write authority")
	}
	if !mgr.IsWorkspacePathAllowed(mainPath, true) {
		t.Fatal("failed inherited persistence removed the existing main approval")
	}
}

func TestPersistenceRescopeCannotInstallStaleInheritedAuthority(t *testing.T) {
	ctx := context.Background()
	sqliteStore, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteStore.Close()
	for _, id := range []string{"scope-one", "scope-two"} {
		sess := &session.Session{ID: id, Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginTUI, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: session.StatusActive}
		if err := sqliteStore.Create(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	store := &blockingInheritedWorkspaceStore{Store: sqliteStore, grants: sqliteStore, entered: make(chan struct{}), release: make(chan struct{})}
	main, linked, _ := newWorkspaceTrustWorktrees(t)
	mgr := NewApprovalManager(NewToolPermissions())
	if err := mgr.SetPrimaryWorkspace(main); err != nil {
		t.Fatal(err)
	}
	if err := mgr.ConfigureWorkspacePersistence(ctx, store, "scope-one"); err != nil {
		t.Fatal(err)
	}
	mgr.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	mainPath := filepath.Join(main, "README.md")
	if _, err := mgr.CheckPathApproval(ReadFileToolName, mainPath, mainPath, false); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- mgr.SetPrimaryWorkspaceWithContext(ctx, linked) }()
	<-store.entered
	if err := mgr.ConfigureWorkspacePersistence(ctx, store, "scope-two"); err != nil {
		t.Fatal(err)
	}
	close(store.release)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	linkedPath := filepath.Join(linked, "README.md")
	if mgr.IsWorkspacePathAllowed(linkedPath, true) {
		t.Fatal("stale approval from the previous session scope installed write authority")
	}
	grants, err := sqliteStore.ListWorkspaceGrants(ctx, "scope-two")
	if err != nil || len(grants) != 1 || grants[0].Path != linked || grants[0].Provenance != primaryWorkspaceProvenanceProposed {
		t.Fatalf("new scope grants = %#v, %v", grants, err)
	}
}

func TestRememberedPrimaryWorkspacePersistenceFailureInstallsNoAuthority(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.workspaceTrustStore = failingWorkspaceTrustStore{rememberErr: errors.New("disk full")}
	if err := mgr.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	mgr.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		return WorkspaceApprovalResult{Approved: true, Remember: true}, nil
	}
	if _, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false); err == nil || !strings.Contains(err.Error(), "remember primary workspace") {
		t.Fatalf("remember failure = %v", err)
	}
	if mgr.IsWorkspacePathAllowed(path, false) {
		t.Fatal("failed remembered approval installed primary authority")
	}
}

func TestConcurrentParentChildPrimaryFirstAccessUsesOnePrompt(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := NewApprovalManager(NewToolPermissions())
	if err := root.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	child := NewApprovalManager(NewToolPermissions())
	if err := child.SetParent(root); err != nil {
		t.Fatal(err)
	}
	var prompts atomic.Int32
	root.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		prompts.Add(1)
		return WorkspaceApprovalResult{Approved: true}, nil
	}

	const callers = 12
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			mgr := root
			if i%2 == 1 {
				mgr = child
			}
			outcome, err := mgr.CheckPathApproval(ReadFileToolName, path, path, false)
			if err != nil || outcome != ProceedAlways {
				errs <- fmt.Errorf("outcome=%v err=%v", outcome, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if prompts.Load() != 1 {
		t.Fatalf("workspace prompts = %d, want 1", prompts.Load())
	}
}

func TestPrimaryWorkspaceConfirmationPersistsResumesAndRebinds(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	sess := &session.Session{ID: "primary-persist", Provider: "mock", Model: "model", Mode: session.ModeChat, Origin: session.OriginTUI, CreatedAt: now, UpdatedAt: now, Status: session.StatusActive}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}
	firstWorkspace := t.TempDir()
	secondWorkspace := t.TempDir()
	reference := t.TempDir()
	firstPath := filepath.Join(firstWorkspace, "one.txt")
	secondPath := filepath.Join(secondWorkspace, "two.txt")
	for _, path := range []string{firstPath, secondPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	first := NewApprovalManager(NewToolPermissions())
	if err := first.SetPrimaryWorkspace(firstWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := first.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	first.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	if outcome, err := first.CheckPathApproval(ReadFileToolName, firstPath, firstPath, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("first confirmation = %v, %v", outcome, err)
	}
	first.SetApprovalMode(ModeYolo)
	if _, err := first.GrantWorkspace(ctx, reference, session.WorkspaceAccessRead, "reference"); err != nil {
		t.Fatal(err)
	}

	resumed := NewApprovalManager(NewToolPermissions())
	if err := resumed.SetPrimaryWorkspace(firstWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := resumed.ConfigureWorkspacePersistence(ctx, store, sess.ID); err != nil {
		t.Fatal(err)
	}
	resumed.WorkspacePromptFunc = func(string) (WorkspaceApprovalResult, error) {
		t.Fatal("persisted primary confirmation prompted on resume")
		return WorkspaceApprovalResult{}, nil
	}
	if outcome, err := resumed.CheckPathApproval(ReadFileToolName, firstPath, firstPath, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("resumed confirmation = %v, %v", outcome, err)
	}

	if err := resumed.SetPrimaryWorkspaceWithContext(ctx, secondWorkspace); err != nil {
		t.Fatal(err)
	}
	caps := resumed.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].Path != secondWorkspace || caps[0].Status != primaryWorkspaceStatusProposed {
		t.Fatalf("capabilities after rebind = %#v", caps)
	}
	if resumed.IsWorkspacePathAllowed(firstPath, false) || resumed.IsWorkspacePathAllowed(reference, false) {
		t.Fatal("rebind retained old primary or resurrected ephemeral yolo grant")
	}

	// An explicit yolo access must neither invoke the installed callback nor
	// promote the durable proposal to confirmed authority.
	resumed.SetApprovalMode(ModeYolo)
	if outcome, err := resumed.CheckPathApproval(ReadFileToolName, secondPath, secondPath, false); err != nil || outcome != ProceedOnce {
		t.Fatalf("yolo rebound access = %v, %v", outcome, err)
	}
	stored, err := store.ListWorkspaceGrants(ctx, sess.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored workspace rows after yolo access = %#v, %v", stored, err)
	}
	foundPrimary := false
	for _, grant := range stored {
		if grant.ID != primaryWorkspaceID {
			continue
		}
		foundPrimary = true
		if grant.Path != secondWorkspace || grant.Provenance != primaryWorkspaceProvenanceProposed {
			t.Fatalf("stored primary after yolo access = %#v", grant)
		}
	}
	if !foundPrimary {
		t.Fatalf("stored workspace rows after yolo access lack primary: %#v", stored)
	}

	resumed.SetApprovalMode(ModePrompt)
	prompts := 0
	resumed.WorkspacePromptFunc = func(path string) (WorkspaceApprovalResult, error) {
		prompts++
		if path != secondWorkspace {
			t.Fatalf("rebound prompt = %q", path)
		}
		return WorkspaceApprovalResult{Approved: true}, nil
	}
	if outcome, err := resumed.CheckPathApproval(ReadFileToolName, secondPath, secondPath, false); err != nil || outcome != ProceedAlways {
		t.Fatalf("rebound confirmation = %v, %v", outcome, err)
	}
	if prompts != 1 {
		t.Fatalf("rebound prompts = %d", prompts)
	}
	stored, err = store.ListWorkspaceGrants(ctx, sess.ID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored workspace rows = %#v, %v", stored, err)
	}
	for _, grant := range stored {
		if grant.ID == primaryWorkspaceID && (grant.Path != secondWorkspace || grant.Provenance != primaryWorkspaceProvenanceConfirmed) {
			t.Fatalf("stored primary = %#v", grant)
		}
	}
}

func TestManageWorkspaceCannotConfirmProposedPrimary(t *testing.T) {
	workspace := t.TempDir()
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeYolo)
	if err := mgr.SetPrimaryWorkspace(workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.GrantWorkspace(context.Background(), workspace, session.WorkspaceAccessWrite, "work here"); err == nil || !strings.Contains(err.Error(), "can only be confirmed by the human") {
		t.Fatalf("model grant error = %v", err)
	}
	caps := mgr.WorkspaceCapabilities()
	if len(caps) != 1 || caps[0].Status != primaryWorkspaceStatusProposed {
		t.Fatalf("capabilities = %#v", caps)
	}
}

func TestManageWorkspaceAutomaticRegistration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled []string
		want    bool
	}{
		{name: "explicit read", enabled: []string{ReadFileToolName}, want: true},
		{name: "explicit write", enabled: []string{WriteFileToolName}, want: true},
		{name: "all", enabled: StandardToolNames(), want: true},
		{name: "no path", enabled: []string{ShellToolName, AskUserToolName}, want: false},
		{name: "manage alone", enabled: []string{ManageWorkspaceToolName}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, err := NewLocalToolRegistry(&ToolConfig{Enabled: tc.enabled}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, got := registry.Get(ManageWorkspaceToolName)
			if got != tc.want {
				t.Fatalf("manage_workspace present=%v, want %v", got, tc.want)
			}
		})
	}

	t.Run("dynamically routed view image", func(t *testing.T) {
		registry, err := NewLocalToolRegistry(&ToolConfig{Enabled: []string{AskUserToolName}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		registry.SetViewImageVisionProvider(llm.NewMockProvider("vision"), "vision-model")
		if _, ok := registry.Get(ManageWorkspaceToolName); !ok {
			t.Fatal("manage_workspace absent after enabling routed view_image")
		}
	})
}

func TestManageWorkspaceListUsesGenericToolOutput(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeYolo)
	target := t.TempDir()
	tool := NewManageWorkspaceTool(mgr, &ToolConfig{BaseDir: target})
	grantArgs, _ := json.Marshal(manageWorkspaceArgs{Action: "grant", Path: target, Reason: "reference"})
	grantOut, err := tool.Execute(context.Background(), grantArgs)
	if err != nil || grantOut.IsError || !strings.Contains(grantOut.Content, "access=read") {
		t.Fatalf("grant output = %#v, %v", grantOut, err)
	}
	listOut, err := tool.Execute(context.Background(), json.RawMessage(`{"action":"list"}`))
	if err != nil || listOut.IsError || !strings.Contains(listOut.Content, target) || !strings.Contains(listOut.Content, "provenance=yolo") {
		t.Fatalf("list output = %#v, %v", listOut, err)
	}
}

func TestManageWorkspaceDeniedOutputIsError(t *testing.T) {
	mgr := NewApprovalManager(NewToolPermissions())
	mgr.SetApprovalMode(ModeAuto)
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: false, Rationale: "denied"}, nil
	}, nil)
	tool := NewManageWorkspaceTool(mgr, &ToolConfig{BaseDir: t.TempDir()})
	args, _ := json.Marshal(manageWorkspaceArgs{Action: "grant", Path: t.TempDir(), Access: "read", Reason: "reference"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil || !out.IsError {
		t.Fatalf("output = %#v, err=%v", out, err)
	}
}
