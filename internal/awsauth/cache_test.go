package awsauth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialsCacheRefreshAndConcurrency(t *testing.T) {
	now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)
	var calls atomic.Int32
	c := &credentialsCache{now: func() time.Time { return now }, provider: retrieveFunc(func(context.Context) (Credentials, error) {
		calls.Add(1)
		return Credentials{AccessKeyID: "id", SecretAccessKey: "secret", CanExpire: true, Expires: now.Add(time.Hour)}, nil
	})}
	concurrent := func() {
		t.Helper()
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 32; i++ {
			wg.Go(func() {
				<-start
				v, err := c.Retrieve(context.Background())
				if err != nil || v.AccessKeyID != "id" {
					t.Errorf("retrieve error: %v", err)
				}
			})
		}
		close(start)
		wg.Wait()
	}
	concurrent()
	if calls.Load() != 1 {
		t.Fatalf("initial refreshes = %d", calls.Load())
	}
	now = expires.Add(-5*time.Minute - time.Nanosecond)
	concurrent()
	if calls.Load() != 1 {
		t.Error("refreshed before expiry window")
	}
	now = expires.Add(-5 * time.Minute)
	concurrent()
	if calls.Load() != 2 {
		t.Errorf("refreshes at window = %d", calls.Load())
	}
	now = now.Add(2 * time.Hour)
	concurrent()
	if calls.Load() != 3 {
		t.Errorf("refreshes after expiry = %d", calls.Load())
	}
}

func TestCredentialsCacheDoesNotCacheErrors(t *testing.T) {
	now := time.Now()
	calls := 0
	fail := errors.New("retrieve failed")
	c := &credentialsCache{now: func() time.Time { return now }, provider: retrieveFunc(func(context.Context) (Credentials, error) {
		calls++
		if calls != 2 {
			return Credentials{}, fail
		}
		return Credentials{AccessKeyID: "id", CanExpire: true, Expires: now.Add(time.Hour)}, nil
	})}
	if _, err := c.Retrieve(context.Background()); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	if _, err := c.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	now = now.Add(time.Hour)
	for i := 0; i < 2; i++ {
		if _, err := c.Retrieve(context.Background()); !errors.Is(err, fail) {
			t.Fatal("stale credentials returned", err)
		}
	}
	if calls != 4 {
		t.Fatal("error was cached")
	}
}

func TestCredentialsCacheStaticNeverExpires(t *testing.T) {
	calls := 0
	c := &credentialsCache{now: func() time.Time { t.Fatal("static should not check expiry"); return time.Time{} }, provider: retrieveFunc(func(context.Context) (Credentials, error) { calls++; return Credentials{AccessKeyID: "id"}, nil })}
	for i := 0; i < 3; i++ {
		if _, err := c.Retrieve(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Error(calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Retrieve(ctx); !errors.Is(err, context.Canceled) {
		t.Error(err)
	}
}

// waitingContext signals when Retrieve starts waiting on a shared refresh.
type waitingContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *waitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestCredentialsCacheSharesInFlightErrors(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	fail := errors.New("refresh failed")
	var calls atomic.Int32
	c := &credentialsCache{now: time.Now, provider: retrieveFunc(func(context.Context) (Credentials, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return Credentials{}, fail
	})}
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := c.Retrieve(context.Background()); !errors.Is(err, fail) {
			t.Error(err)
		}
	})
	<-started
	entered := make([]chan struct{}, 8)
	for i := range entered {
		entered[i] = make(chan struct{})
		ctx := &waitingContext{Context: context.Background(), entered: entered[i]}
		wg.Go(func() {
			if _, err := c.Retrieve(ctx); !errors.Is(err, fail) {
				t.Error(err)
			}
		})
	}
	for _, ch := range entered {
		<-ch
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Error("in-flight error was not shared", calls.Load())
	}
	if _, err := c.Retrieve(context.Background()); !errors.Is(err, fail) {
		t.Error(err)
	}
	if calls.Load() != 2 {
		t.Error("error cached after flight ended")
	}
}

func TestCredentialsCacheWaiterCanCancel(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	c := &credentialsCache{now: time.Now, provider: retrieveFunc(func(context.Context) (Credentials, error) {
		close(started)
		<-release
		return Credentials{AccessKeyID: "id"}, nil
	})}
	go func() {
		defer close(finished)
		if _, err := c.Retrieve(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	<-started
	defer func() { close(release); <-finished }()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &waitingContext{Context: base, entered: make(chan struct{})}
	result := make(chan error, 1)
	go func() { _, err := c.Retrieve(ctx); result <- err }()
	select {
	case <-ctx.entered:
	case <-time.After(time.Second):
		t.Fatal("waiter held the cache mutex during refresh")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter blocked on refresh")
	}
}

func TestCredentialsCacheLeaderCancellationDoesNotFailWaiters(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := &credentialsCache{now: time.Now, provider: retrieveFunc(func(ctx context.Context) (Credentials, error) {
		close(started)
		<-release
		if err := ctx.Err(); err != nil {
			return Credentials{}, err
		}
		return Credentials{AccessKeyID: "id"}, nil
	})}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() { _, err := c.Retrieve(leaderCtx); leaderErr <- err }()
	<-started
	waiter := make(chan error, 1)
	entered := make(chan struct{})
	go func() {
		_, err := c.Retrieve(&waitingContext{Context: context.Background(), entered: entered})
		waiter <- err
	}()
	<-entered
	cancelLeader()
	if err := <-leaderErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-waiter; err != nil {
		t.Fatalf("waiter inherited leader cancellation: %v", err)
	}
}
