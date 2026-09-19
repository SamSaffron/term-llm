package cmd

import (
	"testing"
	"time"
)

func TestParseLiveDecisionsSince(t *testing.T) {
	now := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	got, err := parseLiveDecisionsSince("24h", now)
	if err != nil || !got.Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("duration since = %v, %v", got, err)
	}
	got, err = parseLiveDecisionsSince("2026-09-18T00:00:00Z", now)
	if err != nil || got.Format(time.RFC3339) != "2026-09-18T00:00:00Z" {
		t.Fatalf("timestamp since = %v, %v", got, err)
	}
	if _, err := parseLiveDecisionsSince("yesterday", now); err == nil {
		t.Fatal("invalid --since succeeded")
	}
}
