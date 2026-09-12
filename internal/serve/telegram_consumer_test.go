package serve

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/restart"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

type blockingCloseTestStream struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingCloseTestStream) Recv() (llm.Event, error) { return llm.Event{}, io.EOF }
func (s *blockingCloseTestStream) Close() error {
	close(s.started)
	<-s.release
	return nil
}

func coordinatorRejectingBackgroundActivity(t *testing.T) (*restart.Coordinator, func()) {
	t.Helper()
	phases := make(chan string, 8)
	replaceRelease := make(chan struct{})
	coordinator := &restart.Coordinator{Observe: func(status restart.Status) {
		select {
		case phases <- status.Phase:
		default:
		}
	}}
	stop, err := coordinator.Bind(context.Background(), func(context.Context) error {
		<-replaceRelease
		return errors.New("test replacement stopped")
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.Request()
	deadline := time.After(time.Second)
	for {
		select {
		case phase := <-phases:
			if phase == "replacing" {
				if _, _, err := coordinator.Activity(context.Background()); !errors.Is(err, restart.ErrDraining) {
					t.Fatalf("background activity error = %v, want draining", err)
				}
				return coordinator, func() {
					close(replaceRelease)
					stop()
				}
			}
		case <-deadline:
			t.Fatal("coordinator did not reach replacing phase")
		}
	}
}

func TestStartTelegramStreamCloseRejectedAdmissionStaysAsynchronous(t *testing.T) {
	coordinator, stopCoordinator := coordinatorRejectingBackgroundActivity(t)
	defer stopCoordinator()

	stream := &blockingCloseTestStream{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	var once sync.Once
	returned := make(chan struct{})
	go func() {
		startTelegramStreamCloseWithCoordinator(coordinator, stream, done, &once)
		close(returned)
	}()

	select {
	case <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("fallback Close did not start")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("startTelegramStreamClose blocked on provider Close")
	}
	select {
	case <-done:
		t.Fatal("close completion signaled while provider Close was blocked")
	default:
	}
	close(stream.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("close completion was not signaled")
	}
}

func TestStartTelegramRunnerRejectedAdmissionClosesPipe(t *testing.T) {
	coordinator := &restart.Coordinator{}
	ctx, releaseOperation, err := coordinator.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releaseOperation()

	pipe := runpkg.NewEventPipe(context.Background(), 1)
	done := make(chan struct{})
	called := false
	err = startTelegramRunnerWithCoordinator(coordinator, ctx, pipe, done, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, restart.ErrReleased) {
		t.Fatalf("runner start error = %v, want released operation", err)
	}
	if called {
		t.Fatal("rejected runner callback executed")
	}
	select {
	case <-done:
	default:
		t.Fatal("runner completion remained open")
	}
	if _, recvErr := pipe.Recv(); !errors.Is(recvErr, restart.ErrReleased) {
		t.Fatalf("pipe error = %v, want released operation", recvErr)
	}
}

func TestLaunchTelegramStreamConsumerSettlesRejectedAdmission(t *testing.T) {
	coordinator := &restart.Coordinator{}
	ctx, release, err := coordinator.Root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()

	consumerDone := make(chan struct{})
	streamDone := make(chan error, 1)
	var doneOnce sync.Once
	err = launchTelegramStreamConsumerWithCoordinator(
		coordinator,
		ctx,
		&oneShotTextStream{text: "unused"},
		newTelegramEventAccumulator(nil),
		&telegramSession{},
		make(chan struct{}, 1),
		streamDone,
		&doneOnce,
		consumerDone,
	)
	if !errors.Is(err, restart.ErrReleased) {
		t.Fatalf("launch error = %v, want released operation", err)
	}
	select {
	case <-consumerDone:
	default:
		t.Fatal("consumerDone remained open after rejected admission")
	}
	select {
	case streamErr := <-streamDone:
		if !errors.Is(streamErr, restart.ErrReleased) {
			t.Fatalf("stream error = %v, want released operation", streamErr)
		}
	default:
		t.Fatal("streamDone did not receive rejected admission error")
	}
}
