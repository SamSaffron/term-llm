package ui

import (
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
)

// Smooth buffer constants for 60fps adaptive rendering
const (
	SmoothFrameInterval    = 16 * time.Millisecond // 60fps
	SmoothBufferCapacity   = 500                   // characters
	SmoothMinWordsPerFrame = 1
	SmoothMaxWordsPerFrame = 5
	SmoothMaxWordLength    = 12 // Chunk words longer than this

	smoothBufferCompactThreshold = 4096
)

// SmoothTickMsg is sent to trigger the next frame of smooth text rendering
type SmoothTickMsg struct{}

// SmoothBuffer provides smooth 60fps text streaming with adaptive speed.
// Text is buffered and released word-by-word at a pace that adapts to
// the incoming content rate.
type SmoothBuffer struct {
	mu         sync.Mutex
	buffer     []byte
	readOffset int
	inputDone  bool
}

// NewSmoothBuffer creates a new SmoothBuffer
func NewSmoothBuffer() *SmoothBuffer {
	return &SmoothBuffer{}
}

// Write adds incoming text to the buffer
func (b *SmoothBuffer) Write(text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buffer = append(b.buffer, text...)
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
	return b.inputDone && b.unreadLen() == 0
}

// Len returns the current buffer size in bytes
func (b *SmoothBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.unreadLen()
}

// IsEmpty returns true if the buffer is empty
func (b *SmoothBuffer) IsEmpty() bool {
	return b.Len() == 0
}

func (b *SmoothBuffer) unreadLen() int {
	return len(b.buffer) - b.readOffset
}

// wordsPerFrame calculates how many words to emit based on buffer fill level
func (b *SmoothBuffer) wordsPerFrame() int {
	// No lock needed - caller should hold lock or this is called internally
	bufLen := b.unreadLen()
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

	if b.unreadLen() == 0 {
		return ""
	}

	numWords := b.wordsPerFrame()

	// Extract words with preserved whitespace.
	result, consumed := extractWordsFromBytes(b.buffer[b.readOffset:], numWords)
	b.readOffset += consumed
	b.compactIfNeeded()

	return result
}

// FlushAll returns all remaining content (for immediate display on tool start or cancel)
func (b *SmoothBuffer) FlushAll() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	content := string(b.buffer[b.readOffset:])
	b.buffer = nil
	b.readOffset = 0
	return content
}

// Reset clears the buffer and resets the done flag
func (b *SmoothBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buffer = nil
	b.readOffset = 0
	b.inputDone = false
}

func (b *SmoothBuffer) compactIfNeeded() {
	if b.readOffset == 0 {
		return
	}
	unread := b.unreadLen()
	if unread == 0 {
		b.buffer = nil
		b.readOffset = 0
		return
	}
	if b.readOffset < smoothBufferCompactThreshold || b.readOffset < unread {
		return
	}

	remaining := b.buffer[b.readOffset:]
	if cap(b.buffer) > unread*2+smoothBufferCompactThreshold {
		b.buffer = append([]byte(nil), remaining...)
	} else {
		copy(b.buffer, remaining)
		b.buffer = b.buffer[:unread]
	}
	b.readOffset = 0
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

	result, consumed := extractWordsFromBytes([]byte(content), n)
	return result, content[consumed:]
}

func extractWordsFromBytes(content []byte, n int) (string, int) {
	if len(content) == 0 || n <= 0 {
		return "", 0
	}

	pos := 0
	wordsExtracted := 0
	var result strings.Builder
	result.Grow(initialSmoothResultCapacity(len(content), n))

	for pos < len(content) && wordsExtracted < n {
		// Skip and collect leading whitespace
		wsStart := pos
		for pos < len(content) {
			r, size := decodeSmoothRune(content[pos:])
			if !unicode.IsSpace(r) {
				break
			}
			pos += size
		}
		if pos > wsStart {
			result.Write(content[wsStart:pos])
		}

		if pos >= len(content) {
			break
		}

		// Collect word characters
		wordStart := pos
		chunkEnd := pos
		wordRunes := 0
		for pos < len(content) {
			r, size := decodeSmoothRune(content[pos:])
			if unicode.IsSpace(r) {
				break
			}
			if wordRunes < SmoothMaxWordLength {
				chunkEnd = pos + size
			}
			wordRunes++
			pos += size
			if wordRunes > SmoothMaxWordLength {
				result.Write(content[wordStart:chunkEnd])
				return result.String(), chunkEnd
			}
		}

		if pos > wordStart {
			result.Write(content[wordStart:pos])
			wordsExtracted++
		}
	}

	return result.String(), pos
}

func decodeSmoothRune(content []byte) (rune, int) {
	return utf8.DecodeRune(content)
}

func initialSmoothResultCapacity(contentLen, n int) int {
	capacity := n*(SmoothMaxWordLength+1) + 16
	if capacity > contentLen {
		return contentLen
	}
	return capacity
}
