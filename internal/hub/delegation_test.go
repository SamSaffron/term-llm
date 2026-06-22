package hub

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDelegationPolicyDefaults(t *testing.T) {
	n := Node{ID: "alpha"}
	if n.CanDelegateTo("beta") {
		t.Fatalf("node without a policy must not originate delegations")
	}
	if err := n.AcceptsDelegationFrom("beta"); err == nil {
		t.Fatalf("node without a delegation workdir must not accept delegations")
	}
	if got := n.DelegationMaxInFlight(); got != DefaultDelegationMaxInFlight {
		t.Fatalf("DelegationMaxInFlight = %d, want %d", got, DefaultDelegationMaxInFlight)
	}
}

func TestDelegationPolicyAcceptRequiresWorkdir(t *testing.T) {
	n := Node{ID: "alpha", Delegation: &DelegationPolicy{Enabled: true, AcceptFrom: []string{"*"}}}
	if err := n.AcceptsDelegationFrom("beta"); err == nil {
		t.Fatalf("accept_from without workdir must still deny")
	}
	n.Delegation.Workdir = "/work"
	if err := n.AcceptsDelegationFrom("beta"); err != nil {
		t.Fatalf("workdir + accept_from * should accept: %v", err)
	}
}

func TestDelegationPolicyMatching(t *testing.T) {
	n := Node{ID: "alpha", Delegation: &DelegationPolicy{
		Enabled:    true,
		To:         []string{"beta"},
		AcceptFrom: []string{"gamma"},
		Workdir:    "/work",
	}}
	if !n.CanDelegateTo("beta") {
		t.Fatalf("explicit to entry should allow")
	}
	if n.CanDelegateTo("gamma") {
		t.Fatalf("node not in to list should be denied")
	}
	if err := n.AcceptsDelegationFrom("gamma"); err != nil {
		t.Fatalf("explicit accept_from entry should accept: %v", err)
	}
	if err := n.AcceptsDelegationFrom("beta"); err == nil {
		t.Fatalf("origin not in accept_from should be denied")
	}
	// Workdir set with empty accept_from defaults to accepting any node.
	open := Node{ID: "alpha", Delegation: &DelegationPolicy{Enabled: true, Workdir: "/work"}}
	if err := open.AcceptsDelegationFrom("anyone"); err != nil {
		t.Fatalf("workdir without accept_from should default to *: %v", err)
	}
}

func TestDelegationStatusTerminal(t *testing.T) {
	for _, status := range []string{DelegationStatusSucceeded, DelegationStatusFailed, DelegationStatusCancelled, DelegationStatusTimedOut, DelegationStatusError} {
		if !DelegationStatusTerminal(status) {
			t.Fatalf("%s should be terminal", status)
		}
	}
	for _, status := range []string{DelegationStatusPending, DelegationStatusRunning, DelegationStatusCancelRequested, ""} {
		if DelegationStatusTerminal(status) {
			t.Fatalf("%s should not be terminal", status)
		}
	}
}

func newTestDelegation(id, origin, target, status string, created time.Time) Delegation {
	return Delegation{
		ID:         id,
		OriginNode: origin,
		TargetNode: target,
		Status:     status,
		Depth:      1,
		Chain:      []string{origin, target},
		CreatedAt:  created,
		UpdatedAt:  created,
	}
}

func TestDelegationStoreCRUD(t *testing.T) {
	s := NewDelegationStore(filepath.Join(t.TempDir(), "ledger", "delegations.json"))
	now := time.Now().UTC()

	if err := s.Add(newTestDelegation("dlg_1", "a", "b", DelegationStatusRunning, now)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(newTestDelegation("dlg_1", "a", "b", DelegationStatusRunning, now)); err == nil {
		t.Fatalf("duplicate Add should fail")
	}
	if err := s.Add(newTestDelegation("dlg_2", "a", "c", DelegationStatusRunning, now.Add(time.Second))); err != nil {
		t.Fatalf("Add second: %v", err)
	}

	d, ok, err := s.Get("dlg_1")
	if err != nil || !ok {
		t.Fatalf("Get dlg_1: ok=%v err=%v", ok, err)
	}
	if d.TargetNode != "b" {
		t.Fatalf("TargetNode = %q", d.TargetNode)
	}

	updated, err := s.Update("dlg_1", func(rec *Delegation) {
		rec.Status = DelegationStatusSucceeded
		rec.Response = "done"
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Status != DelegationStatusSucceeded || updated.Response != "done" {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := s.Update("dlg_missing", func(*Delegation) {}); err == nil {
		t.Fatalf("Update on unknown id should fail")
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != "dlg_2" {
		t.Fatalf("List should be newest first, got %+v", list)
	}

	total, byOrigin, byTarget, err := s.ActiveCounts()
	if err != nil {
		t.Fatalf("ActiveCounts: %v", err)
	}
	if total != 1 || byOrigin["a"] != 1 || byTarget["c"] != 1 {
		t.Fatalf("ActiveCounts = %d %v %v", total, byOrigin, byTarget)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(s.Path())
		if err != nil {
			t.Fatalf("stat ledger: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("ledger permissions = %o, want 600", perm)
		}
	}
}

func TestDelegationStoreAtomicWriteFailureLeavesExistingLedgerUntouched(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions are not reliable on windows")
	}

	dir := filepath.Join(t.TempDir(), "ledger")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir ledger dir: %v", err)
	}
	s := NewDelegationStore(filepath.Join(dir, "delegations.json"))
	now := time.Now().UTC()
	if err := s.Add(newTestDelegation("dlg_1", "a", "b", DelegationStatusRunning, now)); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	original, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("read seeded ledger: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod ledger dir: %v", err)
	}
	defer os.Chmod(dir, 0o700)

	_, err = s.Update("dlg_1", func(rec *Delegation) {
		rec.Status = DelegationStatusSucceeded
		rec.Response = "done"
	})
	if err == nil {
		t.Skip("ledger directory remained writable; cannot exercise atomic temp-file creation failure")
	}
	if !strings.Contains(err.Error(), "create temp file") && !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected atomic temp-file creation failure, got: %v", err)
	}

	current, readErr := os.ReadFile(s.Path())
	if readErr != nil {
		t.Fatalf("read ledger after failed update: %v", readErr)
	}
	if string(current) != string(original) {
		t.Fatalf("ledger was modified after failed atomic write:\n got: %q\nwant: %q", string(current), string(original))
	}

	got, ok, err := s.Get("dlg_1")
	if err != nil {
		t.Fatalf("Get after failed update: %v", err)
	}
	if !ok {
		t.Fatalf("delegation missing after failed update")
	}
	if got.Status != DelegationStatusRunning || got.Response != "" {
		t.Fatalf("delegation changed after failed update: %+v", got)
	}

	leftovers, err := filepath.Glob(filepath.Join(dir, ".delegations.json.*.tmp"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("unexpected temp files left behind: %v", leftovers)
	}
}

func TestDelegationStorePrune(t *testing.T) {
	s := NewDelegationStore(filepath.Join(t.TempDir(), "delegations.json"))
	now := time.Now().UTC()
	s.now = func() time.Time { return now }

	old := now.Add(-delegationRetention - time.Hour)
	stale := newTestDelegation("dlg_old", "a", "b", DelegationStatusSucceeded, old)
	stale.UpdatedAt = old
	if err := s.Add(stale); err != nil {
		t.Fatalf("Add stale: %v", err)
	}
	oldActive := newTestDelegation("dlg_old_active", "a", "b", DelegationStatusRunning, old)
	oldActive.UpdatedAt = old
	if err := s.Add(oldActive); err != nil {
		t.Fatalf("Add old active: %v", err)
	}
	// Adding a fresh record triggers a prune pass on write.
	if err := s.Add(newTestDelegation("dlg_new", "a", "b", DelegationStatusRunning, now)); err != nil {
		t.Fatalf("Add fresh: %v", err)
	}

	if _, ok, _ := s.Get("dlg_old"); ok {
		t.Fatalf("terminal record past retention should be pruned")
	}
	if _, ok, _ := s.Get("dlg_old_active"); !ok {
		t.Fatalf("active record must never be pruned")
	}
	if _, ok, _ := s.Get("dlg_new"); !ok {
		t.Fatalf("fresh record should remain")
	}
}

func TestDelegationStorePromptTruncation(t *testing.T) {
	s := NewDelegationStore(filepath.Join(t.TempDir(), "delegations.json"))
	d := newTestDelegation("dlg_big", "a", "b", DelegationStatusRunning, time.Now().UTC())
	for len(d.Prompt) <= delegationPromptLimit {
		d.Prompt += "0123456789abcdef"
	}
	if err := s.Add(d); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, _, _ := s.Get("dlg_big")
	if len(got.Prompt) > delegationPromptLimit+len("… [truncated]") {
		t.Fatalf("prompt not truncated: %d bytes", len(got.Prompt))
	}
}

func TestNewDelegationID(t *testing.T) {
	a, err := NewDelegationID()
	if err != nil {
		t.Fatalf("NewDelegationID: %v", err)
	}
	b, _ := NewDelegationID()
	if a == b || len(a) != len("dlg_")+16 {
		t.Fatalf("ids should be unique 16-hex: %q %q", a, b)
	}
}

func TestAcceptsDelegationAgent(t *testing.T) {
	deny := func(n Node, agent string) {
		t.Helper()
		if err := n.AcceptsDelegationAgent(agent); err == nil {
			t.Errorf("node %q should refuse agent %q", n.ID, agent)
		}
	}
	allow := func(n Node, agent string) {
		t.Helper()
		if err := n.AcceptsDelegationAgent(agent); err != nil {
			t.Errorf("node %q should accept agent %q: %v", n.ID, agent, err)
		}
	}

	// No allowed_agents: only the default agent.
	bare := Node{ID: "bare", Delegation: &DelegationPolicy{Workdir: "/work"}}
	allow(bare, DefaultDelegationAgent)
	deny(bare, "reviewer")
	deny(bare, "skills/custom")

	// "*": plain names only; path-like names always need an exact entry.
	wild := Node{ID: "wild", Delegation: &DelegationPolicy{Workdir: "/work", AllowedAgents: []string{"*"}}}
	allow(wild, "reviewer")
	allow(wild, DefaultDelegationAgent)
	for _, agent := range []string{"../evil", "skills/custom", `..\evil`, ".hidden", "~root"} {
		deny(wild, agent)
	}

	// Exact entries, including path-like ones.
	exact := Node{ID: "exact", Delegation: &DelegationPolicy{Workdir: "/work", AllowedAgents: []string{"reviewer", "skills/custom"}}}
	allow(exact, "reviewer")
	allow(exact, "skills/custom")
	deny(exact, DefaultDelegationAgent)
}

func TestAcceptsDelegationModel(t *testing.T) {
	bare := Node{ID: "bare", Delegation: &DelegationPolicy{Workdir: "/work"}}
	if err := bare.AcceptsDelegationModel(""); err != nil {
		t.Errorf("empty model should always be allowed: %v", err)
	}
	if err := bare.AcceptsDelegationModel("haiku"); err == nil {
		t.Errorf("model override without allowed_models should be refused")
	}

	listed := Node{ID: "listed", Delegation: &DelegationPolicy{Workdir: "/work", AllowedModels: []string{"haiku"}}}
	if err := listed.AcceptsDelegationModel("haiku"); err != nil {
		t.Errorf("listed model refused: %v", err)
	}
	if err := listed.AcceptsDelegationModel("gpt-x"); err == nil {
		t.Errorf("unlisted model should be refused")
	}

	wild := Node{ID: "wild", Delegation: &DelegationPolicy{Workdir: "/work", AllowedModels: []string{"*"}}}
	if err := wild.AcceptsDelegationModel("anything"); err != nil {
		t.Errorf("wildcard model refused: %v", err)
	}
}
