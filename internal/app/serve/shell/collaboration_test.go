package shell

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/app/serve/protocol"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/tools"
)

type recordingServeShellProcess struct {
	writes int
	done   chan Exit
}

func (p *recordingServeShellProcess) Write(data []byte) (int, error) {
	p.writes++
	return len(data), nil
}

func (p *recordingServeShellProcess) Resize(int, int) error { return nil }

func (p *recordingServeShellProcess) Close() {}

func (p *recordingServeShellProcess) Done() <-chan Exit { return p.done }

type blockingCloseServeShellProcess struct {
	closeStarted chan struct{}
	releaseClose chan struct{}
	done         chan Exit
	once         sync.Once
}

func (p *blockingCloseServeShellProcess) Write(data []byte) (int, error) { return len(data), nil }

func (p *blockingCloseServeShellProcess) Resize(int, int) error { return nil }

func (p *blockingCloseServeShellProcess) Done() <-chan Exit { return p.done }

func (p *blockingCloseServeShellProcess) Close() {
	p.once.Do(func() { close(p.closeStarted) })
	<-p.releaseClose
}

func TestServeShellEvictionSerializesCloseBeforeReplacement(t *testing.T) {
	if !Supported() {
		t.Skip("PTY unsupported")
	}
	t.Setenv("SHELL", "/bin/sh")
	manager := NewManager(time.Minute, func(string) bool { return false })
	defer manager.Close()
	process := &blockingCloseServeShellProcess{
		closeStarted: make(chan struct{}), releaseClose: make(chan struct{}), done: make(chan Exit),
	}
	shell := NewShell("sh_old", "session", "/")
	shell.process = process
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	evicted := make(chan struct{})
	go func() {
		manager.evictExpired()
		close(evicted)
	}()
	select {
	case <-process.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("eviction did not begin closing old PTY")
	}
	created := make(chan error, 1)
	go func() {
		_, _, err := manager.Create("session", t.TempDir(), 80, 24)
		created <- err
	}()
	select {
	case err := <-created:
		t.Fatalf("replacement completed before old PTY close: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(process.releaseClose)
	<-evicted
	if err := <-created; err != nil {
		t.Fatal(err)
	}
}

func TestServeShellTeardownInvalidatesBeforeRemovingAuthority(t *testing.T) {
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	shell := NewShell("sh_teardown", "session", "/")
	shell.process = &recordingServeShellProcess{done: make(chan Exit)}
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}

	// Hold invalidation's write gate so the close path pauses exactly at the
	// ordering boundary under test.
	op := manager.sessionOperation("session")
	shell.writeMu.Lock()
	closed := make(chan error, 1)
	go func() { closed <- manager.CloseShell("session", shell.id) }()
	deadline := time.Now().Add(time.Second)
	closeOwnsOperation := false
	for !closeOwnsOperation && time.Now().Before(deadline) {
		if op.TryLock() {
			op.Unlock()
			time.Sleep(time.Millisecond)
			continue
		}
		closeOwnsOperation = true
	}
	if !closeOwnsOperation {
		shell.writeMu.Unlock()
		t.Fatal("close did not reach invalidation boundary")
	}
	mode := controller.Mode(context.Background(), "session")
	if !mode.Enabled || mode.ShellID != shell.id {
		shell.writeMu.Unlock()
		t.Fatalf("authority disappeared before invalidation: %+v", mode)
	}
	shell.writeMu.Unlock()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if mode := controller.Mode(context.Background(), "session"); mode.Enabled {
		t.Fatalf("authority remained enabled after close: %+v", mode)
	}
}

func TestServeShellManagerConcurrentCreateUsesOneGeneration(t *testing.T) {
	if !Supported() {
		t.Skip("PTY unsupported")
	}
	t.Setenv("SHELL", "/bin/sh")
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	const callers = 12
	var wg sync.WaitGroup
	results := make(chan *Shell, callers)
	errs := make(chan error, callers)
	created := make(chan bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			shell, wasCreated, err := manager.Create("session", t.TempDir(), 80, 24)
			if err != nil {
				errs <- err
				return
			}
			results <- shell
			created <- wasCreated
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	close(created)
	for err := range errs {
		t.Fatal(err)
	}
	var first *Shell
	createdCount := 0
	for shell := range results {
		if first == nil {
			first = shell
		} else if shell != first {
			t.Fatalf("duplicate generation %s and %s", first.id, shell.id)
		}
	}
	for value := range created {
		if value {
			createdCount++
		}
	}
	if first == nil || createdCount != 1 {
		t.Fatalf("first=%p created=%d", first, createdCount)
	}
}

type probeReplyServeShellProcess struct {
	shell  *Shell
	writes int
	done   chan Exit
}

func (p *probeReplyServeShellProcess) Write(data []byte) (int, error) {
	p.writes++
	nonce := serveShellTestNoncePattern.FindString(string(data))
	if nonce == "" {
		return 0, errors.New("nonce missing")
	}
	echo := bytes.TrimSuffix(data, []byte{'\n'})
	mid := len(echo) / 2
	styledTail := append([]byte("\x1b[38;5;244m"), echo[mid:]...)
	styledTail = append(styledTail, []byte("\x1b[0m")...)
	p.shell.AppendOutput(append([]byte("prompt redraw\r\n"), echo[:mid]...))
	p.shell.AppendOutput(append(append(styledTail, '\r', '\n'), []byte("\x1b]7770;P;"+nonce+"\x07sam@arch term-llm % ")...))
	return len(data), nil
}

func (p *probeReplyServeShellProcess) Resize(int, int) error { return nil }

func (p *probeReplyServeShellProcess) Close() {}

func (p *probeReplyServeShellProcess) Done() <-chan Exit { return p.done }

func TestServeShellAgentWrapperHiddenThroughBeginMarker(t *testing.T) {
	shell := NewShell("sh_agent_display", "session", "/")
	nonce := strings.Repeat("A", protocol.NonceSize)
	waiter := &markerWaiter{nonce: nonce, finalMarker: 'E', ch: make(chan protocol.Marker, 4)}
	wrapper := []byte("printf protocol wrapper")
	shell.mu.Lock()
	shell.markerWaiter = waiter
	shell.beginProtocolDisplayRedactionLocked(wrapper, 'B', commandDisplay("find . -name '*circle*'"))
	shell.mu.Unlock()

	shell.AppendOutput([]byte("prompt % printf protocol "))
	if snapshot := shell.Snapshot(0); snapshot.NextOffset != 0 || len(snapshot.Data) != 0 {
		t.Fatalf("partial wrapper leaked before B: %+v", snapshot)
	}
	shell.AppendOutput([]byte("wrapper\r\n\x1b]7770;P;" + nonce + "\x07\x1b]7770;B;" + nonce + "\x07command output\r\n"))
	visible := shell.Snapshot(0)
	if bytes.Contains(visible.Data, []byte("protocol wrapper")) || !bytes.Contains(visible.Data, []byte("find . -name '*circle*'\r\n")) || !bytes.Contains(visible.Data, []byte("command output\r\n")) || bytes.Count(visible.Data, []byte{0}) == 0 {
		t.Fatalf("agent wrapper was not replaced with a clean command line: %q", visible.Data)
	}
}

func TestServeShellProbeEchoMismatchAndIncompleteReleaseDisplay(t *testing.T) {
	t.Run("mismatch", func(t *testing.T) {
		shell := NewShell("sh_probe_mismatch", "session", "/")
		waiter := &markerWaiter{nonce: strings.Repeat("M", protocol.NonceSize), finalMarker: 'P', ch: make(chan protocol.Marker, 1)}
		shell.mu.Lock()
		shell.markerWaiter = waiter
		shell.beginProtocolDisplayRedactionLocked([]byte("case hidden\n"), 'P', nil)
		changed := shell.changed
		shell.mu.Unlock()

		shell.AppendOutput([]byte("case nope\r\nordinary output\r\n"))
		select {
		case <-changed:
			t.Fatal("mismatched probe output escaped before the probe finished")
		default:
		}
		_, _, _ = shell.finishProtocol(waiter, 0)
		select {
		case <-changed:
		default:
			t.Fatal("mismatched probe echo did not release display output")
		}
		snapshot := shell.Snapshot(0)
		if !bytes.Contains(snapshot.Data, []byte("case nope\r\nordinary output")) {
			t.Fatalf("mismatched output was hidden: %q", snapshot.Data)
		}
	})

	t.Run("incomplete", func(t *testing.T) {
		shell := NewShell("sh_probe_incomplete", "session", "/")
		waiter := &markerWaiter{nonce: strings.Repeat("N", protocol.NonceSize), finalMarker: 'P', ch: make(chan protocol.Marker, 1)}
		shell.mu.Lock()
		shell.markerWaiter = waiter
		shell.beginProtocolDisplayRedactionLocked([]byte("case hidden\n"), 'P', nil)
		changed := shell.changed
		shell.mu.Unlock()

		shell.AppendOutput([]byte("case hid"))
		select {
		case <-changed:
			t.Fatal("partial possible echo became visible before it was resolved")
		default:
		}
		if snapshot := shell.Snapshot(0); snapshot.NextOffset != 0 || len(snapshot.Data) != 0 {
			t.Fatalf("partial possible echo leaked through snapshot: %+v", snapshot)
		}
		_, _, _ = shell.finishProtocol(waiter, 0)
		select {
		case <-changed:
		default:
			t.Fatal("finishing an incomplete probe did not release display output")
		}
		if snapshot := shell.Snapshot(0); !bytes.Equal(snapshot.Data, []byte("case hid")) {
			t.Fatalf("incomplete output was not restored: %q", snapshot.Data)
		}
	})
}

type contextBlockingServeShellProcess struct {
	writes int
	done   chan Exit
}

func (p *contextBlockingServeShellProcess) Write([]byte) (int, error) {
	panic("context-aware write path was bypassed")
}

func (p *contextBlockingServeShellProcess) WriteContext(ctx context.Context, _ []byte) (int, error) {
	p.writes++
	<-ctx.Done()
	return 0, ctx.Err()
}

func (p *contextBlockingServeShellProcess) Resize(int, int) error { return nil }

func (p *contextBlockingServeShellProcess) Close() {}

func (p *contextBlockingServeShellProcess) Done() <-chan Exit { return p.done }

func TestServeShellProbeDeadlineIncludesBlockedPTYWrite(t *testing.T) {
	process := &contextBlockingServeShellProcess{done: make(chan Exit)}
	shell := NewShell("sh_blocked_write", "session", "/")
	shell.process = process
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	started := time.Now()
	err := shell.Probe(ctx)
	cancel()
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) || elapsed > 200*time.Millisecond {
		t.Fatalf("probe err=%v elapsed=%s writes=%d", err, elapsed, process.writes)
	}
	closed := make(chan struct{})
	go func() { shell.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("teardown deadlocked after blocked write")
	}
}

func TestCollaborativeShellCanceledBeforeInjectionWritesNothing(t *testing.T) {
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	process := &recordingServeShellProcess{done: make(chan Exit)}
	shell := NewShell("sh_canceled", "session", "/")
	shell.process = process
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _ = controller.Execute(ctx, "session", tools.SharedShellArgs{
			Command: "printf must-not-run", TimeoutSeconds: 1, ExpectedShellID: shell.id,
		})
	}
	if process.writes != 0 {
		t.Fatalf("canceled calls injected %d writes", process.writes)
	}
}

type partialWriteServeShellProcess struct {
	done chan Exit
}

func (p *partialWriteServeShellProcess) Write(data []byte) (int, error) {
	return len(data) / 2, errors.New("partial write")
}

func (p *partialWriteServeShellProcess) Resize(int, int) error { return nil }

func (p *partialWriteServeShellProcess) Close() {}

func (p *partialWriteServeShellProcess) Done() <-chan Exit { return p.done }

type countingServeShellProcess struct {
	Process
	mu         sync.Mutex
	interrupts int
}

func (p *countingServeShellProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	if len(data) == 1 && data[0] == 3 {
		p.interrupts++
	}
	p.mu.Unlock()
	return p.Process.Write(data)
}

func (p *countingServeShellProcess) interruptCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interrupts
}

type bufferedServeShellProcess struct {
	mu     sync.Mutex
	writes [][]byte
	done   chan Exit
	notify chan struct{}
}

func (p *bufferedServeShellProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes = append(p.writes, append([]byte(nil), data...))
	p.mu.Unlock()
	select {
	case p.notify <- struct{}{}:
	default:
	}
	return len(data), nil
}

func (p *bufferedServeShellProcess) Resize(int, int) error { return nil }

func (p *bufferedServeShellProcess) Close() {}

func (p *bufferedServeShellProcess) Done() <-chan Exit { return p.done }

func (p *bufferedServeShellProcess) snapshotWrites() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.writes))
	for i := range p.writes {
		out[i] = append([]byte(nil), p.writes[i]...)
	}
	return out
}

type queuedInputFailureProcess struct {
	mu             sync.Mutex
	shell          *Shell
	writes         int
	releaseMarkers chan struct{}
	done           chan Exit
}

var serveShellTestNoncePattern = regexp.MustCompile(`[A-Za-z0-9]{32}`)

func (p *queuedInputFailureProcess) Write(data []byte) (int, error) {
	p.mu.Lock()
	p.writes++
	writeNumber := p.writes
	p.mu.Unlock()
	if writeNumber > 1 {
		return 0, errors.New("queued write failed")
	}
	nonce := serveShellTestNoncePattern.FindString(string(data))
	if nonce == "" {
		return 0, errors.New("nonce missing")
	}
	go func() {
		<-p.releaseMarkers
		p.shell.AppendOutput([]byte("\x1b]7770;P;" + nonce + "\x07\x1b]7770;B;" + nonce + "\x07"))
		time.Sleep(25 * time.Millisecond)
		p.shell.AppendOutput([]byte("\x1b]7770;E;" + nonce + ";0\x07"))
	}()
	return len(data), nil
}

func (p *queuedInputFailureProcess) Resize(int, int) error { return nil }

func (p *queuedInputFailureProcess) Close() {}

func (p *queuedInputFailureProcess) Done() <-chan Exit { return p.done }

func TestCollaborativeShellReportsAcceptedBrowserInputDeliveryFailure(t *testing.T) {
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	shell := NewShell("sh_input_failure", "session", "/")
	process := &queuedInputFailureProcess{shell: shell, releaseMarkers: make(chan struct{}), done: make(chan Exit)}
	shell.process = process
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	resultC := make(chan tools.ShellResult, 1)
	errC := make(chan error, 1)
	go func() {
		result, err := controller.Execute(context.Background(), "session", tools.SharedShellArgs{
			Command: "read answer", TimeoutSeconds: 3, ExpectedShellID: shell.id, OutputLimit: 1024,
		})
		resultC <- result
		errC <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		shell.mu.Lock()
		gated := shell.injectionGate
		shell.mu.Unlock()
		if gated || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := shell.writeFrom(writeBrowser, []byte("human reply\n")); err != nil {
		t.Fatal(err)
	}
	close(process.releaseMarkers)
	result, err := <-resultC, <-errC
	if tools.CollaborativeShellErrorKind(err) != "input_rejected" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	shell.mu.Lock()
	state, reason := shell.collaborationState, shell.collaborationReason
	shell.mu.Unlock()
	if state != CollaborationDesynchronized || !strings.Contains(reason, "could not be delivered") {
		t.Fatalf("state=%s reason=%q", state, reason)
	}
}

func TestServeShellInjectionGateFlushesAfterBeginMarker(t *testing.T) {
	process := &bufferedServeShellProcess{done: make(chan Exit), notify: make(chan struct{}, 4)}
	shell := NewShell("sh_gate", "session", "/")
	shell.process = process
	nonce := strings.Repeat("G", protocol.NonceSize)
	payload, err := protocol.BuildCommandPayload(nonce, "read answer")
	if err != nil {
		t.Fatal(err)
	}
	waiter, start, err := shell.startProtocolWrite(context.Background(), writeAgent, nonce, 'E', payload, commandDisplay("read answer"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := shell.writeFrom(writeBrowser, []byte("human")); err != nil {
		t.Fatal(err)
	}
	shell.AppendOutput([]byte("\x1b]7770;P;" + nonce + "\x07"))
	if err := shell.writeFrom(writeBrowser, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if err := shell.writeFrom(writeBrowser, bytes.Repeat([]byte("x"), queuedInputBytes)); !errors.Is(err, ErrInputQueueFull) {
		t.Fatalf("queue overflow error = %v", err)
	}
	if got := len(process.snapshotWrites()); got != 1 {
		t.Fatalf("browser input flushed before B: writes=%d", got)
	}
	shell.AppendOutput([]byte("\x1b]7770;B;" + nonce + "\x07"))
	select {
	case <-process.notify:
	case <-time.After(time.Second):
		t.Fatal("queued browser input was not flushed")
	}
	deadline := time.Now().Add(time.Second)
	for len(process.snapshotWrites()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	writes := process.snapshotWrites()
	if len(writes) != 2 || !bytes.Equal(writes[1], append([]byte("human"), byte(3))) {
		t.Fatalf("queued writes = %#v", writes)
	}
	shell.AppendOutput([]byte("\x1b]7770;E;" + nonce + ";0\x07"))
	_, _, _ = shell.finishProtocol(waiter, start)
	shell.closeInjectionGate(true)
}

func TestServeShellProtocolWaiterIgnoresWrongNonceAndRejectsMalformedCurrent(t *testing.T) {
	nonce := strings.Repeat("N", protocol.NonceSize)
	waiter := &markerWaiter{nonce: nonce, ch: make(chan protocol.Marker, 3)}
	waiter.ch <- protocol.Marker{Kind: 'P', Nonce: strings.Repeat("X", protocol.NonceSize)}
	waiter.ch <- protocol.Marker{Kind: 'P', Nonce: nonce}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := waitMarker(ctx, context.Background(), waiter, 'P'); err != nil {
		t.Fatal(err)
	}
	malformed := &markerWaiter{nonce: nonce, ch: make(chan protocol.Marker, 1)}
	malformed.ch <- protocol.Marker{Kind: 'P', Nonce: nonce, Malformed: true}
	if _, err := waitMarker(ctx, context.Background(), malformed, 'P'); err == nil {
		t.Fatal("malformed current-nonce marker was accepted")
	}
}

func TestCollaborativeShellRejectsCommandWhenBrowserActivityChanged(t *testing.T) {
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	process := &recordingServeShellProcess{done: make(chan Exit)}
	shell := NewShell("sh_activity_fence", "session", "/")
	shell.process = process
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	fence := tools.NewCollaborativeShellActivityFence(shell.nextOffset, shell.browserInputRevision)
	shell.mu.Lock()
	shell.recordBrowserInputLocked([]byte("ls\n"))
	shell.mu.Unlock()
	shell.AppendOutput([]byte("ls\r\nhuman command output\r\nready % "))
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	result, err := controller.Execute(context.Background(), "session", tools.SharedShellArgs{
		Command: "printf stale", TimeoutSeconds: 1, ExpectedShellID: shell.id, OutputLimit: 1024, ActivityFence: fence,
	})
	if tools.CollaborativeShellErrorKind(err) != "terminal_changed" || !strings.Contains(result.Stdout, "human command output") || process.writes != 0 {
		t.Fatalf("result=%+v err=%v writes=%d", result, err, process.writes)
	}
	if got := fence.Offset(); got != shell.nextOffset {
		t.Fatalf("fence offset=%d want %d", got, shell.nextOffset)
	}
}

func TestCollaborativeShellRejectsBrowserInputWithoutCommandOutput(t *testing.T) {
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	process := &recordingServeShellProcess{done: make(chan Exit)}
	shell := NewShell("sh_private_activity_fence", "session", "/")
	shell.process = process
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	fence := tools.NewCollaborativeShellActivityFence(shell.nextOffset, shell.browserInputRevision)
	shell.mu.Lock()
	shell.recordBrowserInputLocked([]byte("cd /tmp\n"))
	shell.mu.Unlock()
	shell.AppendOutput([]byte("cd /tmp\r\nready % "))
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	result, err := controller.Execute(context.Background(), "session", tools.SharedShellArgs{
		Command: "printf stale", TimeoutSeconds: 1, ExpectedShellID: shell.id, OutputLimit: 1024, ActivityFence: fence,
	})
	if tools.CollaborativeShellErrorKind(err) != "terminal_changed" || !strings.Contains(result.Stdout, "cd /tmp") || !strings.Contains(result.Stdout, "ready %") || process.writes != 0 {
		t.Fatalf("result=%+v err=%v writes=%d", result, err, process.writes)
	}
}

func TestCollaborativeShellMissingPrecommandProbeDesynchronizes(t *testing.T) {
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	process := &countingServeShellProcess{Process: &recordingServeShellProcess{done: make(chan Exit)}}
	shell := NewShell("sh_no_probe", "session", "/")
	shell.process = process
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	result, err := controller.Execute(context.Background(), "session", tools.SharedShellArgs{
		Command: "printf unreachable", TimeoutSeconds: 1, ExpectedShellID: shell.id, OutputLimit: 1024,
	})
	if tools.CollaborativeShellErrorKind(err) != "protocol_failure" || result.TimedOut {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	shell.mu.Lock()
	state := shell.collaborationState
	shell.mu.Unlock()
	if state != CollaborationDesynchronized || process.interruptCount() != 1 {
		t.Fatalf("state=%s interrupts=%d", state, process.interruptCount())
	}
}

func TestCollaborativeShellPinnedBindingFailsClosedAfterExitOrReplacement(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Manager, *Shell)
	}{
		{name: "exit", mutate: func(_ *Manager, shell *Shell) { shell.invalidate(1) }},
		{name: "replacement", mutate: func(manager *Manager, shell *Shell) {
			replacement := NewShell("sh_new", "session", "/")
			replacement.process = &recordingServeShellProcess{done: make(chan Exit)}
			manager.mu.Lock()
			manager.shells["session"] = replacement
			manager.mu.Unlock()
			shell.invalidate(-1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(time.Minute, func(string) bool { return true })
			defer manager.Close()
			oldProcess := &recordingServeShellProcess{done: make(chan Exit)}
			oldShell := NewShell("sh_old", "session", "/")
			oldShell.process = oldProcess
			oldShell.collaborationEnabled = true
			oldShell.collaborationState = CollaborationReady
			manager.mu.Lock()
			manager.shells["session"] = oldShell
			manager.mu.Unlock()
			controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
			approval := tools.NewApprovalManager(tools.NewToolPermissions())
			approval.SetApprovalMode(tools.ModeYolo)
			registry, err := tools.NewLocalToolRegistry(&tools.ToolConfig{Enabled: []string{tools.ShellToolName}}, nil, approval)
			if err != nil {
				t.Fatal(err)
			}
			registry.SetCollaborativeShellController(controller, tools.ShellRoutingControllerRequired)
			tool, ok := registry.Get(tools.ShellToolName)
			if !ok {
				t.Fatal("shell tool missing")
			}
			test.mutate(manager, oldShell)
			ctx := core.ContextWithSessionID(context.Background(), "session")
			ctx = tools.ContextWithCollaborativeShellRunBinding(ctx, tools.CollaborativeShellRunBinding{Required: true, ShellID: oldShell.id})
			output, err := tool.Execute(ctx, []byte(`{"command":"printf local-fallback >/definitely/not/a/path"}`))
			if err != nil || !output.IsError || oldProcess.writes != 0 {
				t.Fatalf("output=%+v err=%v old writes=%d", output, err, oldProcess.writes)
			}
		})
	}
}

func TestServeShellPartialProtocolWriteKeepsRecoveryOwnership(t *testing.T) {
	shell := NewShell("sh_partial", "session", "/")
	shell.process = &partialWriteServeShellProcess{done: make(chan Exit)}
	nonce := strings.Repeat("P", protocol.NonceSize)
	waiter, start, err := shell.startProtocolWrite(context.Background(), writeAgent, nonce, 'E', []byte("some payload"), commandDisplay("some command"), 1024)
	if err == nil || waiter == nil || start != 0 {
		t.Fatalf("waiter=%#v start=%d err=%v", waiter, start, err)
	}
	shell.mu.Lock()
	owned := shell.markerWaiter == waiter && shell.injectionGate
	shell.mu.Unlock()
	if !owned {
		t.Fatal("partial write discarded protocol recovery ownership")
	}
}

func TestCollaborativeShellStableContextOrderingAndCompaction(t *testing.T) {
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	shell := NewShell("sh_context", "session", "/")
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	manager.mu.Lock()
	manager.shells["session"] = shell
	manager.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	ctx := tools.ContextWithCollaborativeShellRunBinding(context.Background(), tools.CollaborativeShellRunBinding{Required: true, ShellID: shell.id})
	activity := core.Message{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: `<collaborative_shell_activity id="sha256:one" shell_id="` + shell.id + `">prompt</collaborative_shell_activity>`}}}
	messages := []core.Message{
		{Role: core.RoleDeveloper, Parts: []core.Part{{Type: core.PartText, Text: "platform"}}},
		activity,
		core.UserText("hello"),
	}
	prepared, err := controller.PrepareRequestContext(ctx, "session", messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 4 || CollectLLMText(prepared[0]) != "platform" || CollectLLMText(prepared[1]) != CollaborativeShellInstruction || !strings.Contains(CollectLLMText(prepared[2]), `id="sha256:one"`) {
		t.Fatalf("prepared order = %#v", prepared)
	}
	again, err := controller.PrepareRequestContext(ctx, "session", prepared)
	if err != nil || len(again) != len(prepared) {
		t.Fatalf("dedup prepared len=%d err=%v", len(again), err)
	}
	compaction := &engine.CompactionResult{}
	if err := controller.PrepareCompactionContext(ctx, "session", compaction); err != nil || len(compaction.EphemeralMessages) != 1 || CollectLLMText(compaction.EphemeralMessages[0]) != CollaborativeShellInstruction {
		t.Fatalf("compaction = %#v, err=%v", compaction, err)
	}
	shell.invalidate(-1)
	afterLoss, err := controller.PrepareRequestContext(ctx, "session", []core.Message{core.UserText("no longer shared")})
	if err != nil || len(afterLoss) != 1 || CollectLLMText(afterLoss[0]) != "no longer shared" {
		t.Fatalf("context after shell loss = %#v, %v", afterLoss, err)
	}
	compaction = &engine.CompactionResult{}
	if err := controller.PrepareCompactionContext(ctx, "session", compaction); err != nil || len(compaction.EphemeralMessages) != 0 {
		t.Fatalf("compaction after shell loss = %#v, err=%v", compaction, err)
	}
}

func TestServeShellCapabilityProjectionIsRevisioned(t *testing.T) {
	shell := NewShell("sh_capability", "session", "/")
	unavailable := shell.UpdateCollaborationCapability(false, true, "tool filtered")
	if unavailable.Revision != 1 || unavailable.Sequence != 1 || unavailable.ShellToolAvailable || unavailable.Reason != "tool filtered" {
		t.Fatalf("unavailable snapshot=%+v", unavailable)
	}
	same := shell.UpdateCollaborationCapability(false, true, "tool filtered")
	if same.Revision != unavailable.Revision || same.Sequence != unavailable.Sequence {
		t.Fatalf("unchanged capability advanced authority: %+v", same)
	}
	available := shell.UpdateCollaborationCapability(true, true, "")
	if available.Revision != 2 || available.Sequence != 2 || !available.ShellToolAvailable || available.Reason != "" {
		t.Fatalf("available snapshot=%+v", available)
	}
	events, overrun, _ := shell.CollaborationEventsAfter(1)
	if overrun || len(events) != 1 || events[0].Snapshot == nil || !events[0].Snapshot.ShellToolAvailable {
		t.Fatalf("capability events=%#v overrun=%v", events, overrun)
	}
}

func TestServeShellEventRingOverrunRequiresSnapshotResync(t *testing.T) {
	shell := NewShell("sh_events", "session", "/")
	shell.mu.Lock()
	for i := 0; i < eventRingSize+3; i++ {
		shell.transitionCollaborationLocked(CollaborationReady, true, "collaboration", "")
	}
	shell.mu.Unlock()
	events, overrun, latest := shell.CollaborationEventsAfter(0)
	if !overrun || len(events) != 0 || latest != eventRingSize+3 {
		t.Fatalf("events=%d overrun=%t latest=%d", len(events), overrun, latest)
	}
}

func TestServeShellInvalidationWakesQueuedLease(t *testing.T) {
	shell := NewShell("sh_lease", "session", "/")
	<-shell.commandLease
	done := make(chan error, 1)
	go func() { done <- shell.acquireCommandLease(context.Background()) }()
	shell.invalidate(-1)
	select {
	case err := <-done:
		if tools.CollaborativeShellErrorKind(err) != "stale_shell" {
			t.Fatalf("lease error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued lease was not woken by invalidation")
	}
}

func TestServeShellQueuedLeaseCancellationDoesNotLeak(t *testing.T) {
	shell := NewShell("sh_lease_cancel", "session", "/")
	<-shell.commandLease
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- shell.acquireCommandLease(ctx) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation error = %v", err)
	}
	shell.commandLease <- struct{}{}
	if err := shell.acquireCommandLease(context.Background()); err != nil {
		t.Fatalf("lease leaked after queued cancellation: %v", err)
	}
	shell.commandLease <- struct{}{}
}

func TestServeShellProtocolParserFragmentation(t *testing.T) {
	nonce := strings.Repeat("A", protocol.NonceSize)
	marker := []byte("prefix\x1b]7770;E;" + nonce + ";17\x07suffix")
	for split := 0; split <= len(marker); split++ {
		var parser protocol.Parser
		got := parser.Feed(0, marker[:split])
		got = append(got, parser.Feed(int64(split), marker[split:])...)
		if len(got) != 1 || got[0].Kind != 'E' || got[0].Nonce != nonce || got[0].Status != 17 || got[0].Malformed {
			t.Fatalf("split %d parsed %#v", split, got)
		}
	}
}

func TestServeShellProtocolParserC1AndST(t *testing.T) {
	nonce := strings.Repeat("b", protocol.NonceSize)
	var parser protocol.Parser
	data := append([]byte{0x9d}, []byte("7770;P;"+nonce)...)
	data = append(data, 0x1b, '\\')
	got := parser.Feed(20, data)
	if len(got) != 1 || got[0].Kind != 'P' || got[0].Nonce != nonce {
		t.Fatalf("parsed %#v", got)
	}

	parser = protocol.Parser{}
	data = append([]byte{0x9d}, []byte("unrelated;osc")...)
	data = append(data, 0x9c)
	data = append(data, []byte("\x1b]7770;P;"+nonce+"\x07")...)
	got = parser.Feed(0, data)
	if len(got) != 1 || got[0].Kind != 'P' || got[0].Nonce != nonce {
		t.Fatalf("C1 ST failed to terminate unrelated OSC: %#v", got)
	}
}

func TestServeShellProtocolParserDoesNotTreatUTF8ContinuationAsC1(t *testing.T) {
	nonce := strings.Repeat("U", protocol.NonceSize)
	data := []byte("shell-fie 🐚\r\n\x1b]7770;P;" + nonce + "\x07\x1b]7770;B;" + nonce + "\x07")
	for split := 0; split <= len(data); split++ {
		var parser protocol.Parser
		got := parser.Feed(0, data[:split])
		got = append(got, parser.Feed(int64(split), data[split:])...)
		if len(got) != 2 || got[0].Kind != 'P' || got[1].Kind != 'B' {
			t.Fatalf("split %d parsed %#v", split, got)
		}
	}
}

func TestServeShellCommandPayloadSafety(t *testing.T) {
	nonce := strings.Repeat("Z", protocol.NonceSize)
	command := "printf '%s\\n' \"hello\"\ncat <<'EOF'\n~ssh-safe\n" + strings.Repeat("x", 5000) + "\nEOF"
	payload, err := protocol.BuildCommandPayload(nonce, command)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsAny(payload, "\x1b\x07") {
		t.Fatal("wrapper contains a raw protocol control byte")
	}
	for _, line := range bytes.Split(payload, []byte{'\n'}) {
		if len(line) > protocol.PhysicalLineBytes {
			t.Fatalf("physical line has %d bytes", len(line))
		}
		if bytes.HasPrefix(line, []byte("~")) {
			t.Fatalf("unsafe SSH escape line %q", line)
		}
	}
	if !bytes.Contains(payload, []byte("GIT_PAGER=cat")) || !bytes.Contains(payload, []byte("GIT_TERMINAL_PROMPT=0")) || !bytes.Contains(payload, []byte("export PAGER GIT_PAGER")) {
		t.Fatalf("shared command lacks noninteractive environment isolation: %q", payload)
	}
	if _, err := protocol.BuildCommandPayload(nonce, strings.Repeat("'", 5000)); err != nil {
		t.Fatalf("safely representable quote-heavy command rejected: %v", err)
	}
	if _, err := protocol.BuildCommandPayload(nonce, "bad\x00command"); err == nil {
		t.Fatal("NUL command accepted")
	}
	if _, err := protocol.BuildCommandPayload(nonce, strings.Repeat("x", protocol.CommandBytes+1)); err == nil {
		t.Fatal("oversized command accepted")
	}
}

func TestServeShellCommandPayloadConfiguresPagerEnvironmentAndPreservesState(t *testing.T) {
	shellPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	nonce := strings.Repeat("I", protocol.NonceSize)
	payload, err := protocol.BuildCommandPayload(nonce, "printf 'during=%s:%s\\n' \"$PAGER\" \"$GIT_TERMINAL_PROMPT\"; cd /")
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte("printf 'after=%s cwd=%s\\n' \"$PAGER\" \"$PWD\"\n")...)
	cmd := exec.Command(shellPath)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(cmd.Environ(), "PAGER=original")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("execute payload: %v\n%s", err, output)
	}
	plain, _ := protocol.SanitizeText(output, len(output)*2)
	for _, want := range []string{"during=cat:0", "after=cat cwd=/"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("payload output missing %q: raw=%q plain=%q", want, output, plain)
		}
	}
}

func TestSanitizeServeShellText(t *testing.T) {
	raw := []byte("start\rreplace\n\x1b[31mred\x1b[0m\x1b]0;title\x07\x00ok\xff café\u009c")
	got, _ := protocol.SanitizeText(raw, 1024)
	if got != "replace\nredok café" {
		t.Fatalf("sanitized = %q", got)
	}
}

func TestServeShellCaptureAcrossFragmentedMarkers(t *testing.T) {
	nonce := strings.Repeat("C", protocol.NonceSize)
	shell := NewShell("sh_capture", "session", "/")
	waiter := &markerWaiter{nonce: nonce, finalMarker: 'E', ch: make(chan protocol.Marker, 32)}
	shell.mu.Lock()
	shell.markerWaiter = waiter
	shell.protocolClaimStart = 0
	shell.captureLimit = 1024
	shell.injectionGate = true
	shell.mu.Unlock()
	stream := []byte("echoed\r\n\x1b]7770;P;" + nonce + "\x07\x1b]7770;B;" + nonce + "\x07hello\x1b]7770;E;" + nonce + ";0\x07prompt")
	for _, b := range stream {
		shell.AppendOutput([]byte{b})
	}
	raw, _, end := shell.finishProtocol(waiter, 0)
	got, _ := protocol.SanitizeText(raw, 1024)
	if got != "hello" {
		t.Fatalf("captured = %q (raw %q)", got, raw)
	}
	wantEnd := int64(bytes.Index(stream, []byte("prompt")))
	if end != wantEnd {
		t.Fatalf("claimed end = %d, want marker end %d", end, wantEnd)
	}
	activity, err := shell.reserveActivity(shell.id)
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || !strings.Contains(activity.Excerpt, "prompt") || strings.Contains(activity.Excerpt, "hello") {
		t.Fatalf("activity after fragmented markers = %#v", activity)
	}
}

func TestServeShellCaptureSanitizesFragmentedWrongNonceOSC(t *testing.T) {
	nonce := strings.Repeat("C", protocol.NonceSize)
	wrong := strings.Repeat("W", protocol.NonceSize)
	shell := NewShell("sh_wrong_marker", "session", "/")
	waiter := &markerWaiter{nonce: nonce, finalMarker: 'E', ch: make(chan protocol.Marker, 32)}
	shell.mu.Lock()
	shell.markerWaiter = waiter
	shell.captureLimit = 1024
	shell.mu.Unlock()
	shell.AppendOutput([]byte("\x1b]7770;B;" + nonce + "\x07before\x1b]7770;P;" + wrong[:10]))
	shell.AppendOutput([]byte(wrong[10:] + "\x07after\x1b]7770;E;" + nonce + ";0\x07"))
	raw, _, _ := shell.finishProtocol(waiter, 0)
	got, _ := protocol.SanitizeText(raw, 1024)
	if got != "beforeafter" {
		t.Fatalf("sanitized capture=%q raw=%q", got, raw)
	}
}

func TestSanitizeServeShellActivityTextPreservesZLETranscript(t *testing.T) {
	raw := []byte("\x1b[?2004hsam@arch term-llm % ls\r\n\x1b[?2004l\rAGENTS.md  cmd  frontend\r\n\x1b[?2004hsam@arch term-llm % ")
	got, truncated := sanitizeActivityText(raw, 4096)
	if truncated || !strings.Contains(got, "sam@arch term-llm % ls") || !strings.Contains(got, "AGENTS.md  cmd  frontend") || !strings.Contains(got, "sam@arch term-llm %") || strings.Contains(got, "27J@c%") {
		t.Fatalf("activity transcript=%q truncated=%t", got, truncated)
	}
}

func TestServeShellActivityIncludesOnlyPTYDisplayedBrowserInput(t *testing.T) {
	shell := NewShell("sh_echo", "session", "/")
	shell.mu.Lock()
	shell.recordBrowserInputLocked([]byte("ls\n"))
	shell.mu.Unlock()
	shell.AppendOutput([]byte("ls\r\nAGENTS.md  cmd  frontend\r\nsam@arch term-llm % "))
	activity, err := shell.reserveActivity(shell.id)
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || !strings.Contains(activity.Excerpt, "ls") || !strings.Contains(activity.Excerpt, "AGENTS.md  cmd  frontend") || !strings.Contains(activity.Excerpt, "sam@arch term-llm %") {
		t.Fatalf("displayed browser activity = %#v", activity)
	}
}

func TestServeShellActivityDoesNotExposeUnechoedBrowserInput(t *testing.T) {
	shell := NewShell("sh_no_echo", "session", "/")
	shell.mu.Lock()
	shell.recordBrowserInputLocked([]byte("super-secret-password\n"))
	shell.mu.Unlock()
	// The browser input itself is never copied into activity. If the PTY has echo
	// disabled, only the process output is visible to the agent.
	shell.AppendOutput([]byte("authenticated\r\nsam@arch term-llm % "))
	activity, err := shell.reserveActivity(shell.id)
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || strings.Contains(activity.Excerpt, "super-secret-password") || !strings.Contains(activity.Excerpt, "authenticated") {
		t.Fatalf("unechoed browser activity = %#v", activity)
	}
}

func TestServeShellResultActivityConsumedOnce(t *testing.T) {
	shell := NewShell("sh_result_activity", "session", "/")
	shell.AppendOutput([]byte("lease output\n"))
	end := shell.nextOffset
	first, _ := shell.consumeShellResultActivity(0, end, 1024)
	second, _ := shell.consumeShellResultActivity(0, end, 1024)
	if !strings.Contains(first, "lease output") || second != "" {
		t.Fatalf("first=%q second=%q", first, second)
	}
}

func TestServeShellActivityExcludesClaimedRanges(t *testing.T) {
	shell := NewShell("sh_activity", "session", "/")
	shell.AppendOutput([]byte("human\n"))
	claimedStart := shell.nextOffset
	shell.AppendOutput([]byte("agent secret\n"))
	shell.mu.Lock()
	shell.addClaimedRangeLocked(claimedStart, shell.nextOffset)
	shell.mu.Unlock()
	shell.AppendOutput([]byte("prompt\n"))
	activity, err := shell.reserveActivity(shell.id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(activity.Excerpt, "human") || !strings.Contains(activity.Excerpt, "prompt") || strings.Contains(activity.Excerpt, "agent secret") {
		t.Fatalf("activity excerpt = %q", activity.Excerpt)
	}
	if err := shell.commitActivity(activity.ID); err != nil {
		t.Fatal(err)
	}
	if again, err := shell.reserveActivity(shell.id); err != nil || again != nil {
		t.Fatalf("second reservation = %#v, %v", again, err)
	}
}

func TestServeShellActivityCommitRejectsInvalidatedGeneration(t *testing.T) {
	shell := NewShell("sh_activity_closed", "session", "/")
	shell.AppendOutput([]byte("pending\n"))
	activity, err := shell.reserveActivity(shell.id)
	if err != nil || activity == nil {
		t.Fatalf("reserve=%#v err=%v", activity, err)
	}
	shell.invalidate(-1)
	if err := shell.commitActivity(activity.ID); tools.CollaborativeShellErrorKind(err) != "stale_activity" {
		t.Fatalf("commit after invalidation err=%v", err)
	}
	shell.mu.Lock()
	cursor := shell.activityCursor
	shell.mu.Unlock()
	if cursor != 0 {
		t.Fatalf("invalidated reservation advanced cursor to %d", cursor)
	}
}

func TestServeShellActivityOverrunRetainsRecentTailAndAdvances(t *testing.T) {
	shell := NewShell("sh_overrun", "session", "/")
	shell.collaborationEnabled = true
	shell.collaborationState = CollaborationReady
	for i := 0; i < activitySegments+50; i++ {
		shell.AppendOutput([]byte("segment\n"))
	}
	activity, err := shell.reserveActivity(shell.id)
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || !activity.Truncated || activity.StartOffset == 0 || !strings.Contains(activity.Excerpt, "segment") {
		t.Fatalf("activity = %#v", activity)
	}
	if err := shell.commitActivity(activity.ID); err != nil {
		t.Fatal(err)
	}
	shell.mu.Lock()
	cursor, end := shell.activityCursor, shell.nextOffset
	shell.mu.Unlock()
	if cursor != end {
		t.Fatalf("cursor=%d end=%d", cursor, end)
	}
}

func TestServeShellActivitySingleOversizedBurstRetainsRecentTail(t *testing.T) {
	shell := NewShell("sh_burst", "session", "/")
	burst := append(bytes.Repeat([]byte("x"), activityBytes+4096), []byte("recent-tail\n")...)
	shell.AppendOutput(burst)
	activity, err := shell.reserveActivity(shell.id)
	if err != nil {
		t.Fatal(err)
	}
	if activity == nil || !activity.Truncated || !strings.Contains(activity.Excerpt, "recent-tail") {
		t.Fatalf("activity = %#v", activity)
	}
}

func TestServeShellClaimedRangeMetadataIsBoundedConservatively(t *testing.T) {
	shell := NewShell("sh_ranges", "session", "/")
	shell.mu.Lock()
	for i := 0; i < claimedRanges+20; i++ {
		start := int64(i * 2)
		shell.addClaimedRangeLocked(start, start+1)
	}
	count, floor := len(shell.claimedRanges), shell.activityFloor
	shell.mu.Unlock()
	if count != claimedRanges || floor == 0 {
		t.Fatalf("claimed ranges=%d floor=%d", count, floor)
	}
}

func TestServeShellCaptureIndependentOfReplayRetention(t *testing.T) {
	shell := NewShell("sh_capture_large", "session", "/")
	nonce := strings.Repeat("L", protocol.NonceSize)
	waiter := &markerWaiter{nonce: nonce, finalMarker: 'E', ch: make(chan protocol.Marker, 4)}
	shell.AppendOutput(bytes.Repeat([]byte("p"), 700<<10))
	shell.mu.Lock()
	start := shell.nextOffset
	shell.markerWaiter = waiter
	shell.protocolClaimStart = start
	shell.captureLimit = protocol.CaptureBytes
	shell.mu.Unlock()
	shell.AppendOutput([]byte("\x1b]7770;B;" + nonce + "\x07"))
	commandOutput := bytes.Repeat([]byte("c"), 700<<10)
	shell.AppendOutput(commandOutput)
	shell.AppendOutput([]byte("\x1b]7770;E;" + nonce + ";0\x07"))
	captured, truncated, _ := shell.finishProtocol(waiter, start)
	if truncated || !bytes.Equal(captured, commandOutput) {
		t.Fatalf("capture bytes=%d truncated=%t", len(captured), truncated)
	}
	shell.mu.Lock()
	baseOffset := shell.baseOffset
	shell.mu.Unlock()
	if baseOffset == 0 {
		t.Fatal("browser replay did not overrun")
	}
}

func TestCollaborativeShellRealPTYExecFailsAsShellExit(t *testing.T) {
	if !Supported() {
		t.Skip("PTY unsupported")
	}
	t.Setenv("SHELL", "/bin/sh")
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	shell, _, err := manager.Create("exec", t.TempDir(), 80, 24)
	if err != nil {
		t.Skipf("start PTY: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	if err := shell.Probe(probeCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	shell.mu.Lock()
	shell.transitionCollaborationLocked(CollaborationReady, true, "collaboration", "")
	shell.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	_, err = controller.Execute(context.Background(), "exec", tools.SharedShellArgs{
		Command: "exec false", TimeoutSeconds: 3, ExpectedShellID: shell.id, OutputLimit: 1024,
	})
	if tools.CollaborativeShellErrorKind(err) != "shell_exited" {
		t.Fatalf("exec error = %v", err)
	}
	shell.mu.Lock()
	state, enabled := shell.collaborationState, shell.collaborationEnabled
	shell.mu.Unlock()
	if state != CollaborationOff || enabled {
		t.Fatalf("exec state=%s enabled=%t", state, enabled)
	}
}

func TestCollaborativeShellRealPTYErrexitFailsClosed(t *testing.T) {
	if !Supported() {
		t.Skip("PTY unsupported")
	}
	t.Setenv("SHELL", "/bin/sh")
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	shell, _, err := manager.Create("errexit", t.TempDir(), 80, 24)
	if err != nil {
		t.Skipf("start PTY: %v", err)
	}
	if err := shell.writeFrom(writeBrowser, []byte("set -e\n")); err != nil {
		t.Fatal(err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err = shell.Probe(probeCtx)
	cancel()
	if err == nil {
		t.Fatal("probe succeeded with inherited errexit")
	}

	// Recreate a clean shell, enable sharing, and then let an agent command make
	// the protocol-incompatible state change. Missing E must fail closed.
	if err := manager.CloseShell("errexit", shell.id); err != nil {
		t.Fatal(err)
	}
	shell, _, err = manager.Create("errexit", t.TempDir(), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	probeCtx, cancel = context.WithTimeout(context.Background(), 750*time.Millisecond)
	if err := shell.Probe(probeCtx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	shell.mu.Lock()
	shell.transitionCollaborationLocked(CollaborationReady, true, "collaboration", "")
	shell.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	result, executeErr := controller.Execute(context.Background(), "errexit", tools.SharedShellArgs{
		Command: "set -e; false", TimeoutSeconds: 1, ExpectedShellID: shell.id, OutputLimit: 1 << 20,
	})
	shell.mu.Lock()
	state := shell.collaborationState
	shell.mu.Unlock()
	if executeErr == nil || (!result.RecoveryFailed && tools.CollaborativeShellErrorKind(executeErr) != "shell_exited") || (state != CollaborationDesynchronized && state != CollaborationOff) {
		t.Fatalf("errexit result=%+v err=%v state=%s", result, executeErr, state)
	}
}

// Cancellation is a protocol/state-machine contract, not a test of OS job-control
// timing. Keep real PTY persistence coverage below, but drive interruption and
// recovery with explicit markers and a fake clock (including failed recovery).
func TestCollaborativeShellCommandCancellation(t *testing.T) {
	for _, action := range []string{"interrupt", "timeout", "disable", "close"} {
		for _, recover := range []bool{true, false} {
			if action == "close" && !recover {
				continue // A closed terminal cannot be probed.
			}
			name := action + "/recovered"
			if !recover {
				name = action + "/recovery_failed"
			} else if action == "close" {
				name = action
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					manager := NewManager(time.Minute, func(string) bool { return true })
					defer manager.Close()
					shell := NewShell("sh_cancel", "session", "/")
					process := &bufferedServeShellProcess{done: make(chan Exit)}
					shell.process = process
					shell.collaborationEnabled = true
					shell.collaborationState = CollaborationReady
					manager.mu.Lock()
					manager.shells["session"] = shell
					manager.mu.Unlock()
					controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
					resultC := make(chan tools.ShellResult, 1)
					errC := make(chan error, 1)
					go func() {
						result, err := controller.Execute(context.Background(), "session", tools.SharedShellArgs{
							Command: "long-running-command", TimeoutSeconds: 1, ExpectedShellID: shell.id, OutputLimit: 1024,
						})
						resultC <- result
						errC <- err
					}()
					synctest.Wait()
					writes := process.snapshotWrites()
					if len(writes) != 1 {
						t.Fatalf("command writes = %q", writes)
					}
					nonce := serveShellTestNoncePattern.FindString(string(writes[0]))
					if nonce == "" {
						t.Fatal("command nonce missing")
					}
					shell.AppendOutput([]byte("\x1b]7770;P;" + nonce + "\x07\x1b]7770;B;" + nonce + "\x07running"))
					synctest.Wait() // Execute has consumed P/B and is waiting for E.
					shell.mu.Lock()
					commandID := shell.commandID
					shell.mu.Unlock()
					operationC := make(chan error, 1)
					switch action {
					case "interrupt":
						go func() { operationC <- shell.InterruptCommand(context.Background(), commandID) }()
					case "disable":
						go func() { operationC <- shell.DisableCollaboration(context.Background()) }()
					case "timeout":
						time.Sleep(time.Second) // Fake time: expire the command's own deadline.
					case "close":
						shell.close()
					}
					synctest.Wait()
					if action != "close" {
						writes = process.snapshotWrites()
						if len(writes) != 2 || !bytes.Equal(writes[1], []byte{3}) {
							t.Fatalf("expected one Ctrl+C after command, writes = %q", writes)
						}
						time.Sleep(75 * time.Millisecond) // Advance the recovery settling interval.
						synctest.Wait()
						writes = process.snapshotWrites()
						if len(writes) != 3 {
							t.Fatalf("recovery probe writes = %q", writes)
						}
						probeNonce := serveShellTestNoncePattern.FindString(string(writes[2]))
						if probeNonce == "" || probeNonce == nonce {
							t.Fatal("recovery must use a fresh nonce")
						}
						// A late marker from the canceled command must not establish recovery.
						shell.AppendOutput([]byte("\x1b]7770;P;" + nonce + "\x07"))
						synctest.Wait()
						select {
						case result := <-resultC:
							t.Fatalf("command completed before recovery: %+v", result)
						case err := <-operationC:
							t.Fatalf("cancellation operation completed before recovery: %v", err)
						default:
						}
						if recover {
							shell.AppendOutput([]byte("\x1b]7770;P;" + probeNonce + "\x07"))
						} else {
							time.Sleep(2 * time.Second) // No probe reply: exhaust the recovery deadline.
						}
						synctest.Wait()
					}
					result, err := <-resultC, <-errC
					if action == "interrupt" || action == "disable" {
						if err := <-operationC; err != nil {
							t.Fatalf("%s: %v", action, err)
						}
					}
					wantState, wantEnabled := CollaborationReady, true
					if action == "close" {
						if tools.CollaborativeShellErrorKind(err) != "shell_exited" || shell.interruptWriteCount() != 0 {
							t.Fatalf("close result = %+v, %v; interrupts = %d", result, err, shell.interruptWriteCount())
						}
					} else {
						if result.Canceled != (action != "timeout") || result.TimedOut != (action == "timeout") || result.RecoveryFailed != !recover || result.Stdout != "running" {
							t.Fatalf("%s result = %+v, %v", action, result, err)
						}
						if recover && err != nil || !recover && tools.CollaborativeShellErrorKind(err) != "recovery_failed" {
							t.Fatalf("recovery error = %v", err)
						}
						if shell.interruptWriteCount() != 1 {
							t.Fatalf("Ctrl+C writes = %d", shell.interruptWriteCount())
						}
						if !recover {
							wantState = CollaborationDesynchronized
						}
					}
					if action == "close" || action == "disable" {
						wantState, wantEnabled = CollaborationOff, false
					}
					shell.mu.Lock()
					state, enabled, commandID := shell.collaborationState, shell.collaborationEnabled, shell.commandID
					shell.mu.Unlock()
					if state != wantState || enabled != wantEnabled || commandID != "" || len(shell.commandLease) != 1 {
						t.Fatalf("state=%s enabled=%t command=%q lease=%d", state, enabled, commandID, len(shell.commandLease))
					}
				})
			})
		}
	}
}

func TestCollaborativeShellRealPTYStatePersistence(t *testing.T) {
	if !Supported() {
		t.Skip("PTY unsupported")
	}
	t.Setenv("SHELL", "/bin/sh")
	manager := NewManager(time.Minute, func(string) bool { return true })
	defer manager.Close()
	shell, _, err := manager.Create("session", t.TempDir(), 80, 24)
	if err != nil {
		t.Skipf("start PTY: %v", err)
	}
	if err := shell.writeFrom(writeBrowser, []byte("export BROWSER_COLLAB=browser; cd /; sh -i\n")); err != nil {
		t.Fatal(err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	if err := shell.Probe(probeCtx); err != nil {
		cancel()
		t.Fatalf("probe: %v", err)
	}
	cancel()
	shell.mu.Lock()
	shell.transitionCollaborationLocked(CollaborationReady, true, "collaboration", "")
	shell.mu.Unlock()
	controller := &Controller{Manager: func() (*Manager, error) { return manager, nil }}
	ctx := tools.ContextWithCollaborativeShellRunBinding(context.Background(), tools.CollaborativeShellRunBinding{Required: true, ShellID: shell.id})
	<-shell.commandLease
	leaseResult := make(chan tools.ShellResult, 1)
	leaseErr := make(chan error, 1)
	go func() {
		result, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
			Command: "printf lease-acquired", TimeoutSeconds: 1, ExpectedShellID: shell.id, OutputLimit: 1 << 20,
		})
		leaseResult <- result
		leaseErr <- err
	}()
	time.Sleep(1100 * time.Millisecond)
	shell.commandLease <- struct{}{}
	if result, err := <-leaseResult, <-leaseErr; err != nil || !strings.Contains(result.Stdout, "lease-acquired") || result.TimedOut {
		t.Fatalf("post-lease timeout result = %+v, %v", result, err)
	}
	command := "printf 'browser=%s:%s\\n' \"$BROWSER_COLLAB\" \"$PWD\"; export COLLAB_TEST=visible; printf '%s:%s' \"$COLLAB_TEST\" \"$PWD\""
	result, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
		Command: command, TimeoutSeconds: 3,
		ExpectedShellID: shell.id, OutputLimit: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Stdout, "browser=browser:/") || !strings.Contains(result.Stdout, "visible:/") {
		t.Fatalf("result = %+v", result)
	}
	var replay bytes.Buffer
	for offset := int64(0); ; {
		snapshot := shell.Snapshot(offset)
		if snapshot.Reset {
			offset = snapshot.BaseOffset
			continue
		}
		if len(snapshot.Data) == 0 {
			break
		}
		replay.Write(snapshot.Data)
		offset += int64(len(snapshot.Data))
	}
	if !bytes.Contains(replay.Bytes(), commandDisplay(command)) || bytes.Contains(replay.Bytes(), []byte("GIT_TERMINAL_PROMPT=0")) || !bytes.Contains(replay.Bytes(), []byte("browser=browser:/")) || bytes.Count(replay.Bytes(), []byte{0}) == 0 {
		t.Fatalf("browser/SSE replay omitted the clean command or exposed its wrapper")
	}
	shell.mu.Lock()
	browserCheckOffset := shell.nextOffset
	shell.mu.Unlock()
	if err := shell.writeFrom(writeBrowser, []byte("printf 'browser-sees=%s:%s\\n' \"$COLLAB_TEST\" \"$PWD\"\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	browserObserved := false
	var browserReplay bytes.Buffer
	for !browserObserved && time.Now().Before(deadline) {
		snapshot := shell.Snapshot(browserCheckOffset)
		if snapshot.Reset {
			browserCheckOffset = snapshot.BaseOffset
			browserReplay.Reset()
		} else if len(snapshot.Data) > 0 {
			browserReplay.Write(snapshot.Data)
			browserObserved = bytes.Contains(browserReplay.Bytes(), []byte("browser-sees=visible:/"))
			browserCheckOffset += int64(len(snapshot.Data))
		}
		if !browserObserved {
			time.Sleep(time.Millisecond)
		}
	}
	if !browserObserved {
		t.Fatal("browser command did not observe agent-established shell state")
	}
	second, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
		Command: "printf '%s:%s' \"$COLLAB_TEST\" \"$PWD\"", TimeoutSeconds: 3,
		ExpectedShellID: shell.id, OutputLimit: 1 << 20,
	})
	if err != nil || !strings.Contains(second.Stdout, "visible:/") {
		t.Fatalf("persistent result = %+v, %v", second, err)
	}
	nonzero, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
		Command: "false", TimeoutSeconds: 3, ExpectedShellID: shell.id, OutputLimit: 1 << 20,
	})
	if err != nil || nonzero.ExitCode != 1 {
		t.Fatalf("non-zero result = %+v, %v", nonzero, err)
	}
	sanitized, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
		Command: `printf '\033[31mred\033[0m\n\033]0;title\007old\rnew\n\001\377'`, TimeoutSeconds: 3,
		ExpectedShellID: shell.id, OutputLimit: 1 << 20,
	})
	if err != nil || sanitized.ExitCode != 0 || !strings.Contains(sanitized.Stdout, "red\nnew\n") || strings.Contains(sanitized.Stdout, "old") || strings.Contains(sanitized.Stdout, "title") || strings.ContainsAny(sanitized.Stdout, "\x1b\x01") || !utf8.ValidString(sanitized.Stdout) {
		t.Fatalf("sanitized PTY result = %+v, %v", sanitized, err)
	}
	longValue := strings.Repeat("x", 6000)
	multilineCommand := "value=$(printf '%s' \"sub\")\ncat <<'EOF'\n~ssh-safe\nEOF\nlong='" + longValue + "'\nprintf 'multi=%s:%s:%s\\n' \"$value\" \"$(printf double)\" \"${#long}\""
	multiline, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
		Command: multilineCommand, TimeoutSeconds: 3, ExpectedShellID: shell.id, OutputLimit: 1 << 20,
	})
	if err != nil || multiline.ExitCode != 0 || !strings.Contains(multiline.Stdout, "~ssh-safe") || !strings.Contains(multiline.Stdout, "multi=sub:double:6000") {
		t.Fatalf("multiline result = %+v, %v", multiline, err)
	}

	funny, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
		Command: "printf 'The terminal passed its shell-fie exam. 🐚\\n'", TimeoutSeconds: 3,
		ExpectedShellID: shell.id, OutputLimit: 1 << 20,
	})
	if err != nil || funny.ExitCode != 0 || !strings.Contains(funny.Stdout, "shell-fie exam. 🐚") {
		t.Fatalf("Unicode printf result = %+v, %v", funny, err)
	}

	interactive := make(chan tools.ShellResult, 1)
	interactiveErr := make(chan error, 1)
	go func() {
		result, err := controller.Execute(ctx, "session", tools.SharedShellArgs{
			Command: "read answer; printf 'answer=%s' \"$answer\"", TimeoutSeconds: 3,
			ExpectedShellID: shell.id, OutputLimit: 1 << 20,
		})
		interactive <- result
		interactiveErr <- err
	}()
	deadline = time.Now().Add(2 * time.Second)
	for {
		shell.mu.Lock()
		running := shell.collaborationState == CollaborationAgentRunning
		shell.mu.Unlock()
		if running || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := shell.writeFrom(writeBrowser, []byte("human reply\n")); err != nil {
		t.Fatal(err)
	}
	if result, err := <-interactive, <-interactiveErr; err != nil || !strings.Contains(result.Stdout, "answer=human reply") {
		t.Fatalf("interactive result = %+v, %v", result, err)
	}
}
