package cmd

import (
	"testing"
	"time"
)

func BenchmarkResponseRunAppendTextDeltaRetainedReplay(b *testing.B) {
	run := newResponseRun("resp_bench", "sess_bench", "", "mock", time.Now().Unix(), func() {})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := run.appendTextDeltaSegmentEvent(0, 0, ""); err != nil {
			b.Fatalf("appendTextDeltaSegmentEvent failed: %v", err)
		}
	}
}
