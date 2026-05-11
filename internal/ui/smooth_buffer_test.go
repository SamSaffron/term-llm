package ui

import (
	"strings"
	"testing"
)

func TestSmoothBuffer_BasicWrite(t *testing.T) {
	b := NewSmoothBuffer()

	b.Write("hello world")
	if b.Len() != 11 {
		t.Errorf("expected len 11, got %d", b.Len())
	}
	if b.IsEmpty() {
		t.Error("expected non-empty buffer")
	}
}

func TestSmoothBuffer_NextWords_SingleWord(t *testing.T) {
	b := NewSmoothBuffer()
	b.Write("hello")

	words := b.NextWords()
	if words != "hello" {
		t.Errorf("expected 'hello', got %q", words)
	}
	if !b.IsEmpty() {
		t.Error("expected empty buffer after extracting single word")
	}
}

func TestSmoothBuffer_NextWords_MultipleWords(t *testing.T) {
	b := NewSmoothBuffer()
	b.Write("hello world how are you")

	// With low buffer, should get 1 word at a time
	// Whitespace after word is part of the next extraction's leading whitespace
	words := b.NextWords()
	if words != "hello" {
		t.Errorf("expected 'hello', got %q", words)
	}

	// Second extraction gets the space and next word
	words = b.NextWords()
	if !strings.HasPrefix(words, " ") {
		t.Errorf("expected leading space, got %q", words)
	}
}

func TestSmoothBuffer_NextWords_PreservesWhitespace(t *testing.T) {
	b := NewSmoothBuffer()
	b.Write("  hello   world  ")

	// First extraction should include leading whitespace
	words := b.NextWords()
	if !strings.HasPrefix(words, "  ") {
		t.Errorf("expected leading whitespace, got %q", words)
	}
}

func TestSmoothBuffer_NextWords_ChunksLongWords(t *testing.T) {
	b := NewSmoothBuffer()
	longWord := "supercalifragilisticexpialidocious"
	b.Write(longWord)

	// Should extract only SmoothMaxWordLength characters
	words := b.NextWords()
	if len(words) != SmoothMaxWordLength {
		t.Errorf("expected %d chars, got %d: %q", SmoothMaxWordLength, len(words), words)
	}
	if words != longWord[:SmoothMaxWordLength] {
		t.Errorf("expected %q, got %q", longWord[:SmoothMaxWordLength], words)
	}

	// Remaining should still be in buffer
	if b.IsEmpty() {
		t.Error("expected remaining content in buffer")
	}
}

func TestSmoothBuffer_FlushAll(t *testing.T) {
	b := NewSmoothBuffer()
	b.Write("hello world how are you")

	content := b.FlushAll()
	if content != "hello world how are you" {
		t.Errorf("expected full content, got %q", content)
	}
	if !b.IsEmpty() {
		t.Error("expected empty buffer after flush")
	}
}

func TestSmoothBuffer_MarkDone(t *testing.T) {
	b := NewSmoothBuffer()
	b.Write("hello")

	if b.IsDrained() {
		t.Error("should not be drained with content")
	}

	b.MarkDone()
	if b.IsDrained() {
		t.Error("should not be drained with content even after MarkDone")
	}

	b.FlushAll()
	if !b.IsDrained() {
		t.Error("should be drained after MarkDone and flush")
	}
}

func TestSmoothBuffer_Reset(t *testing.T) {
	b := NewSmoothBuffer()
	b.Write("hello")
	b.MarkDone()

	b.Reset()
	if !b.IsEmpty() {
		t.Error("expected empty buffer after reset")
	}
	if b.IsDrained() {
		t.Error("should not be drained after reset (inputDone should be false)")
	}
}

func TestSmoothBuffer_AdaptiveSpeed(t *testing.T) {
	b := NewSmoothBuffer()

	// Fill buffer to trigger faster extraction
	content := strings.Repeat("word ", 200) // ~1000 chars
	b.Write(content)

	// With high buffer fill, should get more words per frame
	words := b.NextWords()
	wordCount := len(strings.Fields(words))
	if wordCount < SmoothMinWordsPerFrame {
		t.Errorf("expected at least %d words, got %d", SmoothMinWordsPerFrame, wordCount)
	}
}

func TestSmoothBuffer_EmptyBuffer(t *testing.T) {
	b := NewSmoothBuffer()

	words := b.NextWords()
	if words != "" {
		t.Errorf("expected empty string, got %q", words)
	}
}

func TestSmoothBuffer_WriteAfterPartialRead(t *testing.T) {
	b := NewSmoothBuffer()
	b.Write("one two three")

	first := b.NextWords()
	if first == "" {
		t.Fatal("expected first chunk")
	}
	if b.readOffset == 0 {
		t.Fatal("expected non-zero readOffset after partial read")
	}

	b.Write(" four")
	if got := first + b.FlushAll(); got != "one two three four" {
		t.Fatalf("combined output = %q, want original order", got)
	}
}

func TestSmoothBuffer_CompactPreservesUnreadAndShrinksLargeBacking(t *testing.T) {
	remaining := " tail one two"
	backing := make([]byte, smoothBufferCompactThreshold+len(remaining), 64*1024)
	for i := 0; i < smoothBufferCompactThreshold; i++ {
		backing[i] = 'x'
	}
	copy(backing[smoothBufferCompactThreshold:], remaining)

	b := &SmoothBuffer{
		buffer:     backing,
		readOffset: smoothBufferCompactThreshold,
	}
	oldCap := cap(b.buffer)

	b.compactIfNeeded()

	if b.readOffset != 0 {
		t.Fatalf("readOffset = %d, want 0", b.readOffset)
	}
	if got := string(b.buffer); got != remaining {
		t.Fatalf("buffer = %q, want %q", got, remaining)
	}
	if got := b.Len(); got != len(remaining) {
		t.Fatalf("Len() = %d, want %d", got, len(remaining))
	}
	if cap(b.buffer) >= oldCap {
		t.Fatalf("cap after compact = %d, want less than old cap %d", cap(b.buffer), oldCap)
	}

	b.Write(" three")
	if got := b.FlushAll(); got != " tail one two three" {
		t.Fatalf("FlushAll after compact/write = %q", got)
	}
}

func TestExtractWords_Basic(t *testing.T) {
	tests := []struct {
		content   string
		n         int
		wantWords string
		wantRem   string
	}{
		{"hello world", 1, "hello", " world"},
		{"hello world", 2, "hello world", ""},
		{"hello world", 3, "hello world", ""},
		{"  hello world", 1, "  hello", " world"},
		{"hello", 1, "hello", ""},
		{"", 1, "", ""},
		{"hello world", 0, "", "hello world"},
	}

	for _, tt := range tests {
		got, rem := extractWords(tt.content, tt.n)
		if got != tt.wantWords {
			t.Errorf("extractWords(%q, %d) words = %q, want %q", tt.content, tt.n, got, tt.wantWords)
		}
		if rem != tt.wantRem {
			t.Errorf("extractWords(%q, %d) remaining = %q, want %q", tt.content, tt.n, rem, tt.wantRem)
		}
	}
}

func TestExtractWords_LongWord(t *testing.T) {
	longWord := "abcdefghijklmnopqrstuvwxyz" // 26 chars
	got, rem := extractWords(longWord, 1)

	if len(got) != SmoothMaxWordLength {
		t.Errorf("expected %d chars, got %d", SmoothMaxWordLength, len(got))
	}
	if rem != longWord[SmoothMaxWordLength:] {
		t.Errorf("remaining = %q, want %q", rem, longWord[SmoothMaxWordLength:])
	}
}

func TestExtractWords_LongWordChunksByRune(t *testing.T) {
	longWord := strings.Repeat("界", SmoothMaxWordLength+3)
	got, rem := extractWords(longWord, 1)

	if got != strings.Repeat("界", SmoothMaxWordLength) {
		t.Errorf("got %q, want first %d runes", got, SmoothMaxWordLength)
	}
	if rem != strings.Repeat("界", 3) {
		t.Errorf("remaining = %q, want 3 runes", rem)
	}
}

func TestExtractWords_MixedContent(t *testing.T) {
	// Test with newlines and various whitespace
	content := "hello\nworld\ttab"
	got, rem := extractWords(content, 2)

	// Should get "hello\nworld" and remainder "\ttab" (whitespace before next word)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("expected both words, got %q", got)
	}
	if rem != "\ttab" {
		t.Errorf("remaining = %q, want '\\ttab'", rem)
	}
}

func TestSmoothBuffer_ConcurrentAccess(t *testing.T) {
	b := NewSmoothBuffer()
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			b.Write("word ")
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 50; i++ {
			b.NextWords()
		}
		done <- true
	}()

	<-done
	<-done
	// If we get here without a race condition, the test passes
}
