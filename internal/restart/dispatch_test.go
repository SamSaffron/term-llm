package restart

import (
	"reflect"
	"sync"
	"testing"
)

func TestDeferredInvocationIsNeverReplayed(t *testing.T) {
	var phases []string
	d := New(func(phase string) { phases = append(phases, phase) })
	d.Request()
	d.Request()
	calls := 0
	unbind := d.Bind(func() { calls++ })
	if calls != 0 {
		t.Fatal("early signal replayed into new owner")
	}
	d.Request()
	unbind()
	unbind()
	d.Request()
	d.Finish()
	if calls != 1 {
		t.Fatalf("owner calls = %d", calls)
	}
	if !reflect.DeepEqual(phases, []string{"deferred", "finished"}) {
		t.Fatalf("phases = %v", phases)
	}
}

func TestOwnerUnbindJoinsDispatch(t *testing.T) {
	d := New(nil)
	entered, release := make(chan struct{}), make(chan struct{})
	unbind := d.Bind(func() { close(entered); <-release })
	var wg sync.WaitGroup
	wg.Go(d.Request)
	<-entered
	unbound := make(chan struct{})
	wg.Go(func() { unbind(); close(unbound) })
	select {
	case <-unbound:
		t.Fatal("unbind did not join owner")
	default:
	}
	close(release)
	wg.Wait()
	d.Request() // must not call the now-destroyed owner again
}

func TestMultipleOwnersRequireCoordination(t *testing.T) {
	d := New(nil)
	defer d.Bind(func() {})()
	defer func() {
		if recover() == nil {
			t.Error("accepted competing process owner")
		}
	}()
	d.Bind(func() {})
}
