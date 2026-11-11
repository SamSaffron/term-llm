package tea

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// All sleeps in this file advance synctest's virtual clock, not wall time.
type parkedRenderer struct {
	*cursedRenderer
	ticks atomic.Int32
}

func (r *parkedRenderer) flush(closing bool) (bool, error) {
	if !closing {
		r.ticks.Add(1)
	}
	return r.cursedRenderer.flush(closing)
}

type parkViewMsg struct{ view View }
type parkAckMsg struct{}

type parkModel struct{ view View }

func (m *parkModel) Init() Cmd { return nil }
func (m *parkModel) Update(msg Msg) (Model, Cmd) {
	switch msg := msg.(type) {
	case parkViewMsg:
		m.view = msg.view
	case parkAckMsg:
		m.view.PostFrame = ""
		m.view.PostFrameMsg = nil
	}
	return m, nil
}
func (m *parkModel) View() View { return m.view }

type parkHarness struct {
	p    *Program
	r    *parkedRenderer
	out  *boundaryWriter
	done chan error
}

func newParkHarness(t *testing.T, fps int) *parkHarness {
	t.Helper()
	out := &boundaryWriter{}
	r := &parkedRenderer{cursedRenderer: newBoundaryRenderer(out)}
	r.setNoInput(true)
	p := NewProgram(&parkModel{view: NewView("first")},
		WithInput(nil), WithOutput(out), WithWindowSize(80, 24), WithFPS(fps),
		WithEnvironment([]string{"TERM=xterm-256color", "TERM_PROGRAM=Apple_Terminal"}),
		WithoutSignalHandler())
	p.renderer = r
	h := &parkHarness{p: p, r: r, out: out, done: make(chan error, 1)}
	go func() { _, err := p.Run(); h.done <- err }()
	synctest.Wait()
	return h
}

func (h *parkHarness) park(t *testing.T) {
	t.Helper()
	// Even at 1 FPS this covers the first frame and the idle threshold.
	time.Sleep(3 * time.Second)
	synctest.Wait()
	n := h.r.ticks.Load()
	if n == 0 {
		t.Fatal("renderer never flushed")
	}
	time.Sleep(2 * time.Second)
	synctest.Wait()
	if got := h.r.ticks.Load() - n; got != 0 {
		t.Fatalf("idle renderer flushed %d times, want 0", got)
	}
}

func (h *parkHarness) stop() {
	h.p.Kill()
	<-h.done
}

func (h *parkHarness) send(msg Msg) {
	h.p.Send(msg)
	synctest.Wait()
}

// parkNoopExec exercises the real release/restore path without an OS process.
type parkNoopExec struct{}

func (parkNoopExec) Run() error          { return nil }
func (parkNoopExec) SetStdin(io.Reader)  {}
func (parkNoopExec) SetStdout(io.Writer) {}
func (parkNoopExec) SetStderr(io.Writer) {}

func TestRendererParksWhenIdle(t *testing.T) {
	for _, fps := range []int{1, 60, 120} {
		t.Run(strconv.Itoa(fps), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newParkHarness(t, fps)
				defer h.stop()
				h.park(t)
			})
		})
	}
}

func TestParkedRendererWakes(t *testing.T) {
	cases := []struct {
		name string
		do   func(*parkHarness)
		want string
	}{
		{"view", func(h *parkHarness) { h.send(parkViewMsg{NewView("second")}) }, "second"},
		{"raw", func(h *parkHarness) { h.send(Raw("<raw-wake>")()) }, "<raw-wake>"},
		// No model Update follows execute: it must wake the loop itself.
		{"execute", func(h *parkHarness) { h.p.execute("<execute-wake>") }, "<execute-wake>"},
		{"print", func(h *parkHarness) { h.send(Println("printed")()) }, "printed"},
		{"clear", func(h *parkHarness) { h.send(ClearScreen()) }, "first"},
		{"resize", func(h *parkHarness) { h.send(WindowSizeMsg{Width: 90, Height: 25}) }, "first"},
		{"exec", func(h *parkHarness) {
			h.send(execCommandCmd(parkNoopExec{}, nil)())
			// releaseTerminal bounds the join of the disabled input reader at 500 ms.
			time.Sleep(500 * time.Millisecond)
			synctest.Wait()
		}, ansi.SetModeBracketedPaste},
		{"suspend", func(h *parkHarness) {
			h.p.suspendFunc = func() {}
			h.send(SuspendMsg{})
			time.Sleep(500 * time.Millisecond)
			synctest.Wait()
		}, ansi.SetModeBracketedPaste},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newParkHarness(t, 60)
				defer h.stop()
				h.park(t)
				mark := len(h.out.snapshot())
				tc.do(h)
				time.Sleep(time.Second / 60)
				synctest.Wait()
				if got := bytes.Join(h.out.snapshot()[mark:], nil); !bytes.Contains(got, []byte(tc.want)) {
					t.Fatalf("wake did not write %q within one frame: %q", tc.want, got)
				}
				h.park(t)
			})
		})
	}
}

func TestParkedRendererDeliversUnchangedViewPostFrame(t *testing.T) {
	for _, payload := range []string{"", testPostFrame} {
		t.Run(payload, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newParkHarness(t, 60)
				defer h.stop()
				h.park(t)
				mark := len(h.out.snapshot())
				acks := 0
				v := NewView("first")
				v.PostFrame = payload
				v.PostFrameMsg = func(err error) Msg {
					if err != nil {
						t.Errorf("post-frame error: %v", err)
					}
					acks++
					return parkAckMsg{}
				}
				h.send(parkViewMsg{v})
				time.Sleep(time.Second / 60)
				synctest.Wait()
				if acks != 1 {
					t.Fatalf("post-frame callbacks = %d, want 1", acks)
				}
				if payload != "" && writeContaining(h.out.snapshot(), payload, mark) < 0 {
					t.Fatal("unchanged view lost post-frame payload")
				}
				h.park(t)
				// Processing the ack returns an unchanged text view with the
				// consumed side-band fields cleared. It must park again.
				if acks != 1 {
					t.Fatalf("post-frame callback repeated %d times", acks)
				}
			})
		})
	}
}

func TestParkedRendererShutdown(t *testing.T) {
	for _, kill := range []bool{false, true} {
		t.Run(map[bool]string{false: "quit", true: "kill"}[kill], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newParkHarness(t, 60)
				defer h.p.Kill()
				h.park(t)
				mark := len(h.out.snapshot())
				h.send(parkViewMsg{NewView("last-frame")})
				if kill {
					h.p.Kill()
				} else {
					h.p.Quit()
				}
				synctest.Wait()
				if err := <-h.done; (kill && !errors.Is(err, ErrProgramKilled)) || (!kill && err != nil) {
					t.Fatalf("Run error: %v", err)
				}
				if painted := writeContaining(h.out.snapshot(), "last-frame", mark) >= 0; painted == kill {
					t.Fatalf("final frame painted = %v, kill = %v", painted, kill)
				}
			})
		})
	}
}
func TestParkedRendererKeepsFrameBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// At 10 FPS the harness parks at exactly an original frame boundary.
		h := newParkHarness(t, 10)
		defer h.stop()
		h.park(t)
		time.Sleep(25 * time.Millisecond)
		mark := len(h.out.snapshot())
		h.p.execute("<aligned-wake>")
		synctest.Wait()
		time.Sleep(75*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if writeContaining(h.out.snapshot(), "<aligned-wake>", mark) >= 0 {
			t.Fatal("parked renderer flushed before its next frame boundary")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if writeContaining(h.out.snapshot(), "<aligned-wake>", mark) < 0 {
			t.Fatal("parked renderer missed its original frame boundary")
		}

		// After the shortened wake interval, restore the normal frame period.
		h.send(parkViewMsg{NewView("next-aligned-frame")})
		time.Sleep(100*time.Millisecond - time.Nanosecond)
		synctest.Wait()
		if writeContaining(h.out.snapshot(), "next-aligned-frame", mark) >= 0 {
			t.Fatal("renderer retained the shortened interval after waking")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if writeContaining(h.out.snapshot(), "next-aligned-frame", mark) < 0 {
			t.Fatal("renderer did not resume its normal frame interval")
		}
	})
}
