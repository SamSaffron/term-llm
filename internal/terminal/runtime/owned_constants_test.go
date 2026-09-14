package tea

import (
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

// Backport regression tests for upstream fixes:
//   - 1862dfb ("fix: assign MouseButton11 = uv.MouseButton11 (#1754)")
//   - dc4b017 ("fix(key): map media record to ultraviolet code (#1757)")
//
// Both defects are Go implicit constant repetition: a bare constant name
// with no "= <value>" silently repeats the previous expression instead of
// mapping the ultraviolet value.

func TestOwnedMouseButton11Mapping(t *testing.T) {
	t.Logf("MouseButton11=%d uv.MouseButton11=%d MouseButton10=%d", int(MouseButton11), int(uv.MouseButton11), int(MouseButton10))
	if MouseButton11 != uv.MouseButton11 {
		t.Errorf("MouseButton11 mismatch: got %d, want uv.MouseButton11 %d", int(MouseButton11), int(uv.MouseButton11))
	}
	if MouseButton11 == MouseButton10 {
		t.Errorf("MouseButton11 not distinct: got %d, same as MouseButton10 %d", int(MouseButton11), int(MouseButton10))
	}
	if got, want := MouseButton11.String(), "button11"; got != want {
		t.Errorf("MouseButton11.String() mismatch: got %q, want %q", got, want)
	}
}

func TestOwnedKeyMediaRecordMapping(t *testing.T) {
	t.Logf("KeyMediaRecord=%d uv.KeyMediaRecord=%d KeyMediaPrev=%d", int(KeyMediaRecord), int(uv.KeyMediaRecord), int(KeyMediaPrev))
	if KeyMediaRecord != uv.KeyMediaRecord {
		t.Errorf("KeyMediaRecord mismatch: got %d, want uv.KeyMediaRecord %d", int(KeyMediaRecord), int(uv.KeyMediaRecord))
	}
	if KeyMediaRecord == KeyMediaPrev {
		t.Errorf("KeyMediaRecord not distinct: got %d, same as KeyMediaPrev %d", int(KeyMediaRecord), int(KeyMediaPrev))
	}
	if got, want := (Key{Code: KeyMediaRecord}).String(), "mediarecord"; got != want {
		t.Errorf("Key{Code: KeyMediaRecord}.String() mismatch: got %q, want %q", got, want)
	}
	if got, want := (Key{Code: KeyMediaRecord}).Keystroke(), "mediarecord"; got != want {
		t.Errorf("Key{Code: KeyMediaRecord}.Keystroke() mismatch: got %q, want %q", got, want)
	}
}
