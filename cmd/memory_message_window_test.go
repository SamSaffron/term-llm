package cmd

import (
	"testing"
)

func TestMemoryMessageTimeFlag(t *testing.T) {
	for _, value := range []string{"", "2026-10-22T12:53:00+11:00", "2026-10-22T01:53:00.123Z"} {
		if _, err := parseMemoryMessageTime("message-since", value); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := parseMemoryMessageTime("message-since", "2026-10-22T12:53:00+11:00")
	b, _ := parseMemoryMessageTime("message-since", "2026-10-22T01:53:00Z")
	if !a.Equal(b) {
		t.Fatal("timezone mismatch")
	}
	for _, value := range []string{"yesterday", "24h", "2026-10-22", "2026-10-22T12:53:00", "0001-01-01T00:00:00Z"} {
		if _, err := parseMemoryMessageTime("message-before", value); err == nil {
			t.Fatalf("accepted invalid bound %s", value)
		}
	}
	for _, flag := range []string{"message-since", "message-before"} {
		f := memoryMineCmd.Flags().Lookup(flag)
		if f == nil || f.DefValue != "" || f.Value.Type() != "string" {
			t.Fatalf("flag binding %s", flag)
		}
	}
}
