package jobs

import (
	"testing"
	"time"
)

func TestNextCronTime(t *testing.T) {
	after := time.Date(2026, 1, 5, 10, 7, 30, 0, time.UTC)
	tests := []struct {
		expr string
		tz   string
		want time.Time
	}{
		{"*/15 * * * *", "UTC", time.Date(2026, 1, 5, 10, 15, 0, 0, time.UTC)},
		{"0 9 * * 1-5", "UTC", time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)},
		{"30 12 1,15 * *", "UTC", time.Date(2026, 1, 15, 12, 30, 0, 0, time.UTC)},
		{"0 0 * * *", "Australia/Sydney", time.Date(2026, 1, 5, 13, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		got, err := NextCronTime(tt.expr, tt.tz, after)
		if err != nil {
			t.Fatalf("%q: %v", tt.expr, err)
		}
		if !got.Equal(tt.want) {
			t.Fatalf("%q: got %v want %v", tt.expr, got, tt.want)
		}
	}
}

func TestValidateCronExpressionRejects(t *testing.T) {
	for _, expr := range []string{"", "* * * *", "60 * * * *", "*/0 * * * *", "5-1 * * * *", "a * * * *", "1,,2 * * * *"} {
		if err := ValidateCronExpression(expr); err == nil {
			t.Fatalf("%q: expected error", expr)
		}
	}
	if err := ValidateCronExpression("*/5 1-3 * 1,6 0"); err != nil {
		t.Fatal(err)
	}
}

func TestEffectiveCronTimezonePrefersSchedule(t *testing.T) {
	if got := EffectiveCronTimezone(" UTC ", " Europe/Paris "); got != "Europe/Paris" {
		t.Fatalf("got %q", got)
	}
	if got := EffectiveCronTimezone(" UTC ", ""); got != "UTC" {
		t.Fatalf("got %q", got)
	}
}
