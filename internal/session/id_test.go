package session

import (
	"strings"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	id := NewID()

	// Should be in format YYYYMMDD-HHMMSS-RANDOM
	if len(id) != 22 {
		t.Errorf("NewID() = %q, expected length 22", id)
	}

	// Should start with current year (20XX)
	if !strings.HasPrefix(id, "20") {
		t.Errorf("NewID() = %q, expected to start with '20'", id)
	}

	// Should have hyphens in correct positions
	if id[8] != '-' || id[15] != '-' {
		t.Errorf("NewID() = %q, expected hyphens at positions 8 and 15", id)
	}
}

func TestParseIDTime(t *testing.T) {
	tests := []struct {
		id       string
		wantYear int
		wantOK   bool
	}{
		{"20240115-143052-a1b2c3", 2024, true},
		{"20231225-000000-ffffff", 2023, true},
		{"short", 0, false},
		{"", 0, false},
	}

	for _, tt := range tests {
		got := ParseIDTime(tt.id)
		if tt.wantOK {
			if got.Year() != tt.wantYear {
				t.Errorf("ParseIDTime(%q).Year() = %d, want %d", tt.id, got.Year(), tt.wantYear)
			}
		} else {
			if !got.IsZero() {
				t.Errorf("ParseIDTime(%q) = %v, want zero time", tt.id, got)
			}
		}
	}
}

func TestShortID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"20240115-143052-a1b2c3", "240115-1430"},
		{"20231225-000000-ffffff", "231225-0000"},
		{"short", "short"}, // Too short, returned as-is
		{"", ""},
	}

	for _, tt := range tests {
		got := ShortID(tt.input)
		if got != tt.expected {
			t.Errorf("ShortID(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExpandShortID(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"240115-1430", "20240115-1430%"},                    // Standard short ID
		{"20240115-143052-a1b2c3", "20240115-143052-a1b2c3"}, // Full ID unchanged
		{"240115", "240115%"},                                // Partial - just add wildcard
		{"abc", "abc%"},                                      // Random string - add wildcard
		{"260128-1234", "20260128-1234%"},                    // Future date short ID
	}

	for _, tt := range tests {
		got := ExpandShortID(tt.input)
		if got != tt.expected {
			t.Errorf("ExpandShortID(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestShortIDRoundTrip(t *testing.T) {
	// Test that ExpandShortID can match the prefix of an ID created by NewID
	// when given the ShortID of that ID
	id := NewID()
	short := ShortID(id)
	expanded := ExpandShortID(short)

	// The expanded pattern (without the %) should be a prefix of the original ID
	prefix := strings.TrimSuffix(expanded, "%")
	if !strings.HasPrefix(id, prefix) {
		t.Errorf("Round trip failed: ID=%q, ShortID=%q, ExpandShortID=%q, prefix=%q does not match",
			id, short, expanded, prefix)
	}
}

func TestNewIDUniqueness(t *testing.T) {
	// Generate multiple IDs quickly and ensure they're unique
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := NewID()
		if ids[id] {
			t.Errorf("NewID() generated duplicate ID: %s", id)
		}
		ids[id] = true
		time.Sleep(time.Microsecond) // Small delay to ensure different timestamps
	}
}
