package uv

import (
	"reflect"
	"testing"
)

// The predicate upstream relaxed is "the first parameter need not be 1", so a
// modified function key must behave like a modified navigation key: the
// event-type subparameter decides repeat versus release, and the press decode
// for the same key stays the reference for the modifier and key code.
func TestParseKittyModifiedFunctionKeyRelease(t *testing.T) {
	decode := func(seq []byte) []Event {
		t.Helper()
		var p EventDecoder
		var events []Event
		buf := seq
		for len(buf) > 0 {
			width, ev := p.Decode(buf)
			switch ev := ev.(type) {
			case MultiEvent:
				events = append(events, ev...)
			default:
				events = append(events, ev)
			}
			buf = buf[width:]
		}
		return events
	}

	press := decode([]byte("\x1b[15;2~"))
	if len(press) != 1 {
		t.Fatalf("expected one event for the F5 press, got %#v", press)
	}
	p, ok := press[0].(KeyPressEvent)
	if !ok {
		t.Fatalf("expected a key press for the F5 press, got %#v", press[0])
	}

	repeat := decode([]byte("\x1b[15;2:2~"))
	if len(repeat) != 1 {
		t.Fatalf("expected one event for the F5 repeat, got %#v", repeat)
	}
	if r, ok := repeat[0].(KeyPressEvent); !ok || !r.IsRepeat || r.Code != p.Code || r.Mod != p.Mod {
		t.Errorf("F5 repeat = %#v, want a repeat press of %#v", repeat[0], p)
	}

	release := decode([]byte("\x1b[15;2:3~"))
	if len(release) != 1 {
		t.Fatalf("expected one event for the F5 release, got %#v", release)
	}
	if r, ok := release[0].(KeyReleaseEvent); !ok || r.Code != p.Code || r.Mod != p.Mod {
		t.Errorf("F5 release = %#v, want a release of %#v", release[0], p)
	}
}

// Regression test for upstream 6cf7526 "fix(kitty-keyboard): modified
// navigation/function key releases".
//
// Sequences for modified navigation/function keys whose first parameter is
// not 1 (e.g. "\x1b[6;6:2~") must still honor the event-type subparameter:
// ":2" is a repeat, ":3" is a release.
func TestParseKittyModifiedKeyReleases(t *testing.T) {
	tests := []struct {
		name string
		seq  []byte
		want []Event
	}{
		{
			name: "Shift+Ctrl+PageDown repeat",
			seq:  []byte("\x1b[6;6:2~"),
			want: []Event{KeyPressEvent{Mod: ModShift | ModCtrl, Code: KeyPgDown, IsRepeat: true}},
		},
		{
			name: "Shift+Ctrl+PageDown release",
			seq:  []byte("\x1b[6;6:3~"),
			want: []Event{KeyReleaseEvent{Mod: ModShift | ModCtrl, Code: KeyPgDown}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p EventDecoder
			var events []Event
			buf := tc.seq
			for len(buf) > 0 {
				width, ev := p.Decode(buf)
				switch ev := ev.(type) {
				case MultiEvent:
					events = append(events, ev...)
				default:
					events = append(events, ev)
				}
				buf = buf[width:]
			}
			if len(tc.want) != len(events) {
				t.Fatalf("\nexpected %d events for %q:\n    %#v\ngot %d:\n    %#v", len(tc.want), tc.seq, tc.want, len(events), events)
			}
			for i := range tc.want {
				if !reflect.DeepEqual(tc.want[i], events[i]) {
					t.Errorf("\nexpected event %d for %q:\n    %#v\ngot:\n    %#v", i, tc.seq, tc.want[i], events[i])
				}
			}
		})
	}
}
