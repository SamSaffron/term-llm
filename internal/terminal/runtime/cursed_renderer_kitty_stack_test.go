package tea

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

type kittyNoInputModel struct{}

func (kittyNoInputModel) Init() Cmd { return Quit }

func (m kittyNoInputModel) Update(Msg) (Model, Cmd) { return m, nil }

func (kittyNoInputModel) View() View { return NewView("no input") }

// kittyStackOpRe matches the Kitty keyboard stack operations the renderer emits:
// CSI > flags u pushes an entry and CSI < n u pops entries. The in-place update
// CSI = flags ; mode u is deliberately excluded: it is not stack traffic.
var kittyStackOpRe = regexp.MustCompile(`\x1b\[[<>][0-9]*u`)

// kittyStackOp is one push or pop in the byte order it reached the terminal.
type kittyStackOp struct {
	seq string
	at  int
}

func (op kittyStackOp) String() string {
	return fmt.Sprintf("%q@%d", op.seq, op.at)
}

// kittyStackOps returns the Kitty keyboard stack operations in got in order.
func kittyStackOps(got string) []kittyStackOp {
	matches := kittyStackOpRe.FindAllStringIndex(got, -1)
	ops := make([]kittyStackOp, 0, len(matches))
	for _, m := range matches {
		ops = append(ops, kittyStackOp{seq: got[m[0]:m[1]], at: m[0]})
	}
	return ops
}

// assertKittyStackBalance checks exact accounting for the Kitty keyboard stack
// traffic in got: every push carries wantPush, every pop removes exactly one
// entry, and pushes and pops strictly alternate. The alternation is what makes
// the accounting exact - a second push before its matching pop drives the depth
// past one, and a pop with nothing pushed drives it below zero; both fail, as
// does a lifecycle that ends unbalanced. It returns the operations in order so
// callers can assert where they land relative to the frames.
func assertKittyStackBalance(t *testing.T, got, wantPush string) []kittyStackOp {
	t.Helper()

	ops := kittyStackOps(got)
	if len(ops) == 0 {
		t.Fatalf("no Kitty keyboard stack operations in output: %q", got)
	}

	depth, pushes, pops := 0, 0, 0
	for i, op := range ops {
		switch {
		case strings.HasPrefix(op.seq, "\x1b[>"):
			pushes++
			depth++
			if op.seq != wantPush {
				t.Errorf("Kitty keyboard push %d is %q at %d, want %q", pushes, op.seq, op.at, wantPush)
			}
			if depth > 1 {
				t.Errorf("Kitty keyboard stack depth reached %d at operation %d (%s): %d pushes without an intervening pop",
					depth, i+1, op, depth)
			}
		case strings.HasPrefix(op.seq, "\x1b[<"):
			pops++
			depth--
			if want := ansi.PopKittyKeyboard(1); op.seq != want {
				t.Errorf("Kitty keyboard pop %d is %q at %d, want %q: a pop must remove exactly one entry", pops, op.seq, op.at, want)
			}
			if depth < 0 {
				t.Errorf("Kitty keyboard pop %d (%s) has no matching push", pops, op)
			}
		}
	}
	if pushes != pops {
		t.Errorf("Kitty keyboard stack left unbalanced: %d pushes, %d pops (ops %v)", pushes, pops, ops)
	}
	return ops
}

func TestCursedRendererPushesAndPopsKittyKeyboardStack(t *testing.T) {
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, 20, 4)
	view := NewView("keyboard stack")
	push := ansi.PushKittyKeyboard(keyboardEnhancementsFlags(view.KeyboardEnhancements))

	r.start()
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := r.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := string(bytesJoin(output.snapshot()))
	ops := assertKittyStackBalance(t, got, push)
	if len(ops) != 2 {
		t.Fatalf("start/stop cycle emitted %d Kitty keyboard stack operations, want exactly one push and one pop: %v", len(ops), ops)
	}
	if ops[0].seq != push {
		t.Errorf("first stack operation is %s, want the push %q", ops[0], push)
	}
	if want := ansi.PopKittyKeyboard(1); ops[1].seq != want {
		t.Errorf("last stack operation is %s, want the pop %q", ops[1], want)
	}

	// The entry is pushed for the screen being drawn and popped when the
	// renderer stops, so the frame sits between the two operations.
	contentAt := strings.Index(got, view.Content)
	if contentAt < 0 {
		t.Fatalf("frame content missing from output: %q", got)
	}
	if contentAt < ops[0].at {
		t.Errorf("frame content at %d precedes the push at %d: %q", contentAt, ops[0].at, got)
	}
	if contentAt > ops[1].at {
		t.Errorf("frame content at %d follows the pop at %d: %q", contentAt, ops[1].at, got)
	}
}

func TestCursedRendererStacksKittyKeyboardPerScreenAcrossScreenSwitch(t *testing.T) {
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, 20, 4)
	mainView := NewView("main screen")
	altView := NewView("alt screen")
	altView.AltScreen = true
	push := ansi.PushKittyKeyboard(keyboardEnhancementsFlags(mainView.KeyboardEnhancements))

	r.start()
	for _, view := range []View{mainView, altView, mainView} {
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	if err := r.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := string(bytesJoin(output.snapshot()))
	// Strict push/pop alternation is the per-screen balance: the main entry is
	// popped before the alt screen takes over and the alt entry is popped
	// before the main screen does, so no screen ever leaves an entry behind and
	// no pop ever runs without a push.
	ops := assertKittyStackBalance(t, got, push)
	if len(ops) != 6 {
		t.Fatalf("screen switch cycle emitted %d Kitty keyboard stack operations, want 3 pushes and 3 pops: %v", len(ops), ops)
	}

	enterAlt := strings.Index(got, ansi.SetModeAltScreenSaveCursor)
	exitAlt := strings.Index(got, ansi.ResetModeAltScreenSaveCursor)
	if enterAlt < 0 || exitAlt < 0 {
		t.Fatalf("scenario did not switch screens (enter %d, exit %d): %q", enterAlt, exitAlt, got)
	}

	// Each screen's frame is bracketed by its own push and pop, and the pops
	// are written while the registry they belong to is still active.
	mainAt := strings.Index(got, mainView.Content)
	altAt := strings.Index(got, altView.Content)
	if mainAt < 0 || altAt < 0 {
		t.Fatalf("frame content missing from output: %q", got)
	}
	if mainAt < ops[0].at || mainAt > ops[1].at {
		t.Errorf("main frame at %d is not between the main push %s and pop %s", mainAt, ops[0], ops[1])
	}
	if !(ops[1].at < enterAlt && enterAlt < ops[2].at) {
		t.Errorf("alt screen entry at %d does not follow the main pop %s and precede the alt push %s", enterAlt, ops[1], ops[2])
	}
	if altAt < ops[2].at || altAt > ops[3].at {
		t.Errorf("alt frame at %d is not between the alt push %s and pop %s", altAt, ops[2], ops[3])
	}
	if !(ops[3].at < exitAlt && exitAlt < ops[4].at) {
		t.Errorf("alt screen exit at %d does not follow the alt pop %s and precede the main push %s", exitAlt, ops[3], ops[4])
	}
	if ops[5].at < exitAlt {
		t.Errorf("the stop pop %s at %d does not come after the alt screen exit at %d", ops[5], ops[5].at, exitAlt)
	}
}

func TestCursedRendererStacksKittyKeyboardAcrossStopStart(t *testing.T) {
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, 20, 4)
	view := NewView("restart stack")
	push := ansi.PushKittyKeyboard(keyboardEnhancementsFlags(view.KeyboardEnhancements))

	// A suspend/resume cycle: the stop pops the entry for the screen in use and
	// the start pushes a fresh one for the screen being restored, so the stack
	// never accrues a second entry for the same screen.
	for cycle := 0; cycle < 2; cycle++ {
		r.start()
		r.render(view)
		if err := r.flush(false); err != nil {
			t.Fatalf("flush %d: %v", cycle, err)
		}
		if err := r.close(); err != nil {
			t.Fatalf("close %d: %v", cycle, err)
		}
	}

	ops := assertKittyStackBalance(t, string(bytesJoin(output.snapshot())), push)
	if len(ops) != 4 {
		t.Fatalf("stop/start cycle emitted %d Kitty keyboard stack operations, want 2 pushes and 2 pops: %v", len(ops), ops)
	}
}

func TestKittyStackPushPrecedesPopWhenStoppedBeforeFlush(t *testing.T) {
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, 20, 4)
	view := NewView("stop before flush")
	push := ansi.PushKittyKeyboard(keyboardEnhancementsFlags(view.KeyboardEnhancements))
	pop := ansi.PopKittyKeyboard(1)

	// A suspend/resume where nothing is drawn in between: start() queues the
	// fresh push for the restored screen, and the following stop must not
	// overtake it. If the stop writes its pop before the queued push reaches
	// the terminal, the terminal sees a pop with no matching entry and the
	// real entry is left orphaned on the stack.
	r.start()
	r.render(view)
	if err := r.flush(false); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := r.close(); err != nil {
		t.Fatalf("suspend close: %v", err)
	}
	r.start()
	if err := r.close(); err != nil {
		t.Fatalf("stop close: %v", err)
	}

	got := string(bytesJoin(output.snapshot()))
	ops := assertKittyStackBalance(t, got, push)
	if len(ops) != 4 {
		t.Fatalf("suspend/resume/stop emitted %d Kitty keyboard stack operations, want 2 pushes and 2 pops: %v", len(ops), ops)
	}
	if ops[2].seq != push {
		t.Errorf("third stack operation is %s, want the resume push %q: %q", ops[2], push, got)
	}
	if ops[3].seq != pop {
		t.Errorf("last stack operation is %s, want the stop pop %q: %q", ops[3], pop, got)
	}
	if ops[3].at < ops[2].at {
		t.Errorf("the stop pop %s at %d precedes the resume push %s at %d: %q", ops[3], ops[3].at, ops[2], ops[2].at, got)
	}
}

func TestCursedRendererUpdatesKittyKeyboardFlagsInPlace(t *testing.T) {
	output := &boundaryWriter{}
	r := newCursedRenderer(output, []string{"TERM=xterm-256color"}, 20, 4)
	first := NewView("flags update")
	second := NewView("flags update")
	second.KeyboardEnhancements.ReportEventTypes = true
	firstPush := ansi.PushKittyKeyboard(keyboardEnhancementsFlags(first.KeyboardEnhancements))
	wantUpdate := ansi.KittyKeyboard(keyboardEnhancementsFlags(second.KeyboardEnhancements), 1)

	r.start()
	r.render(first)
	if err := r.flush(false); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	mid := len(output.snapshot())
	if ops := kittyStackOps(string(bytesJoin(output.snapshot()))); len(ops) != 1 || ops[0].seq != firstPush {
		t.Fatalf("first frame emitted %v, want exactly the push %q", ops, firstPush)
	}

	r.render(second)
	if err := r.flush(false); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	// (a) The flags change on the same screen must update the top entry in
	// place. The KittyKeyboard CSI = sequence is not stack traffic, so it is
	// asserted by containment rather than by the push/pop accounting below.
	secondFrame := string(bytesJoin(output.snapshot()[mid:]))
	if !strings.Contains(secondFrame, wantUpdate) {
		t.Errorf("second frame missing in-place update %q for the new flags: %q", wantUpdate, secondFrame)
	}

	// (b) The in-place update must not churn the stack: the second frame adds
	// no push or pop, so the total stack traffic is unchanged.
	if ops := kittyStackOps(secondFrame); len(ops) != 0 {
		t.Errorf("flags change pushed/popped the Kitty stack: %v in %q", ops, secondFrame)
	}
	if ops := kittyStackOps(string(bytesJoin(output.snapshot()))); len(ops) != 1 {
		t.Errorf("flags change altered stack traffic: %d operations, want 1 push before close: %v", len(ops), ops)
	}

	if err := r.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// (b)+(c) The whole lifecycle stays balanced with the per-screen depth
	// never exceeding one: exactly the first push and the stop pop. The
	// helper fails on a second push (depth 2, unbalanced) as well as on any
	// pop without a push.
	got := string(bytesJoin(output.snapshot()))
	ops := assertKittyStackBalance(t, got, firstPush)
	if len(ops) != 2 {
		t.Fatalf("flags-update cycle emitted %d Kitty keyboard stack operations, want one push and one pop: %v", len(ops), ops)
	}
	updateAt := strings.Index(got, wantUpdate)
	if updateAt < 0 {
		t.Fatalf("in-place update %q missing from full output: %q", wantUpdate, got)
	}
	if updateAt < ops[0].at || updateAt > ops[1].at {
		t.Errorf("in-place update at %d is not between the push %s and pop %s", updateAt, ops[0], ops[1])
	}
}

func TestProgramWithNoInputSkipsKeyboardEnhancementSequences(t *testing.T) {
	var output bytes.Buffer
	p := NewProgram(
		kittyNoInputModel{},
		WithInput(nil),
		WithOutput(&output),
		WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		WithoutSignalHandler(),
	)

	if _, err := p.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := output.String()
	keyboardSequences := []string{
		ansi.SetModifyOtherKeys2,
		ansi.ResetModifyOtherKeys,
		ansi.RequestKittyKeyboard,
		ansi.KittyKeyboard(0, 1),
		ansi.KittyKeyboard(1, 1),
		ansi.PushKittyKeyboard(1),
		ansi.PopKittyKeyboard(1),
	}
	for _, seq := range keyboardSequences {
		if strings.Contains(got, seq) {
			t.Errorf("output contains keyboard enhancement sequence %q: %q", seq, got)
		}
	}
}
