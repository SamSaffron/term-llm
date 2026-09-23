//go:build linux

package hostfacts

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func fixtureCollector(t *testing.T) collector {
	t.Helper()
	root := t.TempDir()
	write := func(name, value string) {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("etc/os-release", "PRETTY_NAME=TestOS\nVERSION_ID=1\n")
	write("proc/sys/kernel/osrelease", "6.1-test\n")
	write("proc/1/comm", "runsvdir\n")
	return collector{root: root, readFile: os.ReadFile, lookPath: func(string) (string, error) { return "", errors.New("missing") }, run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("unexpected") }, hostname: func() (string, error) { return "Box.Example", nil }, currentUser: func() (*user.User, error) { return &user.User{Username: "tester", Uid: "1000"}, nil }}
}

func TestHostFactsLinuxFixtures(t *testing.T) {
	f := collect(context.Background(), fixtureCollector(t))
	if f.Distro != "TestOS" || f.Kernel != "6.1-test" || f.Init != "runit" || f.UID != 1000 {
		t.Fatalf("facts = %+v", f)
	}
}
func TestHostFactsBudget(t *testing.T) {
	c := fixtureCollector(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := collect(ctx, c)
	if len(f.Warnings) == 0 {
		t.Fatal("expected partial-fact warning")
	}
}
func TestHostFactsRenderIsDeterministic(t *testing.T) {
	c := fixtureCollector(t)
	first := collect(context.Background(), c).Render()
	for _, want := range []string{"host: Box.Example", "distro: TestOS 1", "init: runit", "user: tester (uid 1000)"} {
		if !strings.Contains(first, want) {
			t.Errorf("render missing %q: %s", want, first)
		}
	}
	for _, volatile := range []string{"load", "mem", "disk", "up:", "sudo", "collected"} {
		if strings.Contains(first, volatile) {
			t.Errorf("render contains volatile field %q: %s", volatile, first)
		}
	}
	if second := collect(context.Background(), c).Render(); second != first {
		t.Fatalf("render changed between collections:\n%s\n%s", first, second)
	}
}

func TestHostFactsCacheIsProcessLifetime(t *testing.T) {
	oldCollect := cacheCollect
	defer func() { cacheCollect = oldCollect; factCache.value = "" }()
	var calls atomic.Int32
	degraded := true
	cacheCollect = func(context.Context) Facts {
		calls.Add(1)
		f := Facts{Hostname: "x"}
		if degraded {
			f.Warnings = []string{"probe budget: deadline exceeded"}
		}
		return f
	}
	factCache.value = ""
	_ = RenderCached(context.Background())
	degraded = false
	_ = RenderCached(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("degraded collection was cached; calls=%d", calls.Load())
	}
	_ = RenderCached(context.Background())
	_ = RenderCached(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("complete collection not reused; calls=%d", calls.Load())
	}
}
