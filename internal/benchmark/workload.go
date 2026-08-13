package benchmark

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

var payloadVocabulary = []string{
	"amber", "apricot", "atlas", "birch", "brook", "cedar", "cinder", "clover",
	"cobalt", "comet", "copper", "coral", "dawn", "delta", "ember", "fern",
	"flint", "forest", "garden", "glade", "granite", "harbor", "hazel", "island",
	"ivory", "juniper", "lantern", "laurel", "linen", "lotus", "maple", "meadow",
	"mist", "moss", "nectar", "oasis", "olive", "onyx", "orbit", "pebble",
	"pine", "plum", "prairie", "quartz", "reed", "river", "robin", "saffron",
	"sage", "sand", "silver", "spruce", "stone", "syntax", "teal", "timber",
	"topaz", "valley", "vector", "violet", "willow", "winter", "wren", "zephyr",
}

type Payload struct {
	Text      string
	SHA256    string
	Bytes     int
	Words     int
	Estimated int
}

func DeriveSeed(seed int64, dimensions ...any) int64 {
	h := sha256.New()
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], uint64(seed))
	_, _ = h.Write(raw[:])
	for _, dimension := range dimensions {
		_, _ = fmt.Fprintf(h, "\x00%v", dimension)
	}
	sum := h.Sum(nil)
	return int64(binary.LittleEndian.Uint64(sum[:8]))
}

func GeneratePayload(seed int64, estimatedTokens int, workload string, outputTokens int, supportsOutputLimit bool) Payload {
	if estimatedTokens < 32 {
		estimatedTokens = 32
	}
	instruction := "Reply with only: OK"
	if !supportsOutputLimit {
		marker := fmt.Sprintf("BENCHMARK-END-%016x", uint64(seed))
		instruction = fmt.Sprintf("Write one concise paragraph of approximately %d words about the value of careful measurement. End the paragraph with the exact marker %s. Do not repeat words or phrases mechanically to reach the requested length, do not repeat the marker, and do not write or continue anything after it.", outputTokens, marker)
	} else if workload == "decode" {
		instruction = fmt.Sprintf("Reply with exactly %d occurrences of the word amber separated by single spaces and nothing else.", outputTokens)
	}

	rng := rand.New(rand.NewSource(seed))
	var b strings.Builder
	b.Grow(estimatedTokens * 4)
	fmt.Fprintf(&b, "benchmark-id %016x\n", uint64(seed))
	words := 0
	// EstimateTokens is bytes/4. Account for the final instruction while filling
	// the random body so the full provider-visible user payload approaches the
	// requested local estimate.
	targetBytes := estimatedTokens * 4
	for b.Len()+len(instruction)+2 < targetBytes {
		if words > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(payloadVocabulary[rng.Intn(len(payloadVocabulary))])
		words++
	}
	b.WriteByte('\n')
	b.WriteString(instruction)
	text := b.String()
	sum := sha256.Sum256([]byte(text))
	return Payload{
		Text:      text,
		SHA256:    hex.EncodeToString(sum[:]),
		Bytes:     len(text),
		Words:     words,
		Estimated: llm.EstimateTokens(text),
	}
}
