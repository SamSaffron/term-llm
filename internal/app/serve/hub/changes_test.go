package hub

import (
	"strconv"
	"testing"
	"time"
)

func TestChangesTimeout(t *testing.T) {
	type testCase struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}
	tests := []testCase{
		{name: "default", value: "", want: 15 * time.Second},
		{name: "whitespace default", value: "  ", want: 15 * time.Second},
		{name: "zero", value: "0", want: 0},
		{name: "seconds", value: "7", want: 7 * time.Second},
		{name: "trimmed", value: " 7 ", want: 7 * time.Second},
		{name: "maximum", value: "30", want: 30 * time.Second},
		{name: "clamped", value: "31", want: 30 * time.Second},
		{name: "maximum integer", value: strconv.Itoa(int(^uint(0) >> 1)), want: 30 * time.Second},
		{name: "negative", value: "-1", wantErr: true},
		{name: "fraction", value: "0.5", wantErr: true},
		{name: "invalid", value: "abc", wantErr: true},
		{name: "integer overflow", value: "99999999999999999999999999", wantErr: true},
	}
	if strconv.IntSize == 64 {
		tests = append(tests, testCase{name: "duration overflow", value: "9223372037", want: 30 * time.Second})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := changesTimeout(tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("changesTimeout(%q) error = %v, wantErr %v", tt.value, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("changesTimeout(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
