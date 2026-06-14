package streaming

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestCorpusStreamingParity(t *testing.T) {
	for _, doc := range loadCorpusDocuments(t) {
		t.Run(doc.name, func(t *testing.T) {
			want := renderDirectBytes(t, doc.content)
			cases := corpusChunkCases(doc.content)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					got := renderStreamedBytes(t, tc.chunks)
					assertRenderedEqual(t, want, got, tc.chunks)
				})
			}
		})
	}
}

type corpusDocument struct {
	name    string
	content []byte
}

type corpusChunkCase struct {
	name   string
	chunks [][]byte
}

func loadCorpusDocuments(t testing.TB) []corpusDocument {
	t.Helper()
	root := filepath.Join("testdata", "corpus")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	docs := make([]corpusDocument, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		docs = append(docs, corpusDocument{name: strings.TrimSuffix(entry.Name(), ".md"), content: content})
	}
	if len(docs) == 0 {
		t.Fatalf("no corpus documents found in %s", root)
	}
	return docs
}

func corpusChunkCases(input []byte) []corpusChunkCase {
	cases := []corpusChunkCase{
		{name: "all-at-once", chunks: [][]byte{append([]byte(nil), input...)}},
		{name: "byte-by-byte", chunks: fixedSizeChunks(input, 1)},
		{name: "line-by-line", chunks: bytes.SplitAfter(input, []byte("\n"))},
		{name: "hundred-pieces", chunks: splitIntoNPieces(input, 100)},
		{name: "adversarial-markdown-cuts", chunks: adversarialMarkdownChunks(input)},
	}
	for _, size := range []int{2, 3, 5, 8, 13, 21, 64} {
		cases = append(cases, corpusChunkCase{
			name:   fmt.Sprintf("fixed-%d", size),
			chunks: fixedSizeChunks(input, size),
		})
	}
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89} {
		cases = append(cases, corpusChunkCase{
			name:   fmt.Sprintf("random-seed-%d", seed),
			chunks: randomChunks(input, seed, 32),
		})
	}
	return cases
}

func normalizeStreamingSourceForDirect(input []byte) []byte {
	// StreamRenderer preserves legacy terminal behaviour by normalizing tabs in
	// markdown to two spaces once any streamed chunk contains a tab.
	input = bytes.ReplaceAll(input, []byte("\t"), []byte("  "))
	// Flush treats a trailing partial line as a complete line by appending a
	// newline before final render. Mirror that source-level behavior for direct
	// parity tests and fuzzing.
	if len(input) > 0 && !bytes.HasSuffix(input, []byte("\n")) {
		input = append(append([]byte(nil), input...), '\n')
	}
	return input
}

func renderDirectBytes(t testing.TB, input []byte) []byte {
	t.Helper()
	renderer := newTestMarkdownRenderer(testRenderWidth)
	out, err := renderer.Render(normalizeStreamingSourceForDirect(input))
	if err != nil {
		t.Fatalf("direct render failed: %v", err)
	}
	return normalizeNewlines(out)
}

func renderStreamedBytes(t testing.TB, chunks [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	sr, err := NewRenderer(&buf, newTestMarkdownRenderer(testRenderWidth))
	if err != nil {
		t.Fatalf("NewRenderer failed: %v", err)
	}
	for i, chunk := range chunks {
		if len(chunk) == 0 {
			continue
		}
		if _, err := sr.Write(chunk); err != nil {
			t.Fatalf("write chunk %d failed: %v", i, err)
		}
	}
	if err := sr.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	return buf.Bytes()
}

func fixedSizeChunks(input []byte, size int) [][]byte {
	if size <= 0 {
		panic("chunk size must be positive")
	}
	chunks := make([][]byte, 0, (len(input)+size-1)/size)
	for start := 0; start < len(input); start += size {
		end := min(start+size, len(input))
		chunks = append(chunks, append([]byte(nil), input[start:end]...))
	}
	return chunks
}

func splitIntoNPieces(input []byte, n int) [][]byte {
	if n <= 0 {
		panic("piece count must be positive")
	}
	if len(input) == 0 {
		return nil
	}
	if n > len(input) {
		n = len(input)
	}
	chunks := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		start := i * len(input) / n
		end := (i + 1) * len(input) / n
		if start == end {
			continue
		}
		chunks = append(chunks, append([]byte(nil), input[start:end]...))
	}
	return chunks
}

func randomChunks(input []byte, seed int64, maxSize int) [][]byte {
	if maxSize <= 0 {
		panic("max chunk size must be positive")
	}
	r := rand.New(rand.NewSource(seed))
	chunks := make([][]byte, 0)
	for start := 0; start < len(input); {
		size := r.Intn(maxSize) + 1
		end := min(start+size, len(input))
		chunks = append(chunks, append([]byte(nil), input[start:end]...))
		start = end
	}
	return chunks
}

func adversarialMarkdownChunks(input []byte) [][]byte {
	cutAfter := map[int]struct{}{len(input): {}}
	markers := []byte("#*_`~|[]()>-+.\n")
	for i, b := range input {
		if bytes.IndexByte(markers, b) >= 0 {
			for _, cut := range []int{i, i + 1, i + 2} {
				if cut > 0 && cut < len(input) {
					cutAfter[cut] = struct{}{}
				}
			}
		}
	}
	cuts := make([]int, 0, len(cutAfter))
	for cut := range cutAfter {
		cuts = append(cuts, cut)
	}
	sort.Ints(cuts)
	chunks := make([][]byte, 0, len(cuts))
	prev := 0
	for _, cut := range cuts {
		if cut <= prev {
			continue
		}
		chunks = append(chunks, append([]byte(nil), input[prev:cut]...))
		prev = cut
	}
	return chunks
}

func assertRenderedEqual(t *testing.T, want, got []byte, chunks [][]byte) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	idx := firstDiff(want, got)
	t.Fatalf("streamed render mismatch at byte %d\nwant around diff: %q\ngot  around diff: %q\nchunk sizes: %v", idx, surroundingBytes(want, idx), surroundingBytes(got, idx), chunkSizes(chunks))
}

func firstDiff(a, b []byte) int {
	limit := min(len(a), len(b))
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}

func surroundingBytes(b []byte, idx int) []byte {
	start := max(0, idx-40)
	end := min(len(b), idx+80)
	return b[start:end]
}

func chunkSizes(chunks [][]byte) []int {
	sizes := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		if len(chunk) > 0 {
			sizes = append(sizes, len(chunk))
		}
	}
	return sizes
}

func TestSplitIntoNPiecesProducesDeterministicNonEmptyChunks(t *testing.T) {
	input := []byte("abcdefghijklmnopqrstuvwxyz")
	chunks := splitIntoNPieces(input, 100)
	if len(chunks) != len(input) {
		t.Fatalf("got %d chunks, want %d", len(chunks), len(input))
	}
	if joined := bytes.Join(chunks, nil); !bytes.Equal(joined, input) {
		t.Fatalf("chunks did not reconstruct input")
	}
	if !reflect.DeepEqual(chunkSizes(splitIntoNPieces(input, 5)), []int{5, 5, 5, 5, 6}) {
		t.Fatalf("unexpected 5-piece split sizes: %v", chunkSizes(splitIntoNPieces(input, 5)))
	}
}
