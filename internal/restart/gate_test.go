package restart

import "testing"

func TestGateIncludesDescendantsAndRollback(t *testing.T) {
	var gate Gate
	release, ok := gate.Enter()
	if !ok {
		t.Fatal("initial work rejected")
	}
	reopen := gate.Pause()
	if _, ok := gate.Enter(); ok {
		t.Fatal("new work admitted during drain")
	}
	child := gate.TrackChild()
	release()
	release()
	if gate.Drained() {
		t.Fatal("ignored child ownership")
	}
	child()
	child()
	if !gate.Drained() {
		t.Fatal("completed gate not drained")
	}
	reopen()
	reopen()
	release, ok = gate.Enter()
	if !ok {
		t.Fatal("failed replacement did not reopen admission")
	}
	release()
}

func TestGateNestedPauseDoesNotReopenAnotherOwner(t *testing.T) {
	var gate Gate
	first, second := gate.Pause(), gate.Pause()
	first()
	if _, ok := gate.Enter(); ok {
		t.Fatal("one rollback reopened another owner")
	}
	second()
	release, ok := gate.Enter()
	if !ok {
		t.Fatal("all owners released but admission stayed closed")
	}
	release()
}
