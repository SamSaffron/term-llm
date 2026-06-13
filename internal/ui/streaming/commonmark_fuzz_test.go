package streaming

import "testing"

func FuzzCommonMarkFullSpecChunking(f *testing.F) {
	for _, seed := range []int64{0, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89} {
		f.Add(seed, byte(seed))
	}

	input := []byte(commonMarkSpecMarkdown(f))
	want := renderDirectBytes(f, input)

	f.Fuzz(func(t *testing.T, seed int64, mode byte) {
		var chunks [][]byte
		switch mode % 4 {
		case 0:
			chunks = splitIntoNPieces(input, 100)
		case 1:
			chunks = randomChunks(input, seed, 32)
		case 2:
			chunks = randomChunks(input, seed, 128)
		default:
			chunks = adversarialMarkdownChunks(input)
		}
		got := renderStreamedBytes(t, chunks)
		assertRenderedEqual(t, want, got, chunks)
	})
}
