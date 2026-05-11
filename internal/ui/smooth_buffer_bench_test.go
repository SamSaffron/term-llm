package ui

import (
	"strings"
	"testing"
)

func BenchmarkSmoothBufferDrainBacklog(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"4KB", 4 * 1024},
		{"16KB", 16 * 1024},
		{"64KB", 64 * 1024},
	} {
		content := strings.Repeat("word ", tc.size/len("word "))
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				buf := NewSmoothBuffer()
				buf.Write(content)

				emitted := 0
				for !buf.IsEmpty() {
					chunk := buf.NextWords()
					if chunk == "" {
						b.Fatal("NextWords returned empty chunk before buffer drained")
					}
					emitted += len(chunk)
				}
				if emitted != len(content) {
					b.Fatalf("emitted %d bytes, want %d", emitted, len(content))
				}
			}
		})
	}
}
