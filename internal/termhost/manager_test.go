package termhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/lifecycle"
)

type recordedCommand struct {
	path  string
	args  []string
	stdin []byte
}

func recordingRunner(commands *[]recordedCommand, mu *sync.Mutex, called chan<- struct{}) commandRunner {
	return func(_ context.Context, path string, args []string, stdin []byte) error {
		mu.Lock()
		*commands = append(*commands, recordedCommand{path: path, args: append([]string(nil), args...), stdin: append([]byte(nil), stdin...)})
		mu.Unlock()
		if called != nil {
			called <- struct{}{}
		}
		return nil
	}
}

func testRuntime(env map[string]string) runtimeContext {
	return runtimeContext{
		getenv: func(name string) string { return env[name] },
		lookPath: func(name string) (string, error) {
			return "/usr/local/bin/" + name, nil
		},
		run:        runCommand,
		now:        func() time.Time { return time.Unix(1700000000, 100) },
		goos:       "darwin",
		pid:        42,
		cwd:        "/work",
		executable: "/usr/local/bin/term-llm",
	}
}

func TestHerdrAdapterExactArgv(t *testing.T) {
	var commands []recordedCommand
	var mu sync.Mutex
	adapter := &herdrAdapter{binPath: "/opt/herdr", paneID: "w1:p2", run: recordingRunner(&commands, &mu, nil)}
	state := lifecycle.NewEvent(lifecycle.KindState, 100, time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{
		State: lifecycle.Blocked, SessionID: "session-a", Message: "Waiting for approval",
	})
	if err := adapter.Send(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	release := lifecycle.NewEvent(lifecycle.KindRelease, 101, time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{SessionID: "session-a"})
	if err := adapter.Send(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	want := []recordedCommand{
		{path: "/opt/herdr", args: []string{"pane", "report-agent", "w1:p2", "--source", "custom:term-llm", "--agent", "term-llm", "--state", "blocked", "--seq", "100", "--agent-session-id", "session-a", "--message", "Waiting for approval"}},
		{path: "/opt/herdr", args: []string{"pane", "release-agent", "w1:p2", "--source", "custom:term-llm", "--agent", "term-llm", "--seq", "101"}},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("commands = %#v\nwant = %#v", commands, want)
	}
}

func TestCMUXAdapterExactArgvAndStateMapping(t *testing.T) {
	var commands []recordedCommand
	var mu sync.Mutex
	adapter := &cmuxAdapter{
		binPath: "/Applications/cmux.app/Contents/Resources/bin/cmux", workspaceID: "workspace-1",
		statusKey: "term-llm:surface-2", run: recordingRunner(&commands, &mu, nil),
	}
	for sequence, state := range []lifecycle.State{lifecycle.Working, lifecycle.Blocked, lifecycle.Idle} {
		event := lifecycle.NewEvent(lifecycle.KindState, int64(sequence+1), time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{State: state})
		if err := adapter.Send(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if err := adapter.Send(context.Background(), lifecycle.NewEvent(lifecycle.KindRelease, 4, time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{})); err != nil {
		t.Fatal(err)
	}
	wantArgs := [][]string{
		{"set-status", "term-llm:surface-2", "Working", "--workspace", "workspace-1"},
		{"set-status", "term-llm:surface-2", "Needs input", "--workspace", "workspace-1"},
		{"set-status", "term-llm:surface-2", "Idle", "--workspace", "workspace-1"},
		{"clear-status", "term-llm:surface-2", "--workspace", "workspace-1"},
	}
	for i, want := range wantArgs {
		if commands[i].path != adapter.binPath || !reflect.DeepEqual(commands[i].args, want) {
			t.Fatalf("command %d = %#v, want %#v", i, commands[i], want)
		}
	}
}

func TestDiscoveryFansOutNestedHerdrAndCMUXDeterministically(t *testing.T) {
	rt := testRuntime(map[string]string{
		"HERDR_ENV": "1", "HERDR_PANE_ID": "pane-1", "HERDR_BIN_PATH": "/bin/herdr",
		"CMUX_WORKSPACE_ID": "workspace-1", "CMUX_SURFACE_ID": "surface-1", "CMUX_SOCKET_PATH": "/tmp/cmux.sock",
	})
	found, report, err := discoverAll(config.LifecycleConfig{Enabled: true, Adapters: []string{"auto"}, OSC: "off"}, false, rt)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range found {
		names = append(names, entry.adapter.Name())
	}
	if !reflect.DeepEqual(names, []string{"cmux", "herdr"}) {
		t.Fatalf("enabled adapters = %v", names)
	}
	if len(report.Adapters) != 2 || report.Adapters[0].Name != "herdr" || report.Adapters[1].Name != "cmux" {
		t.Fatalf("status order = %#v", report.Adapters)
	}
}

func TestDiscoveryAllowlistAndOptOuts(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config.LifecycleConfig
		env       map[string]string
		wantNames []string
		wantPart  string
	}{
		{name: "explicit allowlist", cfg: config.LifecycleConfig{Enabled: true, Adapters: []string{"herdr"}, OSC: "off"}, env: map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p", "HERDR_BIN_PATH": "/bin/herdr", "CMUX_WORKSPACE_ID": "w", "CMUX_SURFACE_ID": "s"}, wantNames: []string{"herdr"}},
		{name: "global config", cfg: config.LifecycleConfig{Enabled: false, Adapters: []string{"auto"}, OSC: "off"}, env: map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p", "HERDR_BIN_PATH": "/bin/herdr"}, wantPart: "lifecycle.enabled"},
		{name: "global env", cfg: config.LifecycleConfig{Enabled: true, Adapters: []string{"auto"}, OSC: "off"}, env: map[string]string{"TERM_LLM_LIFECYCLE": "0", "HERDR_ENV": "1", "HERDR_PANE_ID": "p", "HERDR_BIN_PATH": "/bin/herdr"}, wantPart: "TERM_LLM_LIFECYCLE"},
		{name: "herdr env", cfg: config.LifecycleConfig{Enabled: true, Adapters: []string{"auto"}, OSC: "off"}, env: map[string]string{"TERM_LLM_HERDR": "0", "HERDR_ENV": "1", "HERDR_PANE_ID": "p", "HERDR_BIN_PATH": "/bin/herdr"}, wantPart: "TERM_LLM_HERDR"},
		{name: "cmux env", cfg: config.LifecycleConfig{Enabled: true, Adapters: []string{"auto"}, OSC: "off"}, env: map[string]string{"TERM_LLM_CMUX": "0", "CMUX_WORKSPACE_ID": "w", "CMUX_SURFACE_ID": "s"}, wantPart: "TERM_LLM_CMUX"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			found, report, err := discoverAll(test.cfg, false, testRuntime(test.env))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, entry := range found {
				names = append(names, entry.adapter.Name())
			}
			if !reflect.DeepEqual(names, test.wantNames) {
				t.Fatalf("enabled = %v, want %v; report=%#v", names, test.wantNames, report)
			}
			if test.wantPart != "" {
				encoded, _ := json.Marshal(report)
				if !contains(string(encoded), test.wantPart) {
					t.Fatalf("report %s does not contain %q", encoded, test.wantPart)
				}
			}
		})
	}
}

func TestHerdrOptOutSkipsBinaryLookup(t *testing.T) {
	lookedUp := false
	rt := testRuntime(map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p", "TERM_LLM_HERDR": "0"})
	rt.lookPath = func(string) (string, error) { lookedUp = true; return "", errors.New("missing") }
	_ = discoverHerdr(rt)
	if lookedUp {
		t.Fatal("TERM_LLM_HERDR=0 performed binary lookup")
	}
}

func TestCMUXOptOutSkipsBinaryLookup(t *testing.T) {
	lookedUp := false
	rt := testRuntime(map[string]string{"CMUX_WORKSPACE_ID": "w", "CMUX_SURFACE_ID": "s", "TERM_LLM_CMUX": "0"})
	rt.lookPath = func(string) (string, error) { lookedUp = true; return "", errors.New("missing") }
	_ = discoverCMUX(rt)
	if lookedUp {
		t.Fatal("TERM_LLM_CMUX=0 performed binary lookup")
	}
}

func TestDisabledAndUnselectedAdaptersSkipDiscovery(t *testing.T) {
	tests := []struct {
		name        string
		cfg         config.LifecycleConfig
		wantLookups []string
	}{
		{
			name: "global disable",
			cfg:  config.LifecycleConfig{Enabled: false, Adapters: []string{"auto"}, OSC: "auto"},
		},
		{
			name:        "explicit allowlist",
			cfg:         config.LifecycleConfig{Enabled: true, Adapters: []string{"herdr"}, OSC: "off"},
			wantLookups: []string{"herdr"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rt := testRuntime(map[string]string{
				"HERDR_ENV": "1", "HERDR_PANE_ID": "p",
				"CMUX_WORKSPACE_ID": "w", "CMUX_SURFACE_ID": "s",
			})
			var lookups []string
			rt.lookPath = func(name string) (string, error) {
				lookups = append(lookups, name)
				return "/bin/" + name, nil
			}
			_, report, err := discoverAll(test.cfg, false, rt)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(lookups, test.wantLookups) {
				t.Fatalf("binary lookups = %v, want %v", lookups, test.wantLookups)
			}
			for _, status := range report.Adapters {
				if status.Name == "cmux" && !contains(status.Reason, "detection skipped") {
					t.Fatalf("cmux status does not explain skipped detection: %#v", status)
				}
			}
			if !test.cfg.Enabled && !contains(report.OSC.Reason, "detection skipped") {
				t.Fatalf("OSC status does not explain skipped detection: %#v", report.OSC)
			}
		})
	}
}

type fakeAdapter struct {
	name string
	send func(context.Context, lifecycle.Event) error
}

func (a *fakeAdapter) Name() string { return a.name }
func (a *fakeAdapter) Send(ctx context.Context, event lifecycle.Event) error {
	if a.send != nil {
		return a.send(ctx, event)
	}
	return nil
}

func TestAdapterWorkerTimeoutNeverOverlapsSendAndKeepsOrder(t *testing.T) {
	firstStarted := make(chan struct{})
	firstTimedOut := make(chan struct{})
	unblockFirst := make(chan struct{})
	called := make(chan lifecycle.Event, 8)
	var mu sync.Mutex
	active, maxActive := 0, 0
	var events []lifecycle.Event
	adapter := &fakeAdapter{name: "ordered", send: func(ctx context.Context, event lifecycle.Event) error {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		events = append(events, event)
		first := len(events) == 1
		mu.Unlock()
		called <- event
		if first {
			close(firstStarted)
			// Observe but deliberately do not return on cancellation. The worker
			// must not start another Send until this call actually returns.
			<-ctx.Done()
			close(firstTimedOut)
			<-unblockFirst
		}
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}}
	worker := newAdapterWorker(adapter, 10*time.Millisecond, 50*time.Millisecond, nil)
	first := lifecycle.NewEvent(lifecycle.KindState, 1, time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{State: lifecycle.Idle})
	worker.enqueue(first)
	<-firstStarted
	<-called
	for sequence := int64(2); sequence <= 100; sequence++ {
		worker.enqueue(lifecycle.NewEvent(lifecycle.KindState, sequence, time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{
			State: lifecycle.Working, Message: fmt.Sprintf("state-%d", sequence),
		}))
	}
	<-firstTimedOut
	select {
	case event := <-called:
		t.Fatalf("overlapping Send after timeout: %#v", event)
	default:
	}
	close(unblockFirst)
	latest := <-called
	if latest.Sequence != 100 {
		t.Fatalf("coalesced event sequence = %d, want 100", latest.Sequence)
	}
	release := lifecycle.NewEvent(lifecycle.KindRelease, 101, time.Now(), lifecycle.Metadata{}, lifecycle.Snapshot{})
	worker.beginClose(release, true)
	gotRelease := <-called
	if gotRelease.Kind != lifecycle.KindRelease || gotRelease.Sequence != 101 {
		t.Fatalf("release = %#v", gotRelease)
	}
	<-worker.done
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 || active != 0 {
		t.Fatalf("Send concurrency max=%d active=%d, want 1/0", maxActive, active)
	}
	if len(events) != 3 || events[0].Sequence != 1 || events[1].Sequence != 100 || events[2].Kind != lifecycle.KindRelease {
		t.Fatalf("adapter order = %#v", events)
	}
}

func TestLifecycleDebugReportsSafeRateLimitedErrorClasses(t *testing.T) {
	var output bytes.Buffer
	rt := testRuntime(map[string]string{"TERM_LLM_LIFECYCLE_DEBUG": "1"})
	rt.stderr = &output
	called := make(chan struct{}, 4)
	adapter := &fakeAdapter{name: "unsafe\nname", send: func(context.Context, lifecycle.Event) error {
		called <- struct{}{}
		return errors.New("secret argv --token=abc session=session-private")
	}}
	manager := newManager([]Adapter{adapter}, nil, rt, 200*time.Millisecond)
	manager.Report(lifecycle.Snapshot{State: lifecycle.Idle, SessionID: "session-private"})
	<-called
	manager.Report(lifecycle.Snapshot{State: lifecycle.Working, SessionID: "session-private"})
	<-called
	manager.Close()
	got := output.String()
	if got != "term-llm lifecycle: adapter \"unsafe\\nname\" send failed\n" {
		t.Fatalf("diagnostic = %q", got)
	}
	for _, forbidden := range []string{"--token", "abc", "session-private"} {
		if contains(got, forbidden) {
			t.Fatalf("diagnostic leaked %q: %q", forbidden, got)
		}
	}
}

func TestLifecycleDebugIsOffByDefault(t *testing.T) {
	var output bytes.Buffer
	rt := testRuntime(nil)
	rt.stderr = &output
	called := make(chan struct{}, 2)
	manager := newManager([]Adapter{&fakeAdapter{name: "quiet", send: func(context.Context, lifecycle.Event) error {
		called <- struct{}{}
		return errors.New("failed")
	}}}, nil, rt, 200*time.Millisecond)
	manager.Report(lifecycle.Snapshot{State: lifecycle.Idle})
	<-called
	manager.Close()
	if output.Len() != 0 {
		t.Fatalf("default diagnostic output = %q", output.String())
	}
}

func TestManagerFanoutIndependentCoalescingFailuresAndRelease(t *testing.T) {
	slowStarted := make(chan struct{})
	unblockSlow := make(chan struct{})
	var slowOnce sync.Once
	var mu sync.Mutex
	events := map[string][]lifecycle.Event{}
	called := make(chan string, 16)
	adapter := func(name string, slow bool, fail bool) *fakeAdapter {
		return &fakeAdapter{name: name, send: func(ctx context.Context, event lifecycle.Event) error {
			mu.Lock()
			events[name] = append(events[name], event)
			mu.Unlock()
			called <- name
			if slow && event.Kind == lifecycle.KindState {
				slowOnce.Do(func() { close(slowStarted) })
				select {
				case <-unblockSlow:
				case <-ctx.Done():
				}
			}
			if fail {
				return errors.New("broken adapter")
			}
			return nil
		}}
	}
	rt := testRuntime(nil)
	manager := newManager([]Adapter{adapter("slow", true, false), adapter("fast", false, true)}, nil, rt, 500*time.Millisecond)
	manager.Report(lifecycle.Snapshot{State: lifecycle.Idle, SessionID: "session-a", CWD: "/session/worktree"})
	<-slowStarted
	waitForAdapter(t, called, "fast")

	manager.Report(lifecycle.Snapshot{State: lifecycle.Working, SessionID: "session-a", Message: "obsolete"})
	waitForAdapter(t, called, "fast")
	manager.Report(lifecycle.Snapshot{State: lifecycle.Blocked, SessionID: "session-a", Message: "obsolete blocker"})
	waitForAdapter(t, called, "fast")
	manager.Report(lifecycle.Snapshot{State: lifecycle.Working, SessionID: "session-a", Message: "latest"})
	waitForAdapter(t, called, "fast")
	close(unblockSlow)
	waitForAdapter(t, called, "slow")
	manager.Close()

	mu.Lock()
	defer mu.Unlock()
	if got := events["slow"]; len(got) != 3 || got[1].Message != "latest" || got[2].Kind != lifecycle.KindRelease {
		t.Fatalf("slow events = %#v", got)
	}
	if got := events["fast"]; len(got) != 5 || got[4].Kind != lifecycle.KindRelease {
		t.Fatalf("fast events = %#v", got)
	}
	if events["slow"][2].Sequence != events["fast"][4].Sequence {
		t.Fatal("release did not use shared sequence")
	}
	if events["fast"][0].CWD != "/session/worktree" {
		t.Fatalf("visible session cwd = %q", events["fast"][0].CWD)
	}
	if got := events["fast"][1].ResumeArgv; !reflect.DeepEqual(got, []string{"/usr/local/bin/term-llm", "chat", "--resume=session-a"}) {
		t.Fatalf("resume argv = %#v", got)
	}
}

func TestManagerDeduplicatesAndReleasesOnce(t *testing.T) {
	var mu sync.Mutex
	var events []lifecycle.Event
	called := make(chan struct{}, 2)
	adapter := &fakeAdapter{name: "one", send: func(_ context.Context, event lifecycle.Event) error {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		called <- struct{}{}
		return nil
	}}
	manager := newManager([]Adapter{adapter}, nil, testRuntime(nil), 500*time.Millisecond)
	snapshot := lifecycle.Snapshot{State: lifecycle.Working, SessionID: "s", Message: "work"}
	manager.Report(snapshot)
	<-called
	manager.Report(snapshot)
	manager.Close()
	manager.Close()
	manager.Report(lifecycle.Snapshot{State: lifecycle.Idle})
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Kind != lifecycle.KindState || events[1].Kind != lifecycle.KindRelease {
		t.Fatalf("events = %#v", events)
	}
	if events[1].Sequence <= events[0].Sequence {
		t.Fatalf("non-monotonic events = %#v", events)
	}
}

func TestManagerCloseUsesOneAggregateBudget(t *testing.T) {
	adapter := func(name string) *fakeAdapter {
		return &fakeAdapter{name: name, send: func(_ context.Context, event lifecycle.Event) error {
			if event.Kind == lifecycle.KindRelease {
				time.Sleep(80 * time.Millisecond)
			}
			return nil
		}}
	}
	manager := newManager([]Adapter{adapter("a"), adapter("b"), adapter("c")}, nil, testRuntime(nil), 300*time.Millisecond)
	manager.Report(lifecycle.Snapshot{State: lifecycle.Idle})
	time.Sleep(10 * time.Millisecond)
	started := time.Now()
	manager.Close()
	if elapsed := time.Since(started); elapsed > 140*time.Millisecond {
		t.Fatalf("Close took %v; adapters appear serialized", elapsed)
	}
}

func TestManagerConcurrentReportIsRaceSafe(t *testing.T) {
	adapter := &fakeAdapter{name: "race"}
	manager := newManager([]Adapter{adapter}, nil, testRuntime(nil), 500*time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			state := lifecycle.Working
			if i%2 == 0 {
				state = lifecycle.Blocked
			}
			manager.Report(lifecycle.Snapshot{State: state, Message: "state"})
		}(i)
	}
	wg.Wait()
	manager.Close()
}

func waitForAdapter(t *testing.T, called <-chan string, want string) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case got := <-called:
			if got == want {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", want)
		}
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func TestStatusAdapterOrderStable(t *testing.T) {
	_, report, err := discoverAll(config.LifecycleConfig{
		Enabled: true, Adapters: []string{"auto"}, OSC: "off",
		Commands: []config.LifecycleCommandConfig{{Name: "zellij", Command: []string{"bridge"}}, {Name: "tmux", Command: []string{"bridge"}}},
	}, false, testRuntime(nil))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(report.Adapters))
	for _, status := range report.Adapters {
		got = append(got, status.Type+":"+status.Name)
	}
	want := append([]string(nil), got...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status order = %v, want sorted %v", got, want)
	}
}
