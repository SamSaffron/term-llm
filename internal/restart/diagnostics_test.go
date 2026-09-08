package restart

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBlockerDiagnosticsTrackOwnershipNotCancellation(t *testing.T) {
	c := &Coordinator{}
	ctx, cancel := context.WithCancel(context.Background())
	var owners []context.Context
	var releases []func()
	for i := 0; i < 2; i++ {
		owned, release, err := c.Root(ctx)
		if err != nil {
			t.Fatal(err)
		}
		owners = append(owners, owned)
		releases = append(releases, release)
		defer release()
	}
	var observed []Blocker
	c.Diagnostic = func(phase string, blockers []Blocker) {
		_ = c.Status() // Logging must not run under the admission mutex.
		if phase != "draining" {
			t.Error("wrong diagnostic phase", phase)
		}
		observed = blockers
	}
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	cancel()
	c.reportBlockers(c.done)
	if len(observed) != 1 || observed[0].Count != 2 || !strings.HasPrefix(observed[0].Source, "diagnostics_test.go:") || observed[0].Oldest <= 0 {
		t.Fatalf("missing cancelled-but-unsettled owners: %+v", observed)
	}
	Passive(owners[0])
	releases[0]() // Passive detachment and deferred release must be idempotent.
	c.reportBlockers(c.done)
	if len(observed) != 1 || observed[0].Count != 1 {
		t.Fatal("passive stream still blocks", observed)
	}
	observed = nil
	c.reportBlockers(make(chan struct{}))
	if observed != nil {
		t.Fatal("old attempt reported current blockers")
	}
	releases[1]()
	waitAttempt(t, c)
	c.reportBlockers(c.done)
	if observed != nil {
		t.Fatal("idle process logged blockers")
	}
	if len(c.operations) != 0 {
		t.Fatal("released operations retained")
	}
}

func TestGraceDiagnosticIdentifiesStillRunningOwner(t *testing.T) {
	c := &Coordinator{InterruptAfter: 10 * time.Millisecond}
	observed := make(chan []Blocker, 1)
	c.Diagnostic = func(phase string, blockers []Blocker) {
		if phase != "cancelling" {
			t.Error("expected cancellation diagnostic", phase)
		}
		observed <- blockers
	}
	ctx, release, _ := c.Root(context.Background())
	defer release()
	ctx, finish := c.Cancellable(ctx)
	defer finish()
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	select {
	case blockers := <-observed:
		if len(blockers) != 1 || blockers[0].Count != 1 {
			t.Fatal(blockers)
		}
	case <-time.After(time.Second):
		t.Fatal("missing grace-period diagnostic")
	}
	<-ctx.Done()
	release()
	waitAttempt(t, c)
}

func TestSlowDrainLogsBeforeGraceCancellation(t *testing.T) {
	observed := make(chan []Blocker, 1)
	c := &Coordinator{Diagnostic: func(_ string, blockers []Blocker) { observed <- blockers }}
	_, release, _ := c.Root(context.Background())
	defer release()
	stop, err := c.Bind(context.Background(), func(context.Context) error { return errors.New("fixture exec") })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	c.Request()
	select {
	case blockers := <-observed:
		if len(blockers) != 1 || blockers[0].Count != 1 {
			t.Fatal(blockers)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("slow drain did not identify its blocker before 30s grace")
	}
	release()
	waitAttempt(t, c)
}
