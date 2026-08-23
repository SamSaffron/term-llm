package tools

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	primaryWorkspaceID                      = "primary"
	primaryWorkspaceStatusProposed          = "proposed"
	primaryWorkspaceStatusConfirmed         = "confirmed"
	primaryWorkspaceProvenanceProposed      = "primary-proposed"
	primaryWorkspaceProvenanceConfirmed     = "human-confirmed-primary"
	primaryWorkspaceProvenanceMainInherited = "main-worktree-inherited-primary"
	workspaceProvenanceYolo                 = "yolo"

	primaryWorkspaceRationaleInheritedMain = "inherited confirmation from the Git main worktree; shell commands remain separately controlled"
)

// WorkspaceCapability is the combined list view of the proposed/confirmed
// primary workspace and additional session-scoped grants.
type WorkspaceCapability struct {
	ID         string
	Path       string
	Access     session.WorkspaceAccess
	Status     string
	Provenance string
	Rationale  string
	Primary    bool
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// WorkspaceGrantResult reports whether a grant was newly installed and whether
// it was durable. Stores without WorkspaceGrantStore intentionally remain
// runtime-only.
type WorkspaceGrantResult struct {
	Capability WorkspaceCapability
	Changed    bool
	Persisted  bool
}

// SetPrimaryWorkspace records a canonical proposed primary workspace. The
// proposal grants no file authority until the direct human confirmation path
// succeeds. Rebinding preserves additional grants and invalidates a mismatched
// primary confirmation.
func (m *ApprovalManager) SetPrimaryWorkspace(path string) error {
	return m.SetPrimaryWorkspaceWithContext(context.Background(), path)
}

// SetPrimaryWorkspaceWithContext is the context-aware rebinding path used by
// session/worktree controls.
func (m *ApprovalManager) SetPrimaryWorkspaceWithContext(ctx context.Context, path string) error {
	if m == nil {
		return nil
	}
	canonical, err := canonicalWorkspaceDirectory(path)
	if err != nil {
		return err
	}
	root := m.root()
	const maxStateRetries = 3
	for attempt := 0; attempt < maxStateRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		root.workspaceMu.RLock()
		oldProposal := root.primaryWorkspace
		loadedPrimary := root.primaryWorkspaceGrant
		store := root.workspaceStore
		sessionID := root.workspaceSessionID
		version := root.workspaceVersion
		root.workspaceMu.RUnlock()
		if oldProposal == canonical {
			return nil
		}

		inheritable := loadedPrimary.ID == primaryWorkspaceID &&
			loadedPrimary.Path == oldProposal &&
			loadedPrimary.Access == session.WorkspaceAccessWrite &&
			(loadedPrimary.Provenance == primaryWorkspaceProvenanceConfirmed || loadedPrimary.Provenance == primaryWorkspaceProvenanceMainInherited)
		inheritedMainApproval := inheritable && inheritsRegisteredMainWorktreeApproval(ctx, oldProposal, canonical, loadedPrimary.Provenance)

		// Git discovery deliberately happens before this lock so a slow repository
		// cannot block unrelated human approval prompts. Revalidate the complete
		// authority snapshot after acquiring it and again after persistence.
		promptLock := root.PromptLock()
		promptLock.Lock()
		root.workspaceMu.RLock()
		stable := root.workspaceVersion == version &&
			root.primaryWorkspace == oldProposal &&
			root.primaryWorkspaceGrant == loadedPrimary &&
			root.workspaceSessionID == sessionID
		root.workspaceMu.RUnlock()
		if !stable {
			promptLock.Unlock()
			continue
		}

		// A store may be attached before an explicit serve/session binding is
		// restored. Reuse only a matching persisted decision for that same binding.
		if oldProposal == "" && loadedPrimary.Path == canonical {
			root.workspaceMu.Lock()
			if root.workspaceVersion != version || root.primaryWorkspace != oldProposal || root.primaryWorkspaceGrant != loadedPrimary || root.workspaceSessionID != sessionID {
				root.workspaceMu.Unlock()
				promptLock.Unlock()
				continue
			}
			root.primaryWorkspace = canonical
			root.primaryWorkspaceDenied = false
			root.workspaceVersion++
			root.workspaceMu.Unlock()
			promptLock.Unlock()
			return nil
		}

		now := time.Now()
		decision := session.WorkspaceGrant{
			ID: primaryWorkspaceID, Path: canonical, Access: session.WorkspaceAccessWrite,
			Provenance: primaryWorkspaceProvenanceProposed,
			Rationale:  "pending direct human confirmation for the session primary workspace",
			CreatedAt:  now, UpdatedAt: now,
		}
		if inheritedMainApproval {
			decision.Provenance = primaryWorkspaceProvenanceMainInherited
			decision.Rationale = primaryWorkspaceRationaleInheritedMain
		}
		if loadedPrimary.ID == primaryWorkspaceID && !loadedPrimary.CreatedAt.IsZero() {
			decision.CreatedAt = loadedPrimary.CreatedAt
		}
		persisted := store != nil && strings.TrimSpace(sessionID) != ""
		if persisted {
			if err := store.SaveWorkspaceGrant(ctx, sessionID, decision); err != nil {
				promptLock.Unlock()
				if inheritedMainApproval {
					return fmt.Errorf("persist inherited primary workspace confirmation: %w", err)
				}
				return fmt.Errorf("persist proposed primary workspace: %w", err)
			}
		}

		root.workspaceMu.Lock()
		stable = root.workspaceVersion == version &&
			root.primaryWorkspace == oldProposal &&
			root.primaryWorkspaceGrant == loadedPrimary &&
			root.workspaceSessionID == sessionID
		if stable {
			root.primaryWorkspace = canonical
			root.primaryWorkspaceGrant = decision
			root.primaryWorkspaceDenied = false
			root.workspaceVersion++
		}
		root.workspaceMu.Unlock()
		promptLock.Unlock()
		if stable {
			return nil
		}
		if persisted {
			restorePersistedPrimaryDecision(ctx, store, sessionID, loadedPrimary)
		}
	}
	return fmt.Errorf("primary workspace authority changed repeatedly while rebinding to %s", canonical)
}

func restorePersistedPrimaryDecision(ctx context.Context, store session.WorkspaceGrantStore, sessionID string, previous session.WorkspaceGrant) {
	if store == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitCommandTimeout)
	defer cancel()
	if previous.ID == primaryWorkspaceID && previous.Path != "" {
		_ = store.SaveWorkspaceGrant(restoreCtx, sessionID, previous)
		return
	}
	_ = store.DeleteWorkspaceGrant(restoreCtx, sessionID, primaryWorkspaceID)
}

// ClearPrimaryWorkspace removes a proposal/confirmation without touching
// additional dynamic grants.
func (m *ApprovalManager) ClearPrimaryWorkspace(ctx context.Context) error {
	root := m.root()
	if root == nil {
		return nil
	}
	promptLock := root.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	root.workspaceMu.RLock()
	store := root.workspaceStore
	sessionID := root.workspaceSessionID
	root.workspaceMu.RUnlock()
	if store != nil && strings.TrimSpace(sessionID) != "" {
		if err := store.DeleteWorkspaceGrant(ctx, sessionID, primaryWorkspaceID); err != nil && !errors.Is(err, session.ErrNotFound) {
			return fmt.Errorf("clear persisted primary workspace: %w", err)
		}
	}
	root.workspaceMu.Lock()
	if root.primaryWorkspace != "" || root.primaryWorkspaceGrant.ID != "" || root.primaryWorkspaceDenied {
		root.primaryWorkspace = ""
		root.primaryWorkspaceGrant = session.WorkspaceGrant{}
		root.primaryWorkspaceDenied = false
		root.workspaceVersion++
	}
	root.workspaceMu.Unlock()
	return nil
}

// ConfigureWorkspacePersistence installs the optional durable grant backend and
// rehydrates primary confirmation plus additional grants before tools execute.
// Migration 46 needs no schema change: the stable reserved ID "primary"
// distinguishes the primary decision row from additional dynamic grants.
func (m *ApprovalManager) ConfigureWorkspacePersistence(ctx context.Context, store session.Store, sessionID string) error {
	root := m.root()
	if root == nil {
		return nil
	}
	if m.parent != nil {
		// Child runtimes have their own transcript/session IDs, but workspace
		// capabilities belong to the root session. Never let child setup retarget
		// the shared manager or rehydrate authority from the child row.
		root.workspaceMu.RLock()
		sessionID = root.workspaceSessionID
		root.workspaceMu.RUnlock()
		if strings.TrimSpace(sessionID) == "" {
			return nil
		}
	}
	var workspaceStore session.WorkspaceGrantStore
	if store != nil {
		workspaceStore, _ = session.AsWorkspaceGrantStore(store)
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		root.workspaceMu.RLock()
		sessionID = root.workspaceSessionID
		root.workspaceMu.RUnlock()
	}
	var grants []session.WorkspaceGrant
	var err error
	if workspaceStore != nil && sessionID != "" {
		grants, err = workspaceStore.ListWorkspaceGrants(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("restore workspace grants: %w", err)
		}
	}

	next := make(map[string]session.WorkspaceGrant, len(grants))
	var persistedPrimary session.WorkspaceGrant
	for _, grant := range grants {
		if strings.EqualFold(strings.TrimSpace(grant.Provenance), workspaceProvenanceYolo) {
			// Older versions could persist yolo grants. They never become runtime
			// authority again, and cleanup is deliberately best-effort so a stale
			// row cannot prevent an otherwise safe resume.
			_ = workspaceStore.DeleteWorkspaceGrant(context.WithoutCancel(ctx), sessionID, grant.ID)
			continue
		}
		canonical, canonicalErr := canonicalWorkspaceDirectory(grant.Path)
		if canonicalErr != nil {
			// A persisted path may disappear between sessions. Do not re-authorize a
			// different lexical path or silently weaken symlink safety.
			continue
		}
		grant.Path = canonical
		if grant.Access != session.WorkspaceAccessRead && grant.Access != session.WorkspaceAccessWrite {
			continue
		}
		if grant.ID == primaryWorkspaceID {
			if grant.Access == session.WorkspaceAccessWrite && (grant.Provenance == primaryWorkspaceProvenanceConfirmed || grant.Provenance == primaryWorkspaceProvenanceMainInherited || grant.Provenance == primaryWorkspaceProvenanceProposed) {
				persistedPrimary = grant
			}
			continue
		}
		next[canonical] = grant
	}

	root.workspaceMu.Lock()
	scopeChanged := root.workspaceSessionID != "" && sessionID != "" && root.workspaceSessionID != sessionID
	root.workspaceStore = workspaceStore
	root.workspaceSessionID = sessionID
	if workspaceStore != nil && sessionID != "" {
		root.workspaceGrants = next
		if scopeChanged {
			root.workspaceYoloGrants = make(map[string]session.WorkspaceGrant)
		}
		if root.primaryWorkspace == "" || persistedPrimary.Path == root.primaryWorkspace {
			root.primaryWorkspaceGrant = persistedPrimary
		} else {
			root.primaryWorkspaceGrant = session.WorkspaceGrant{}
		}
		if scopeChanged {
			root.primaryWorkspaceDenied = false
		}
	} else if scopeChanged {
		// A reusable manager must never carry runtime-only authority into another
		// session merely because that session store lacks the optional capability.
		root.workspaceGrants = make(map[string]session.WorkspaceGrant)
		root.workspaceYoloGrants = make(map[string]session.WorkspaceGrant)
		root.primaryWorkspaceGrant = session.WorkspaceGrant{}
		root.primaryWorkspaceDenied = false
	}
	// Every persistence reconfiguration invalidates authority decisions that may
	// be in flight, even when the session ID happens to remain unchanged.
	root.workspaceVersion++
	root.workspaceMu.Unlock()
	return nil
}

// clearYoloWorkspaceGrants ends the current yolo workspace epoch. Incrementing
// the version even when the overlay is empty invalidates grants whose yolo
// approval is concurrently in flight, so re-entering yolo cannot install work
// authorized by the previous epoch.
func (m *ApprovalManager) clearYoloWorkspaceGrants() {
	root := m.root()
	if root == nil {
		return
	}
	root.workspaceMu.Lock()
	root.workspaceYoloGrants = make(map[string]session.WorkspaceGrant)
	root.workspaceVersion++
	root.workspaceMu.Unlock()
}

func (m *ApprovalManager) primaryWorkspaceConfirmedLocked() bool {
	root := m.root()
	provenance := root.primaryWorkspaceGrant.Provenance
	return root.primaryWorkspace != "" &&
		root.primaryWorkspaceGrant.ID == primaryWorkspaceID &&
		root.primaryWorkspaceGrant.Path == root.primaryWorkspace &&
		root.primaryWorkspaceGrant.Access == session.WorkspaceAccessWrite &&
		(provenance == primaryWorkspaceProvenanceConfirmed || provenance == primaryWorkspaceProvenanceMainInherited)
}

// ErrWorkspaceApprovalCancelled reports that a proactive workspace prompt was
// dismissed without making an authority decision.
var ErrWorkspaceApprovalCancelled = errors.New("workspace approval cancelled")

// EnsurePrimaryWorkspaceAccess proactively confirms the proposed primary
// workspace through the same direct-human boundary used by the first path tool
// access. Remembered workspaces and already-confirmed sessions return without a
// visible prompt; yolo remains an in-memory bypass and never persists trust.
func (m *ApprovalManager) EnsurePrimaryWorkspaceAccess(ctx context.Context) error {
	if m == nil || m.YoloEnabled() {
		return nil
	}
	root := m.root()
	if root == nil {
		return nil
	}
	root.workspaceMu.RLock()
	proposal := root.primaryWorkspace
	root.workspaceMu.RUnlock()
	if proposal == "" {
		return nil
	}
	return m.ensurePrimaryWorkspaceAccess(ctx, proposal, false)
}

// ensurePrimaryWorkspaceAccess performs the authority-boundary confirmation
// outside yolo and before every other approval mechanism. The shared root prompt
// lock gives concurrent parent/child first accesses one human prompt and one result.
// Proactive prompts leave cancellation undecided so first access can ask again.
func (m *ApprovalManager) ensurePrimaryWorkspaceAccess(ctx context.Context, canonicalPath string, latchCancellation bool) error {
	root := m.root()
	if root == nil {
		return nil
	}
	root.workspaceMu.RLock()
	proposal := root.primaryWorkspace
	confirmed := root.primaryWorkspaceConfirmedLocked()
	denied := root.primaryWorkspaceDenied
	trustStore := root.workspaceTrustStore
	root.workspaceMu.RUnlock()
	if proposal == "" || !pathWithinWorkspace(canonicalPath, proposal) || confirmed {
		return nil
	}
	if denied {
		return NewToolErrorf(ErrPermissionDenied, "proposed primary workspace %s was denied by the human; explicit workspace read/write confirmation is required", proposal)
	}

	// Remembered-workspace lookup may query Git. Keep it outside the shared
	// prompt lock so repository I/O cannot block unrelated human approvals.
	remembered := false
	if trustStore != nil {
		var err error
		remembered, err = trustStore.IsTrusted(ctx, proposal)
		if err != nil {
			// An unreadable ledger grants no authority, but it must not disable the
			// existing direct-human confirmation path. Remember writes still fail
			// closed below because the user explicitly requested durable approval.
			remembered = false
			if m.DebugApproval {
				log.Printf("[approval] remembered workspace lookup failed for %q: %v", proposal, err)
			}
		}
	}

	promptLock := root.PromptLock()
	promptLock.Lock()
	defer promptLock.Unlock()

	lookedUpProposal := proposal
	root.workspaceMu.RLock()
	proposal = root.primaryWorkspace
	confirmed = root.primaryWorkspaceConfirmedLocked()
	denied = root.primaryWorkspaceDenied
	version := root.workspaceVersion
	trustStore = root.workspaceTrustStore
	root.workspaceMu.RUnlock()
	if proposal == "" || !pathWithinWorkspace(canonicalPath, proposal) || confirmed {
		return nil
	}
	if denied {
		return NewToolErrorf(ErrPermissionDenied, "proposed primary workspace %s was denied by the human; explicit workspace read/write confirmation is required", proposal)
	}
	if proposal != lookedUpProposal {
		return NewToolError(ErrPermissionDenied, "primary workspace changed during remembered approval lookup; access denied")
	}

	if !remembered {
		prompt := m.lookupWorkspacePromptFunc()
		if prompt == nil {
			return NewToolErrorf(ErrPermissionDenied, "proposed primary workspace %s requires explicit human read/write confirmation, but no workspace approval transport is available", proposal)
		}
		result, err := prompt(proposal)
		if err != nil {
			return NewToolErrorf(ErrPermissionDenied, "proposed primary workspace %s requires explicit human read/write confirmation: %v", proposal, err)
		}
		if result.Cancelled && !latchCancellation {
			return ErrWorkspaceApprovalCancelled
		}
		if !result.Approved || result.Cancelled {
			root.workspaceMu.Lock()
			if root.workspaceVersion == version && root.primaryWorkspace == proposal && !root.primaryWorkspaceConfirmedLocked() {
				root.primaryWorkspaceDenied = true
				root.workspaceVersion++
			}
			root.workspaceMu.Unlock()
			return NewToolErrorf(ErrPermissionDenied, "human denied read/write access to proposed primary workspace %s", proposal)
		}
		if result.Remember {
			if trustStore == nil {
				return NewToolError(ErrPermissionDenied, "remembered workspace approval is unavailable")
			}
			if err := trustStore.Remember(proposal); err != nil {
				return NewToolErrorf(ErrPermissionDenied, "remember primary workspace %s: %v", proposal, err)
			}
			remembered = true
		}
	}

	now := time.Now()
	rationale := "direct human confirmation for this session; shell commands remain separately controlled"
	if remembered {
		rationale = "remembered direct human confirmation; shell commands remain separately controlled"
	}
	grant := session.WorkspaceGrant{
		ID: primaryWorkspaceID, Path: proposal, Access: session.WorkspaceAccessWrite,
		Provenance: primaryWorkspaceProvenanceConfirmed,
		Rationale:  rationale,
		CreatedAt:  now, UpdatedAt: now,
	}
	root.workspaceMu.RLock()
	if root.primaryWorkspaceGrant.ID == primaryWorkspaceID && !root.primaryWorkspaceGrant.CreatedAt.IsZero() {
		grant.CreatedAt = root.primaryWorkspaceGrant.CreatedAt
	}
	root.workspaceMu.RUnlock()
	store, sessionID := root.workspacePersistence(ctx)
	if store != nil && sessionID != "" {
		if err := store.SaveWorkspaceGrant(ctx, sessionID, grant); err != nil {
			return fmt.Errorf("persist primary workspace confirmation: %w", err)
		}
	}

	root.workspaceMu.Lock()
	if root.workspaceVersion != version || root.primaryWorkspace != proposal {
		root.workspaceMu.Unlock()
		if store != nil && sessionID != "" {
			_ = store.DeleteWorkspaceGrant(context.WithoutCancel(ctx), sessionID, primaryWorkspaceID)
		}
		return NewToolError(ErrPermissionDenied, "primary workspace changed while confirmation was pending; access denied")
	}
	root.primaryWorkspaceGrant = grant
	root.primaryWorkspaceDenied = false
	root.workspaceVersion++
	root.workspaceMu.Unlock()
	return nil
}

// IsWorkspacePathAllowed performs a race-safe, boundary-safe capability check.
// It never consults shell permissions and therefore cannot authorize commands.
func (m *ApprovalManager) IsWorkspacePathAllowed(path string, isWrite bool) bool {
	root := m.root()
	if root == nil {
		return false
	}
	canonical, err := canonicalApprovalPath(path, isWrite)
	if err != nil {
		return false
	}
	root.workspaceMu.RLock()
	primary := root.primaryWorkspace
	primaryConfirmed := root.primaryWorkspaceConfirmedLocked()
	yoloGrants := make([]session.WorkspaceGrant, 0, len(root.workspaceYoloGrants))
	for _, grant := range root.workspaceYoloGrants {
		yoloGrants = append(yoloGrants, grant)
	}
	grants := make([]session.WorkspaceGrant, 0, len(root.workspaceGrants))
	for _, grant := range root.workspaceGrants {
		grants = append(grants, grant)
	}
	root.workspaceMu.RUnlock()
	if primaryConfirmed && pathWithinWorkspace(canonical, primary) {
		return true
	}
	for _, grantSet := range [][]session.WorkspaceGrant{yoloGrants, grants} {
		for _, grant := range grantSet {
			if isWrite && grant.Access != session.WorkspaceAccessWrite {
				continue
			}
			if pathWithinWorkspace(canonical, grant.Path) {
				return true
			}
		}
	}
	return false
}

// WorkspaceCapabilities returns a deterministic snapshot shared by this manager
// and every descendant. A proposed primary is visible but is not authority.
func (m *ApprovalManager) WorkspaceCapabilities() []WorkspaceCapability {
	root := m.root()
	if root == nil {
		return nil
	}
	root.workspaceMu.RLock()
	capabilities := make([]WorkspaceCapability, 0, len(root.workspaceGrants)+len(root.workspaceYoloGrants)+1)
	if root.primaryWorkspace != "" {
		status := primaryWorkspaceStatusProposed
		provenance := primaryWorkspaceProvenanceProposed
		rationale := "pending direct human confirmation"
		createdAt := time.Time{}
		updatedAt := time.Time{}
		if root.primaryWorkspaceConfirmedLocked() {
			status = primaryWorkspaceStatusConfirmed
			provenance = root.primaryWorkspaceGrant.Provenance
			rationale = root.primaryWorkspaceGrant.Rationale
			createdAt = root.primaryWorkspaceGrant.CreatedAt
			updatedAt = root.primaryWorkspaceGrant.UpdatedAt
		}
		capabilities = append(capabilities, WorkspaceCapability{
			ID: primaryWorkspaceID, Path: root.primaryWorkspace, Access: session.WorkspaceAccessWrite,
			Status: status, Provenance: provenance, Rationale: rationale, Primary: true,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		})
	}
	effective := make(map[string]session.WorkspaceGrant, len(root.workspaceGrants)+len(root.workspaceYoloGrants))
	for path, grant := range root.workspaceGrants {
		effective[path] = grant
	}
	// The yolo map is an effective runtime overlay. Keeping the durable baseline
	// separate lets write elevation disappear without weakening an existing read
	// grant when yolo ends.
	for path, grant := range root.workspaceYoloGrants {
		effective[path] = grant
	}
	for _, grant := range effective {
		capabilities = append(capabilities, capabilityFromGrant(grant))
	}
	root.workspaceMu.RUnlock()
	sort.Slice(capabilities, func(i, j int) bool {
		if capabilities[i].Primary != capabilities[j].Primary {
			return capabilities[i].Primary
		}
		if capabilities[i].Path != capabilities[j].Path {
			return capabilities[i].Path < capabilities[j].Path
		}
		return capabilities[i].ID < capabilities[j].ID
	})
	return capabilities
}

func capabilityFromGrant(grant session.WorkspaceGrant) WorkspaceCapability {
	return WorkspaceCapability{
		ID: grant.ID, Path: grant.Path, Access: grant.Access, Status: primaryWorkspaceStatusConfirmed, Provenance: grant.Provenance,
		Rationale: grant.Rationale, CreatedAt: grant.CreatedAt, UpdatedAt: grant.UpdatedAt,
	}
}

// GrantWorkspace validates, reviews, persists, and then installs an additional
// workspace capability. Callers must pass the already canonical narrow project
// root returned by CanonicalWorkspaceRoot.
func (m *ApprovalManager) GrantWorkspace(ctx context.Context, canonicalPath string, access session.WorkspaceAccess, reason string) (WorkspaceGrantResult, error) {
	root := m.root()
	if root == nil {
		return WorkspaceGrantResult{}, NewToolError(ErrPermissionDenied, "workspace approval manager is unavailable")
	}
	canonicalPath, err := canonicalWorkspaceDirectory(canonicalPath)
	if err != nil {
		return WorkspaceGrantResult{}, err
	}
	if access != session.WorkspaceAccessRead && access != session.WorkspaceAccessWrite {
		return WorkspaceGrantResult{}, NewToolErrorf(ErrInvalidParams, "workspace access must be read or write, got %q", access)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return WorkspaceGrantResult{}, NewToolError(ErrInvalidParams, "workspace grant reason is required")
	}

	if m.parent == nil {
		m.BindWorkspaceSessionID(llm.SessionIDFromContext(ctx))
	}
	existing, baseline, yoloOverlay, version, idempotent := root.workspaceGrantState(canonicalPath, access)
	if idempotent {
		return WorkspaceGrantResult{Capability: existing, Persisted: existing.Provenance != workspaceProvenanceYolo && root.workspaceGrantCanPersist(ctx)}, nil
	}
	root.workspaceMu.RLock()
	proposal := root.primaryWorkspace
	primaryConfirmed := root.primaryWorkspaceConfirmedLocked()
	root.workspaceMu.RUnlock()
	if proposal != "" && !primaryConfirmed && (pathWithinWorkspace(canonicalPath, proposal) || pathWithinWorkspace(proposal, canonicalPath)) {
		return WorkspaceGrantResult{}, NewToolErrorf(ErrPermissionDenied, "the proposed primary workspace %s can only be confirmed by the human on first file/path access", proposal)
	}

	provenance, err := m.approveWorkspaceGrant(ctx, canonicalPath, access, reason)
	if err != nil {
		return WorkspaceGrantResult{}, err
	}

	// Yolo grants are an in-memory overlay, never a mutation of the durable
	// baseline. In particular, a temporary write elevation gets its own ID and
	// leaves an existing durable read grant untouched.
	prior := baseline
	if provenance == workspaceProvenanceYolo {
		prior = yoloOverlay
	}
	now := time.Now()
	grant := session.WorkspaceGrant{
		ID: session.NewID(), Path: canonicalPath, Access: access, Provenance: provenance,
		Rationale: reason, CreatedAt: now, UpdatedAt: now,
	}
	if prior.ID != "" {
		grant.ID = prior.ID
		grant.CreatedAt = prior.CreatedAt
		if grant.CreatedAt.IsZero() {
			grant.CreatedAt = now
		}
	}

	store, sessionID := root.workspacePersistence(ctx)
	persisted := false
	if provenance != workspaceProvenanceYolo && store != nil && sessionID != "" {
		if err := store.SaveWorkspaceGrant(ctx, sessionID, grant); err != nil {
			return WorkspaceGrantResult{}, fmt.Errorf("persist workspace grant: %w", err)
		}
		persisted = true
	}

	root.workspaceMu.Lock()
	if root.workspaceVersion != version {
		if current, _, ok := root.workspaceGrantStateLocked(canonicalPath, access); ok {
			currentPersisted := current.Provenance != workspaceProvenanceYolo && store != nil && sessionID != ""
			root.workspaceMu.Unlock()
			return WorkspaceGrantResult{Capability: current, Persisted: currentPersisted}, nil
		}
		root.workspaceMu.Unlock()
		if persisted {
			_ = store.DeleteWorkspaceGrant(context.WithoutCancel(ctx), sessionID, grant.ID)
		}
		return WorkspaceGrantResult{}, NewToolError(ErrPermissionDenied, "workspace grants changed while approval was pending; request the grant again")
	}
	if provenance == workspaceProvenanceYolo {
		root.workspaceYoloGrants[canonicalPath] = grant
	} else {
		root.workspaceGrants[canonicalPath] = grant
	}
	root.workspaceVersion++
	root.workspaceMu.Unlock()
	return WorkspaceGrantResult{Capability: capabilityFromGrant(grant), Changed: true, Persisted: persisted}, nil
}

func (m *ApprovalManager) workspaceGrantState(path string, access session.WorkspaceAccess) (WorkspaceCapability, session.WorkspaceGrant, session.WorkspaceGrant, uint64, bool) {
	root := m.root()
	root.workspaceMu.RLock()
	defer root.workspaceMu.RUnlock()
	capability, _, ok := root.workspaceGrantStateLocked(path, access)
	return capability, root.workspaceGrants[path], root.workspaceYoloGrants[path], root.workspaceVersion, ok
}

func (m *ApprovalManager) workspaceGrantStateLocked(path string, access session.WorkspaceAccess) (WorkspaceCapability, session.WorkspaceGrant, bool) {
	root := m.root()
	if root.primaryWorkspaceConfirmedLocked() && pathWithinWorkspace(path, root.primaryWorkspace) {
		capability := WorkspaceCapability{ID: primaryWorkspaceID, Path: root.primaryWorkspace, Access: session.WorkspaceAccessWrite, Status: primaryWorkspaceStatusConfirmed, Provenance: primaryWorkspaceProvenanceConfirmed, Rationale: root.primaryWorkspaceGrant.Rationale, Primary: true}
		return capability, session.WorkspaceGrant{}, true
	}
	for _, grants := range []map[string]session.WorkspaceGrant{root.workspaceYoloGrants, root.workspaceGrants} {
		grant, exact := grants[path]
		if exact {
			if access == session.WorkspaceAccessWrite && grant.Access != session.WorkspaceAccessWrite {
				continue
			}
			return capabilityFromGrant(grant), grant, true
		}
	}
	for _, grants := range []map[string]session.WorkspaceGrant{root.workspaceYoloGrants, root.workspaceGrants} {
		for _, covering := range grants {
			if !pathWithinWorkspace(path, covering.Path) {
				continue
			}
			if access == session.WorkspaceAccessWrite && covering.Access != session.WorkspaceAccessWrite {
				continue
			}
			return capabilityFromGrant(covering), covering, true
		}
	}
	baseline, exact := root.workspaceGrants[path]
	if exact {
		return capabilityFromGrant(baseline), baseline, false
	}
	return WorkspaceCapability{}, session.WorkspaceGrant{}, false
}

func (m *ApprovalManager) approveWorkspaceGrant(ctx context.Context, path string, access session.WorkspaceAccess, reason string) (string, error) {
	isWrite := access == session.WorkspaceAccessWrite
	for {
		switch m.ApprovalMode() {
		case ModeYolo:
			return "yolo", nil
		case ModeAuto:
			if err := m.reviewWorkspaceGrant(ctx, path, access, reason); err != nil {
				return "", err
			}
			return "guardian", nil
		default:
			lock := m.PromptLock()
			lock.Lock()
			// Re-evaluate after waiting so a mode toggle takes effect atomically with
			// the prompt transport.
			if m.ApprovalMode() != ModePrompt {
				lock.Unlock()
				continue
			}
			prompt := m.lookupPromptUIFunc()
			if prompt != nil {
				result, err := prompt(path, isWrite, false, "")
				lock.Unlock()
				if err != nil {
					return "", err
				}
				if result.Cancelled || result.Choice == ApprovalChoiceDeny {
					return "", NewToolError(ErrPermissionDenied, "workspace grant denied by user")
				}
				return "user", nil
			}
			legacy := m.lookupPromptFunc()
			if legacy == nil {
				lock.Unlock()
				return "", NewToolError(ErrPermissionDenied, "workspace grant requires approval but no approval UI is available")
			}
			outcome, _ := legacy(&ApprovalRequest{ToolName: ManageWorkspaceToolName, Path: path, Description: fmt.Sprintf("Allow %s workspace access: %s", access, path), ToolInfo: reason})
			lock.Unlock()
			if outcome != ProceedOnce && outcome != ProceedAlways && outcome != ProceedAlwaysAndSave {
				return "", NewToolError(ErrPermissionDenied, "workspace grant denied by user")
			}
			return "user", nil
		}
	}
}

func (m *ApprovalManager) reviewWorkspaceGrant(ctx context.Context, path string, access session.WorkspaceAccess, reason string) error {
	reviewFunc := m.lookupPolicyReviewFunc()
	isWrite := access == session.WorkspaceAccessWrite
	if reviewFunc == nil {
		detail := "guardian policy reviewer is not configured"
		m.emitGuardianEventForContext(ctx, m.guardianPathEvent(ctx, ManageWorkspaceToolName, path, isWrite, GuardianWarning, "guardian: auto mode unavailable (no reviewer configured); workspace grant denied"))
		return guardianReviewFailure(detail)
	}
	existing := m.workspaceApprovalContext()
	_, scopeID := m.workspacePersistence(ctx)
	decision, err := reviewFunc(ctx, PolicyReviewRequest{
		ToolName: ManageWorkspaceToolName, Path: path, IsWrite: isWrite, IsDirectory: true,
		Transcript: approvalTranscriptFromContext(ctx), ApprovalContext: existing,
		ScopeID: scopeID, WorkspaceAccess: string(access), Reason: reason,
	})
	if err != nil {
		m.emitGuardianEventForContext(ctx, m.guardianPathDecisionEvent(ctx, ManageWorkspaceToolName, path, isWrite, GuardianError, fmt.Sprintf("guardian: workspace review failed (%v); grant denied", err), decision))
		return guardianReviewFailure(err.Error())
	}
	if decision.Allowed && guardianAllowContradictsPolicy(decision) {
		rationale := guardianDenialRationale(decision, "guardian allow contradicted policy risk/authorization fields")
		trip := m.recordGuardianDenial()
		m.emitGuardianEventForContext(ctx, m.guardianPathDecisionEvent(ctx, ManageWorkspaceToolName, path, isWrite, GuardianDenied, "guardian: workspace grant denied: "+rationale, decision))
		m.applyGuardianBreakerTrip(trip)
		return guardianPolicyDenial(rationale)
	}
	if !decision.Allowed {
		rationale := guardianDenialRationale(decision, "workspace grant was not approved by guardian policy")
		trip := m.recordGuardianDenial()
		m.emitGuardianEventForContext(ctx, m.guardianPathDecisionEvent(ctx, ManageWorkspaceToolName, path, isWrite, GuardianDenied, "guardian: workspace grant denied: "+rationale, decision))
		m.applyGuardianBreakerTrip(trip)
		return guardianPolicyDenial(rationale)
	}
	m.resetGuardianDenials()
	m.emitGuardianEventForContext(ctx, m.guardianPathDecisionEvent(ctx, ManageWorkspaceToolName, path, isWrite, GuardianApproved, "guardian: "+formatGuardianApproval(decision), decision))
	return nil
}

func (m *ApprovalManager) workspaceApprovalContext() string {
	var b strings.Builder
	b.WriteString("workspace_scope=\"current_session\"\n")
	for _, capability := range m.WorkspaceCapabilities() {
		fmt.Fprintf(&b, "existing_workspace id=%q path=%q access=%q status=%q provenance=%q primary=%t\n", capability.ID, capability.Path, capability.Access, capability.Status, capability.Provenance, capability.Primary)
	}
	b.WriteString("Workspace capabilities authorize local file tools only. They never authorize shell commands, network access, MCP actions, or project/global approval records.\n")
	return b.String()
}

// RevokeWorkspace removes an additional grant by stable ID or exact canonical
// path. A yolo overlay is removed without touching any durable baseline beneath
// it. The proposed/confirmed primary can only change through direct session
// controls, never through the model-facing tool.
func (m *ApprovalManager) RevokeWorkspace(ctx context.Context, selector string) (WorkspaceCapability, bool, error) {
	root := m.root()
	if root == nil {
		return WorkspaceCapability{}, false, nil
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return WorkspaceCapability{}, false, NewToolError(ErrInvalidParams, "grant_id or path is required")
	}

	root.workspaceMu.RLock()
	primary := root.primaryWorkspace
	var found session.WorkspaceGrant
	foundYolo := false
	for _, grant := range root.workspaceYoloGrants {
		if selector == grant.ID || selector == grant.Path {
			found = grant
			foundYolo = true
			break
		}
	}
	if found.ID == "" {
		for _, grant := range root.workspaceGrants {
			if selector == grant.ID || selector == grant.Path {
				found = grant
				break
			}
		}
	}
	root.workspaceMu.RUnlock()
	if selector == primary || selector == primaryWorkspaceID {
		return WorkspaceCapability{}, false, NewToolError(ErrPermissionDenied, "the primary workspace cannot be revoked; rebind the session workspace instead")
	}
	if found.ID == "" {
		if canonical, err := canonicalWorkspaceDirectory(selector); err == nil {
			root.workspaceMu.RLock()
			found, foundYolo = root.workspaceYoloGrants[canonical]
			if !foundYolo {
				found = root.workspaceGrants[canonical]
			}
			primary = root.primaryWorkspace
			root.workspaceMu.RUnlock()
			if canonical == primary {
				return WorkspaceCapability{}, false, NewToolError(ErrPermissionDenied, "the primary workspace cannot be revoked; rebind the session workspace instead")
			}
		}
	}
	if found.ID == "" {
		// Also invalidate approvals currently in flight. If a matching durable grant
		// was persisted just before this revoke but not yet installed, its grant path
		// observes the version change and removes that row.
		root.workspaceMu.Lock()
		root.workspaceVersion++
		root.workspaceMu.Unlock()
		return WorkspaceCapability{}, false, nil
	}

	store, sessionID := root.workspacePersistence(ctx)
	if !foundYolo && store != nil && sessionID != "" {
		if err := store.DeleteWorkspaceGrant(ctx, sessionID, found.ID); err != nil && !errors.Is(err, session.ErrNotFound) {
			return WorkspaceCapability{}, false, fmt.Errorf("persist workspace revocation: %w", err)
		}
	}
	root.workspaceMu.Lock()
	grants := root.workspaceGrants
	if foundYolo {
		grants = root.workspaceYoloGrants
	}
	if current, ok := grants[found.Path]; ok && current.ID == found.ID {
		delete(grants, found.Path)
		root.workspaceVersion++
		root.workspaceMu.Unlock()
		return capabilityFromGrant(found), true, nil
	}
	root.workspaceMu.Unlock()
	return capabilityFromGrant(found), false, nil
}

func (m *ApprovalManager) workspacePersistence(ctx context.Context) (session.WorkspaceGrantStore, string) {
	root := m.root()
	root.workspaceMu.RLock()
	store := root.workspaceStore
	sessionID := root.workspaceSessionID
	root.workspaceMu.RUnlock()
	if sessionID == "" {
		sessionID = strings.TrimSpace(llm.SessionIDFromContext(ctx))
	}
	return store, strings.TrimSpace(sessionID)
}

// BindWorkspaceSessionID records the owning root session once it becomes known.
// Child agent contexts cannot replace an existing root owner.
func (m *ApprovalManager) BindWorkspaceSessionID(sessionID string) {
	root := m.root()
	sessionID = strings.TrimSpace(sessionID)
	if root == nil || sessionID == "" {
		return
	}
	root.workspaceMu.Lock()
	if root.workspaceSessionID == "" {
		root.workspaceSessionID = sessionID
		root.workspaceVersion++
	}
	root.workspaceMu.Unlock()
}

func (m *ApprovalManager) workspaceGrantCanPersist(ctx context.Context) bool {
	store, sessionID := m.workspacePersistence(ctx)
	return store != nil && sessionID != ""
}

// CanonicalWorkspaceRoot resolves symlinks, rejects missing/non-directory
// targets, and narrows paths inside Git repositories to that repository's own
// worktree root (never a broad shared parent/common-dir root).
func CanonicalWorkspaceRoot(path, baseDir string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", NewToolError(ErrInvalidParams, "workspace path is required")
	}
	if !filepath.IsAbs(path) && strings.TrimSpace(baseDir) != "" {
		path = filepath.Join(baseDir, path)
	}
	canonical, err := canonicalWorkspaceDirectory(path)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = canonical
	if output, gitErr := cmd.Output(); gitErr == nil {
		root := strings.TrimSpace(string(output))
		if root != "" {
			root, err = canonicalWorkspaceDirectory(root)
			if err != nil {
				return "", err
			}
			if pathWithinWorkspace(canonical, root) {
				return root, nil
			}
		}
	}
	return canonical, nil
}

func canonicalWorkspaceDirectory(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", NewToolError(ErrInvalidParams, "workspace path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "resolve workspace path %q: %v", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "workspace path %q is not accessible: %v", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", NewToolErrorf(ErrInvalidParams, "workspace path %q is not accessible: %v", resolved, err)
	}
	if !info.IsDir() {
		return "", NewToolErrorf(ErrInvalidParams, "workspace path %q is not a directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func pathWithinWorkspace(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
