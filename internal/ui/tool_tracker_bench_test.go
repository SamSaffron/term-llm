package ui

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	streamingRenderBenchmarkWidth          = 100
	streamingRenderBenchmarkFlushThreshold = 4096
)

var streamingRenderBenchmarkSink int

// BenchmarkTUIStreamingRender replays a growing Markdown response through the
// same ToolTracker -> TextSegmentRenderer -> StreamRenderer path used for live
// TUI responses. Every token-like append is followed by a View render and the
// normal incremental scrollback flush check.
//
// The corpus contains exactly the requested number of top-level blocks. It
// cycles through ATX headings, inline-rich paragraphs, fenced code blocks,
// tables, and thematic breaks. Chunk sizes cycle through 17, 31, 47, and 73
// bytes, deliberately splitting lines and inline markup as a token stream does.
func BenchmarkTUIStreamingRender(b *testing.B) {
	for _, blocks := range []int{32, 128, 512, 1024} {
		b.Run(fmt.Sprintf("blocks=%d", blocks), func(b *testing.B) {
			corpus := streamingRenderBenchmarkCorpus(blocks)
			chunks := streamingRenderBenchmarkChunks(corpus)

			// Warm the Markdown renderer's internal caches and collect a separate
			// representative frame-latency sample without contaminating ns/op.
			streamingRenderBenchmarkReplay(b, chunks, nil)
			frameDurations := make([]time.Duration, 0, len(chunks))
			streamingRenderBenchmarkReplay(b, chunks, &frameDurations)
			sort.Slice(frameDurations, func(i, j int) bool {
				return frameDurations[i] < frameDurations[j]
			})

			p50FrameNS := float64(framePercentile(frameDurations, 50).Nanoseconds())
			p95FrameNS := float64(framePercentile(frameDurations, 95).Nanoseconds())
			p99FrameNS := float64(framePercentile(frameDurations, 99).Nanoseconds())
			maxFrameNS := float64(frameDurations[len(frameDurations)-1].Nanoseconds())

			b.ReportAllocs()
			b.SetBytes(int64(len(corpus)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				streamingRenderBenchmarkReplay(b, chunks, nil)
			}
			b.StopTimer()
			b.ReportMetric(float64(len(chunks)), "frames/op")
			b.ReportMetric(p50FrameNS, "p50-frame-ns")
			b.ReportMetric(p95FrameNS, "p95-frame-ns")
			b.ReportMetric(p99FrameNS, "p99-frame-ns")
			b.ReportMetric(maxFrameNS, "max-frame-ns")
		})
	}
}

func streamingRenderBenchmarkReplay(b *testing.B, chunks []string, frameDurations *[]time.Duration) {
	b.Helper()
	tracker := NewToolTracker()
	maxTailBytes := 0
	for _, chunk := range chunks {
		var started time.Time
		if frameDurations != nil {
			started = time.Now()
		}

		tracker.AddTextSegment(chunk, streamingRenderBenchmarkWidth)
		view := tracker.RenderUnflushed(streamingRenderBenchmarkWidth, RenderMarkdown, false)
		flushed := tracker.FlushStreamingText(
			streamingRenderBenchmarkFlushThreshold,
			streamingRenderBenchmarkWidth,
			RenderMarkdown,
		)
		if len(view) > maxTailBytes {
			maxTailBytes = len(view)
		}
		streamingRenderBenchmarkSink += len(view) + len(flushed.ToPrint)

		if frameDurations != nil {
			*frameDurations = append(*frameDurations, time.Since(started))
		}
	}

	tracker.CompleteTextSegments(func(content string) string {
		return RenderMarkdown(content, streamingRenderBenchmarkWidth)
	})
	remaining := tracker.FlushAllRemaining(streamingRenderBenchmarkWidth, 0, RenderMarkdown)
	streamingRenderBenchmarkSink += len(remaining.ToPrint) + maxTailBytes
}

func streamingRenderBenchmarkCorpus(blocks int) string {
	var corpus strings.Builder
	corpus.Grow(blocks * 120)
	for i := 0; i < blocks; i++ {
		switch i % 5 {
		case 0:
			fmt.Fprintf(&corpus, "## Streaming renderer phase %d\n\n", i/5+1)
		case 1:
			fmt.Fprintf(&corpus, "The agent inspected **%d files**, compared `rendered` snapshots, and linked the [profiling guide](https://go.dev/pprof) before continuing.\n\n", 40+i)
		case 2:
			fmt.Fprintf(&corpus, "```go\nfunc renderFrame%d(markdown string) string {\nreturn render(markdown)\n}\n```\n\n", i)
		case 3:
			fmt.Fprintf(&corpus, "| sample | blocks | status |\n| ---: | ---: | :--- |\n| %d | %d | stable |\n\n", i/5+1, i+1)
		case 4:
			corpus.WriteString("***\n\n")
		}
	}
	return corpus.String()
}

func streamingRenderBenchmarkChunks(corpus string) []string {
	sizes := [...]int{17, 31, 47, 73}
	chunks := make([]string, 0, len(corpus)/32+1)
	for offset, sizeIndex := 0, 0; offset < len(corpus); sizeIndex++ {
		end := offset + sizes[sizeIndex%len(sizes)]
		if end > len(corpus) {
			end = len(corpus)
		}
		chunks = append(chunks, corpus[offset:end])
		offset = end
	}
	return chunks
}

func framePercentile(durations []time.Duration, percentile int) time.Duration {
	index := (len(durations)*percentile + 99) / 100
	if index > 0 {
		index--
	}
	return durations[index]
}
