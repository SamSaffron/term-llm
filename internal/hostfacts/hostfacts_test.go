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
	"time"
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
	write("proc/uptime", "90061.00 0\n")
	write("proc/loadavg", "0.10 0.20 0.30 1/1 1\n")
	write("proc/meminfo", "MemTotal: 1024 kB\nMemAvailable: 512 kB\n")
	return collector{root: root, now: func() time.Time { return time.Unix(1, 0) }, readFile: os.ReadFile, lookPath: func(string) (string, error) { return "", errors.New("missing") }, run: func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("unexpected") }, hostname: func() (string, error) { return "Box.Example", nil }, currentUser: func() (*user.User, error) { return &user.User{Username: "tester", Uid: "1000"}, nil }}
}

func TestHostFactsLinuxFixtures(t *testing.T) {
	f := collect(context.Background(), fixtureCollector(t))
	if f.Distro != "TestOS" || f.Init != "runit" || !f.LoadKnown || f.MemTotalBytes != 1024*1024 {
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
func TestHostFactsSudoSkip(t *testing.T) {
	c := fixtureCollector(t)
	c.currentUser = func() (*user.User, error) { return &user.User{Username: "root", Uid: "0"}, nil }
	c.lookPath = func(name string) (string, error) {
		if name == "sudo" {
			return "/usr/bin/sudo", nil
		}
		return "", errors.New("missing")
	}
	c.run = func(context.Context, string, ...string) ([]byte, error) { t.Fatal("sudo ran as root"); return nil, nil }
	f := collect(context.Background(), c)
	if f.SudoNoPassword != nil {
		t.Fatal("sudo result should be nil")
	}
}
func TestHostFactsRender(t *testing.T) {
	f := collect(context.Background(), fixtureCollector(t))
	got := f.Render()
	for _, want := range []string{"host: Box.Example", "init: runit", "load: 0.10 0.20 0.30", "term-llm:"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q: %s", want, got)
		}
	}
}
func TestHostFactsCache(t *testing.T) {
	oldNow, oldCollect := cacheNow, cacheCollect
	defer func() { cacheNow, cacheCollect = oldNow, oldCollect; factCache.value = "" }()
	now := time.Unix(0, 0)
	cacheNow = func() time.Time { return now }
	var calls atomic.Int32
	cacheCollect = func(context.Context) Facts {
		calls.Add(1)
		return Facts{Hostname: "x", CollectedAt: now, UptimeSeconds: -1}
	}
	factCache.value = ""
	_ = RenderCached(context.Background())
	_ = RenderCached(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	now = now.Add(61 * time.Second)
	_ = RenderCached(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("calls after expiry=%d", calls.Load())
	}
}
