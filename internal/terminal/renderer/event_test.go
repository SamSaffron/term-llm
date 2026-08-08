package uv

import (
	"image"
	"image/color"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

// TestUnknownEventString tests String methods for all Unknown event types
func TestUnknownEventString(t *testing.T) {
	tests := []struct {
		name  string
		event interface{ String() string }
		want  string
	}{
		{
			name:  "UnknownEvent",
			event: UnknownEvent("test"),
			want:  `"test"`,
		},
		{
			name:  "UnknownCsiEvent",
			event: UnknownCsiEvent("csi"),
			want:  `"csi"`,
		},
		{
			name:  "UnknownSs3Event",
			event: UnknownSs3Event("ss3"),
			want:  `"ss3"`,
		},
		{
			name:  "UnknownOscEvent",
			event: UnknownOscEvent("osc"),
			want:  `"osc"`,
		},
		{
			name:  "UnknownDcsEvent",
			event: UnknownDcsEvent("dcs"),
			want:  `"dcs"`,
		},
		{
			name:  "UnknownSosEvent",
			event: UnknownSosEvent("sos"),
			want:  `"sos"`,
		},
		{
			name:  "UnknownPmEvent",
			event: UnknownPmEvent("pm"),
			want:  `"pm"`,
		},
		{
			name:  "UnknownApcEvent",
			event: UnknownApcEvent("apc"),
			want:  `"apc"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.event.String()
			if got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMultiEventString tests the String method for MultiEvent
func TestMultiEventString(t *testing.T) {
	events := MultiEvent{
		KeyPressEvent{Code: 'a'},
		KeyPressEvent{Code: 'b'},
		KeyPressEvent{Code: 'c'},
	}

	got := events.String()
	want := "a\nb\nc\n"
	if got != want {
		t.Errorf("MultiEvent.String() = %q, want %q", got, want)
	}
}

// TestSizeBounds tests the Bounds method for Size and related types
func TestSizeBounds(t *testing.T) {
	tests := []struct {
		name string
		size interface{ Bounds() Rectangle }
		want Rectangle
	}{
		{
			name: "Size",
			size: Size{Width: 80, Height: 24},
			want: Rectangle{
				Min: image.Point{X: 0, Y: 0},
				Max: image.Point{X: 80, Y: 24},
			},
		},
		{
			name: "WindowSizeEvent",
			size: WindowSizeEvent{Width: 100, Height: 50},
			want: Rectangle{
				Min: image.Point{X: 0, Y: 0},
				Max: image.Point{X: 100, Y: 50},
			},
		},
		{
			name: "WindowPixelSizeEvent",
			size: PixelSizeEvent{Width: 1920, Height: 1080},
			want: Rectangle{
				Min: image.Point{X: 0, Y: 0},
				Max: image.Point{X: 1920, Y: 1080},
			},
		},
		{
			name: "CellSizeEvent",
			size: CellSizeEvent{Width: 10, Height: 20},
			want: Rectangle{
				Min: image.Point{X: 0, Y: 0},
				Max: image.Point{X: 10, Y: 20},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.size.Bounds()
			if got != tt.want {
				t.Errorf("Bounds() = %v, want %v", got, tt.want)
			}
		})
	}
}

type keyEventMethods interface {
	MatchString(...string) bool
	String() string
	Keystroke() string
	Key() Key
}

func TestKeyEventMethods(t *testing.T) {
	tests := []struct {
		name   string
		event  keyEventMethods
		want   string
		other  string
		other2 string
		key    Key
	}{
		{name: "press", event: KeyPressEvent{Code: 'a', Mod: ModCtrl}, want: "ctrl+a", other: "ctrl+b", other2: "ctrl+c", key: Key{Code: 'a', Mod: ModCtrl}},
		{name: "release", event: KeyReleaseEvent{Code: 'b', Mod: ModAlt}, want: "alt+b", other: "alt+a", other2: "alt+c", key: Key{Code: 'b', Mod: ModAlt}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.event.MatchString(tc.want) || tc.event.MatchString(tc.other) {
				t.Fatalf("MatchString mismatch for %s", tc.want)
			}
			if !tc.event.MatchString(tc.other, tc.want) || tc.event.MatchString(tc.other, tc.other2) {
				t.Fatalf("multi MatchString mismatch for %s", tc.want)
			}
			if got := tc.event.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			if got := tc.event.Keystroke(); got != tc.want {
				t.Errorf("Keystroke() = %q, want %q", got, tc.want)
			}
			if got := tc.event.Key(); got != tc.key {
				t.Errorf("Key() = %v, want %v", got, tc.key)
			}
		})
	}
}

// TestMouseEventMethods tests all mouse event types and their methods
func TestMouseEventMethods(t *testing.T) {
	mouse := Mouse{X: 10, Y: 20, Button: MouseLeft}

	t.Run("MouseClickEvent", func(t *testing.T) {
		e := MouseClickEvent(mouse)
		if got := e.String(); got != "left" {
			t.Errorf("String() = %q, want %q", got, "left")
		}
		if got := e.Mouse(); got != mouse {
			t.Errorf("Mouse() = %v, want %v", got, mouse)
		}
	})

	t.Run("MouseReleaseEvent", func(t *testing.T) {
		e := MouseReleaseEvent(mouse)
		if got := e.String(); got != "left" {
			t.Errorf("String() = %q, want %q", got, "left")
		}
		if got := e.Mouse(); got != mouse {
			t.Errorf("Mouse() = %v, want %v", got, mouse)
		}
	})

	t.Run("MouseWheelEvent", func(t *testing.T) {
		e := MouseWheelEvent(Mouse{X: 10, Y: 20, Button: MouseWheelUp})
		if got := e.String(); got != "wheelup" {
			t.Errorf("String() = %q, want %q", got, "wheelup")
		}
		if got := e.Mouse(); got.Button != MouseWheelUp {
			t.Errorf("Mouse().Button = %v, want %v", got.Button, MouseWheelUp)
		}
	})

	t.Run("MouseMotionEvent", func(t *testing.T) {
		// Test with button
		e := MouseMotionEvent(mouse)
		if got := e.String(); got != "left+motion" {
			t.Errorf("String() = %q, want %q", got, "left+motion")
		}
		if got := e.Mouse(); got != mouse {
			t.Errorf("Mouse() = %v, want %v", got, mouse)
		}

		// Test without button
		e2 := MouseMotionEvent(Mouse{X: 10, Y: 20, Button: 0})
		if got := e2.String(); got != "motion" {
			t.Errorf("String() = %q, want %q", got, "motion")
		}
	})
}

// TestKittyEnhancementsEventContains tests the Contains method
func TestKittyEnhancementsEventContains(t *testing.T) {
	e := KeyboardEnhancementsEvent{0b111} // Has bits 0, 1, and 2 set

	tests := []struct {
		name         string
		enhancements int
		want         bool
	}{
		{
			name:         "has single bit",
			enhancements: 0b001,
			want:         true,
		},
		{
			name:         "has multiple bits",
			enhancements: 0b011,
			want:         true,
		},
		{
			name:         "has all bits",
			enhancements: 0b111,
			want:         true,
		},
		{
			name:         "missing bit",
			enhancements: 0b1000,
			want:         false,
		},
		{
			name:         "partially missing bits",
			enhancements: 0b1011,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := e.Contains(tt.enhancements)
			if got != tt.want {
				t.Errorf("Contains(%b) = %v, want %v", tt.enhancements, got, tt.want)
			}
		})
	}
}

// TestColorEventMethods tests color event methods
func TestColorEventMethods(t *testing.T) {
	redColor := color.RGBA{R: 255, G: 0, B: 0, A: 255}
	blackColor := color.RGBA{R: 0, G: 0, B: 0, A: 255}
	whiteColor := color.RGBA{R: 255, G: 255, B: 255, A: 255}

	t.Run("ForegroundColorEvent", func(t *testing.T) {
		e := ForegroundColorEvent{redColor}
		if got := e.String(); got != "#ff0000" {
			t.Errorf("String() = %q, want %q", got, "#ff0000")
		}
		if got := e.IsDark(); got != false {
			t.Errorf("IsDark() = %v, want %v", got, false)
		}

		e2 := ForegroundColorEvent{blackColor}
		if got := e2.IsDark(); got != true {
			t.Errorf("IsDark() = %v, want %v", got, true)
		}
	})

	t.Run("BackgroundColorEvent", func(t *testing.T) {
		e := BackgroundColorEvent{whiteColor}
		if got := e.String(); got != "#ffffff" {
			t.Errorf("String() = %q, want %q", got, "#ffffff")
		}
		if got := e.IsDark(); got != false {
			t.Errorf("IsDark() = %v, want %v", got, false)
		}

		e2 := BackgroundColorEvent{blackColor}
		if got := e2.IsDark(); got != true {
			t.Errorf("IsDark() = %v, want %v", got, true)
		}
	})

	t.Run("CursorColorEvent", func(t *testing.T) {
		e := CursorColorEvent{redColor}
		if got := e.String(); got != "#ff0000" {
			t.Errorf("String() = %q, want %q", got, "#ff0000")
		}
		if got := e.IsDark(); got != false {
			t.Errorf("IsDark() = %v, want %v", got, false)
		}

		e2 := CursorColorEvent{blackColor}
		if got := e2.IsDark(); got != true {
			t.Errorf("IsDark() = %v, want %v", got, true)
		}
	})
}

// TestClipboardEventString tests the String method for ClipboardEvent
func TestClipboardEventString(t *testing.T) {
	e := ClipboardEvent{
		Content:   "test content",
		Selection: SystemClipboard,
	}

	got := e.String()
	want := "test content"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestEventTypes tests that various event types exist and can be created
func TestEventTypes(t *testing.T) {
	// Test that these types can be instantiated without panic
	_ = CursorPositionEvent(image.Point{X: 10, Y: 20})
	_ = FocusEvent{}
	_ = BlurEvent{}
	_ = DarkColorSchemeEvent{}
	_ = LightColorSchemeEvent{}
	_ = PasteEvent{"pasted text"}
	_ = PasteStartEvent{}
	_ = PasteEndEvent{}
	_ = TerminalVersionEvent{"1.0.0"}
	_ = ModifyOtherKeysEvent{1}
	_ = KittyGraphicsEvent{
		Options: kitty.Options{},
		Payload: []byte("test"),
	}
	_ = PrimaryDeviceAttributesEvent{1, 2, 3}
	_ = SecondaryDeviceAttributesEvent{1, 2, 3}
	_ = TertiaryDeviceAttributesEvent("test")
	_ = ModeReportEvent{
		Mode:  ansi.DECMode(1000),
		Value: ansi.ModeSet,
	}
	_ = WindowOpEvent{
		Op:   1,
		Args: []int{100, 200},
	}
	_ = CapabilityEvent{"RGB"}
	_ = ignoredEvent("ignored")
}
