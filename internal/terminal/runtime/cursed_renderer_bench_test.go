package tea

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// benchmarkByteCounter deliberately does not copy or perform I/O. Benchmarks
// using it measure renderer CPU/control-path cost and report bytes-counted/op;
// only the explicitly named bytes.Buffer benchmark measures payload copying.
type benchmarkByteCounter struct {
	bytes int64
}

func (w *benchmarkByteCounter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	return len(p), nil
}

func (w *benchmarkByteCounter) WriteString(s string) (int, error) {
	w.bytes += int64(len(s))
	return len(s), nil
}

func (w *benchmarkByteCounter) reset() { w.bytes = 0 }

type rendererBenchmarkSize struct {
	width  int
	height int
}

var rendererBenchmarkSizes = []rendererBenchmarkSize{
	{width: 120, height: 40},
	{width: 200, height: 50},
}

func benchmarkFrame(width, height, offset, variant int, chrome bool) string {
	var b strings.Builder
	b.Grow(width * height * 2)
	for row := 0; row < height; row++ {
		if row > 0 {
			b.WriteByte('\n')
		}

		logicalRow := offset + row
		label := "body"
		if chrome {
			switch {
			case row < 2:
				label = "header"
				logicalRow = row
			case row >= height-5:
				label = "footer"
				logicalRow = row - (height - 5)
			default:
				logicalRow = offset + row - 2
			}
		}
		line := fmt.Sprintf("\x1b[38;2;190;190;200m\x1b[48;2;28;30;38m%s %04d v%d │ renderer workload", label, logicalRow, variant)
		if pad := width - ansi.StringWidth(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		b.WriteString(line)
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func benchmarkRepaintFrame(width, height, variant int) string {
	var b strings.Builder
	b.Grow(width * height)
	fill := 'A'
	color := "\x1b[31m"
	if variant%2 != 0 {
		fill = 'B'
		color = "\x1b[44m"
	}
	for row := 0; row < height; row++ {
		if row > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(color)
		b.WriteString(strings.Repeat(string(fill), width))
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func benchmarkFooterDiff(frame string) string {
	needle := "renderer workload"
	at := strings.LastIndex(frame, needle)
	if at < 0 {
		panic("benchmark frame missing footer marker")
	}
	changed := []byte(frame)
	changed[at] = 'R'
	return string(changed)
}

func assertBenchmarkFrame(b *testing.B, frame string, size rendererBenchmarkSize) {
	b.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) != size.height {
		b.Fatalf("benchmark frame height = %d, want %d", len(lines), size.height)
	}
	for row, line := range lines {
		if got := ansi.StringWidth(line); got != size.width {
			b.Fatalf("benchmark frame row %d width = %d, want %d", row, got, size.width)
		}
	}
}

func benchmarkSmallDiff(frame string) string {
	needle := "renderer workload"
	at := strings.Index(frame, needle)
	if at < 0 {
		panic("benchmark frame missing marker")
	}
	changed := []byte(frame)
	changed[at] = 'R'
	return string(changed)
}

func newBenchmarkRenderer(w io.Writer, size rendererBenchmarkSize) *cursedRenderer {
	r := newCursedRenderer(w, []string{"TERM=xterm-256color", "COLORTERM=truecolor", "TERM_PROGRAM=Apple_Terminal"}, size.width, size.height)
	r.syncdUpdates = false
	return r
}

func benchmarkFlushView(b *testing.B, size rendererBenchmarkSize, initial View, next func(int) View) {
	b.Helper()
	w := &benchmarkByteCounter{}
	r := newBenchmarkRenderer(w, size)
	r.render(initial)
	if err := r.flush(false); err != nil {
		b.Fatal(err)
	}
	w.reset()
	b.ReportAllocs()
	b.ReportMetric(float64(size.width*size.height), "input-cells/op")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.render(next(i))
		if err := r.flush(false); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(w.bytes)/float64(b.N), "bytes-counted/op")
}

func benchmarkBounceFrame(i, frameCount int) int {
	if frameCount < 2 {
		return 0
	}
	period := 2 * (frameCount - 1)
	frame := (i + 1) % period
	if frame >= frameCount {
		frame = period - frame
	}
	return frame
}

func BenchmarkCursedRenderer(b *testing.B) {
	for _, size := range rendererBenchmarkSizes {
		size := size
		b.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(b *testing.B) {
			baseContent := benchmarkFrame(size.width, size.height, 0, 0, false)
			assertBenchmarkFrame(b, baseContent, size)
			base := NewView(baseContent)
			base.AltScreen = true

			b.Run("unchanged", func(b *testing.B) {
				benchmarkFlushView(b, size, base, func(int) View { return base })
			})

			b.Run("one-cell-diff", func(b *testing.B) {
				changed := NewView(benchmarkSmallDiff(baseContent))
				changed.AltScreen = true
				benchmarkFlushView(b, size, base, func(i int) View {
					if i%2 == 0 {
						return changed
					}
					return base
				})
			})

			b.Run("whole-frame-content-fallback", func(b *testing.B) {
				changedContent := benchmarkFrame(size.width, size.height, size.height*4, 1, false)
				assertBenchmarkFrame(b, changedContent, size)
				changed := NewView(changedContent)
				changed.AltScreen = true
				benchmarkFlushView(b, size, base, func(i int) View {
					if i%2 == 0 {
						return changed
					}
					return base
				})
			})

			b.Run("full-repaint-all-cells", func(b *testing.B) {
				contents := []string{
					benchmarkRepaintFrame(size.width, size.height, 0),
					benchmarkRepaintFrame(size.width, size.height, 1),
				}
				for _, content := range contents {
					assertBenchmarkFrame(b, content, size)
				}
				views := []View{NewView(contents[0]), NewView(contents[1])}
				views[0].AltScreen = true
				views[1].AltScreen = true
				benchmarkFlushView(b, size, views[0], func(i int) View { return views[(i+1)%2] })
			})

			b.Run("vertical-scroll-fixed-chrome", func(b *testing.B) {
				const frameCount = 128
				frames := make([]View, frameCount)
				for i := range frames {
					content := benchmarkFrame(size.width, size.height, i, 0, true)
					assertBenchmarkFrame(b, content, size)
					frames[i] = NewView(content)
					frames[i].AltScreen = true
				}
				benchmarkFlushView(b, size, frames[0], func(i int) View {
					return frames[benchmarkBounceFrame(i, len(frames))]
				})
			})

			b.Run("near-miss-scroll-changed-footer-fallback", func(b *testing.B) {
				const frameCount = 128
				frames := make([]View, frameCount)
				for i := range frames {
					content := benchmarkFrame(size.width, size.height, i, 0, true)
					if i%2 != 0 {
						content = benchmarkFooterDiff(content)
					}
					assertBenchmarkFrame(b, content, size)
					frames[i] = NewView(content)
					frames[i].AltScreen = true
				}
				benchmarkFlushView(b, size, frames[0], func(i int) View {
					return frames[benchmarkBounceFrame(i, len(frames))]
				})
			})

			b.Run("inline-scrollback-insertion", func(b *testing.B) {
				w := &benchmarkByteCounter{}
				r := newBenchmarkRenderer(w, size)
				view := NewView(baseContent)
				r.render(view)
				if err := r.flush(false); err != nil {
					b.Fatal(err)
				}
				w.reset()
				b.ReportAllocs()
				b.ReportMetric(float64(size.width*size.height), "input-cells/op")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					r.render(view)
					if err := r.insertAbove(fmt.Sprintf("committed scrollback line %06d", i)); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(w.bytes)/float64(b.N), "bytes-counted/op")
			})

			b.Run("resize", func(b *testing.B) {
				smaller := rendererBenchmarkSize{width: size.width - 7, height: size.height - 3}
				smallerContent := benchmarkFrame(smaller.width, smaller.height, 0, 0, false)
				assertBenchmarkFrame(b, smallerContent, smaller)
				views := []View{base, NewView(smallerContent)}
				views[1].AltScreen = true
				w := &benchmarkByteCounter{}
				r := newBenchmarkRenderer(w, size)
				r.render(views[0])
				if err := r.flush(false); err != nil {
					b.Fatal(err)
				}
				w.reset()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					idx := (i + 1) % 2
					target := []rendererBenchmarkSize{size, smaller}[idx]
					r.resize(target.width, target.height)
					r.render(views[idx])
					if err := r.flush(false); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(w.bytes)/float64(b.N), "bytes-counted/op")
			})

			b.Run("post-frame-byte-counter-no-copy-64KiB", func(b *testing.B) {
				payload := strings.Repeat("P", 64<<10)
				benchmarkFlushView(b, size, base, func(int) View {
					view := base
					view.PostFrame = payload
					view.PostFrameMsg = func(error) Msg { return nil }
					return view
				})
			})

			b.Run("post-frame-bytes-buffer-copy-64KiB", func(b *testing.B) {
				payload := strings.Repeat("P", 64<<10)
				var w bytes.Buffer
				w.Grow(len(payload) * 2)
				r := newBenchmarkRenderer(&w, size)
				r.render(base)
				if err := r.flush(false); err != nil {
					b.Fatal(err)
				}
				w.Reset()
				b.ReportAllocs()
				b.SetBytes(int64(len(payload)))
				b.ResetTimer()
				for range b.N {
					view := base
					view.PostFrame = payload
					view.PostFrameMsg = func(error) Msg { return nil }
					r.render(view)
					if err := r.flush(false); err != nil {
						b.Fatal(err)
					}
					w.Reset()
				}
			})

			b.Run("coalesced-post-frame-byte-counter-no-copy-64KiB", func(b *testing.B) {
				payload := strings.Repeat("P", 64<<10)
				w := &benchmarkByteCounter{}
				r := newBenchmarkRenderer(w, size)
				r.render(base)
				if err := r.flush(false); err != nil {
					b.Fatal(err)
				}
				w.reset()
				b.ReportAllocs()
				b.ReportMetric(float64(size.width*size.height), "input-cells/op")
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for replacement := 0; replacement < 4; replacement++ {
						view := base
						view.PostFrame = payload
						view.PostFrameMsg = func(error) Msg { return nil }
						r.render(view)
					}
					if err := r.flush(false); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(w.bytes)/float64(b.N), "bytes-counted/op")
			})
		})
	}
}
