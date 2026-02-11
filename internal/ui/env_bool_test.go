package ui

import "testing"

func TestParseBoolDefault(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		defaultValue bool
		want         bool
	}{
		{name: "true-1", raw: "1", defaultValue: false, want: true},
		{name: "true-word", raw: "true", defaultValue: false, want: true},
		{name: "true-yes", raw: "yes", defaultValue: false, want: true},
		{name: "false-0", raw: "0", defaultValue: true, want: false},
		{name: "false-word", raw: "false", defaultValue: true, want: false},
		{name: "false-no", raw: "no", defaultValue: true, want: false},
		{name: "empty-default-true", raw: "", defaultValue: true, want: true},
		{name: "empty-default-false", raw: "", defaultValue: false, want: false},
		{name: "unknown-default-true", raw: "maybe", defaultValue: true, want: true},
		{name: "unknown-default-false", raw: "maybe", defaultValue: false, want: false},
		{name: "trim-and-case", raw: "  TrUe ", defaultValue: false, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseBoolDefault(tt.raw, tt.defaultValue)
			if got != tt.want {
				t.Fatalf("ParseBoolDefault(%q, %t) = %t, want %t", tt.raw, tt.defaultValue, got, tt.want)
			}
		})
	}
}
