package chat

import (
	"testing"
	"time"
)

func TestOperationGateSealCancelsUndispatchedReservation(t *testing.T) {
	var gate operationGate
	lease, ok := gate.reserve()
	if !ok {
		t.Fatal("reserve rejected on open gate")
	}
	if !gate.sealAndWait(time.Second) {
		t.Fatal("undispatched reservation blocked cleanup")
	}
	if lease.start() {
		t.Fatal("command started after cleanup sealed its reservation")
	}
	if _, ok := gate.reserve(); ok {
		t.Fatal("sealed gate accepted new work")
	}
}

func TestOperationGateTimeoutDoesNotReleaseRunningOperation(t *testing.T) {
	var gate operationGate
	lease, ok := gate.reserve()
	if !ok || !lease.start() {
		t.Fatal("failed to start operation")
	}
	if gate.sealAndWait(time.Millisecond) {
		t.Fatal("running operation incorrectly reported drained")
	}
	lease.finish()
	gate.wait()
}
