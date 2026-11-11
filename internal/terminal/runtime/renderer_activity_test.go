package tea

import (
	"errors"
	"testing"
)

func TestCursedRendererFlushReportsActivity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload bool
		factory bool
		fail    bool
	}{
		{name: "unchanged"},
		{name: "payload", payload: true},
		{name: "factory", factory: true},
		{name: "payload-and-factory", payload: true, factory: true},
		{name: "failed-side-band", payload: true, factory: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &boundaryWriter{}
			r := newBoundaryRenderer(out)
			r.render(NewView(testRendererFrame))
			if active, err := r.flush(false); !active || err != nil {
				t.Fatalf("initial flush = %v, %v", active, err)
			}
			frameErr := errors.New("side-band write failed")
			if tc.fail {
				out.failExact, out.failErr = []byte(testPostFrame), frameErr
			}
			v := NewView(testRendererFrame)
			if tc.payload {
				v.PostFrame = testPostFrame
			}
			callbacks := 0
			if tc.factory {
				v.PostFrameMsg = func(err error) Msg {
					callbacks++
					if tc.fail != errors.Is(err, frameErr) || (!tc.fail && err != nil) {
						t.Errorf("callback error = %v, failed write = %v", err, tc.fail)
					}
					return nil
				}
			}
			r.render(v)
			if active, err := r.flush(false); active != (tc.payload || tc.factory) || err != nil {
				t.Fatalf("unchanged-view flush = %v, %v", active, err)
			}
			if (callbacks == 1) != tc.factory {
				t.Fatalf("callbacks = %d, factory = %v", callbacks, tc.factory)
			}
			// A failed side-band write must keep the scheduler active until the
			// invalidated screen is repainted, even with no new model message.
			if active, err := r.flush(false); active != tc.fail || err != nil {
				t.Fatalf("next flush = %v, %v; recovery = %v", active, err, tc.fail)
			}
			if active, err := r.flush(false); active || err != nil {
				t.Fatalf("consumed side-band flush = %v, %v", active, err)
			}
		})
	}
}
