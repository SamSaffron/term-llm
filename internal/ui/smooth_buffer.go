package ui

import (
	"strings"
	"sync"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

// Smooth buffer constants for 60fps adaptive rendering
const (
	SmoothFrameInterval    = 16 * time.Millisecond // 60fps
	SmoothBufferCapacity   = 500                   // characters
	SmoothMinWordsPerFrame = 1
	SmoothMaxWordsPerFrame = 5
	SmoothMaxWordLength    = 12 // Chunk words longer than this
)

// SmoothTickMsg is sent to trigger the next frame of smooth text rendering
type SmoothTickMsg struct{}

// SmoothBuffer provides smooth 60fps text streaming with adaptive speed.
// Text is buffered and released word-by-word at a pace that adapts to
// the incoming content rate.
type SmoothBuffer struct {
	mu        sync.Mutex
	buffer    strings.Builder
	inputDone bool
}

// NewSmoothBuffer creates a new SmoothBuffer
func NewSmoothBuffer() *SmoothBuffer {
	return &SmoothBuffer{}
}

// Write adds incoming text to the buffer
func (b *SmoothBuffer) Write(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buffer.WriteString(text)
}

// MarkDone signals that the input stream has ended
func (b *SmoothBuffer) MarkDone() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inputDone = true
}

// IsDrained returns true if the stream is done and buffer is empty
func (b *SmoothBuffer) IsDrained() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inputDone && b.buffer.Len() == 0
}

// Len returns the current buffer size in bytes
func (b *SmoothBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Len()
}

// IsEmpty returns true if the buffer is empty
func (b *SmoothBuffer) IsEmpty() bool {
	return b.Len() == 0
}

// wordsPerFrame calculates how many words to emit based on buffer fill level
func (b *SmoothBuffer) wordsPerFrame() int {
	// No lock needed - caller should hold lock or this is called internally
	bufLen := b.buffer.Len()
	fillRatio := float64(bufLen) / float64(SmoothBufferCapacity)

	if fillRatio < 0.2 {
		return SmoothMinWordsPerFrame // Buffer low - slow down
	} else if fillRatio > 0.8 {
		return SmoothMaxWordsPerFrame // Buffer full - speed up
	}
	// Linear scale between min and max
	return int(float64(SmoothMinWordsPerFrame) + (float64(SmoothMaxWordsPerFrame)-float64(SmoothMinWordsPerFrame))*fillRatio)
}

// NextWords returns the next N words based on buffer fill level.
// Long words are chunked into pieces. Whitespace is preserved.
func (b *SmoothBuffer) NextWords() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.buffer.Len() == 0 {
		return ""
	}

	content := b.buffer.String()
	numWords := b.wordsPerFrame()

	// Extract words with preserved whitespace
	result, remaining := extractWords(content, numWords)

	// Update buffer with remaining content
	b.buffer.Reset()
	b.buffer.WriteString(remaining)

	return result
}

// FlushAll returns all remaining content (for immediate display on tool start or cancel)
func (b *SmoothBuffer) FlushAll() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	content := b.buffer.String()
	b.buffer.Reset()
	return content
}

// Reset clears the buffer and resets the done flag
func (b *SmoothBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buffer.Reset()
	b.inputDone = false
}

// SmoothTick returns a tea.Cmd that sends a SmoothTickMsg after the frame interval
func SmoothTick() tea.Cmd {
	return tea.Tick(SmoothFrameInterval, func(time.Time) tea.Msg {
		return SmoothTickMsg{}
	})
}

// extractWords extracts up to n words from content, chunking long words.
// Returns (extracted, remaining).
func extractWords(content string, n int) (string, string) {
	if content == "" || n <= 0 {
		return "", content
	}

	runes := []rune(content)
	pos := 0
	wordsExtracted := 0
	var result strings.Builder

	for pos < len(runes) && wordsExtracted < n {
		// Skip and collect leading whitespace
		wsStart := pos
		for pos < len(runes) && unicode.IsSpace(runes[pos]) {
			pos++
		}
		if pos > wsStart {
			result.WriteString(string(runes[wsStart:pos]))
		}

		if pos >= len(runes) {
			break
		}

		// Collect word characters
		wordStart := pos
		for pos < len(runes) && !unicode.IsSpace(runes[pos]) {
			pos++
		}

		if pos > wordStart {
			word := runes[wordStart:pos]

			// Chunk long words
			if len(word) > SmoothMaxWordLength {
				// Take only a chunk
				chunk := word[:SmoothMaxWordLength]
				result.WriteString(string(chunk))
				// Put the rest back
				remaining := string(word[SmoothMaxWordLength:]) + string(runes[pos:])
				return result.String(), remaining
			}

			result.WriteString(string(word))
			wordsExtracted++
		}
	}

	return result.String(), string(runes[pos:])
}
