package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Frame represents a single screen capture.
type Frame struct {
	Timestamp time.Time
	Raw       string // Raw output with ANSI codes
	Plain     string // Text without ANSI codes
	Phase     string // Phase at capture time (if known)
}

// ScreenCapture stores captured screen frames.
type ScreenCapture struct {
	mu        sync.Mutex
	frames    []Frame
	enabled   bool
	startTime time.Time
}

// NewScreenCapture creates a new screen capture.
func NewScreenCapture() *ScreenCapture {
	return &ScreenCapture{
		startTime: time.Now(),
	}
}

// Enable turns on screen capture.
func (s *ScreenCapture) Enable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = true
	s.startTime = time.Now()
}

// Disable turns off screen capture.
func (s *ScreenCapture) Disable() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = false
}

// IsEnabled returns true if capture is enabled.
func (s *ScreenCapture) IsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// Capture records a new frame.
func (s *ScreenCapture) Capture(raw, phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return
	}
	s.frames = append(s.frames, Frame{
		Timestamp: time.Now(),
		Raw:       raw,
		Plain:     StripANSI(raw),
		Phase:     phase,
	})
}

// Frames returns all captured frames.
func (s *ScreenCapture) Frames() []Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Frame, len(s.frames))
	copy(result, s.frames)
	return result
}

// FrameCount returns the number of captured frames.
func (s *ScreenCapture) FrameCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

// LastFrame returns the most recent frame, or empty frame if none.
func (s *ScreenCapture) LastFrame() Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return Frame{}
	}
	return s.frames[len(s.frames)-1]
}

// FinalScreen returns the raw content of the last frame.
func (s *ScreenCapture) FinalScreen() string {
	frame := s.LastFrame()
	return frame.Raw
}

// FinalScreenPlain returns the plain text content of the last frame.
func (s *ScreenCapture) FinalScreenPlain() string {
	frame := s.LastFrame()
	return frame.Plain
}

// Clear removes all captured frames.
func (s *ScreenCapture) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = nil
}

// RenderScreen prints the final screen to stdout with ANSI codes.
func (s *ScreenCapture) RenderScreen() {
	frame := s.LastFrame()
	if frame.Raw != "" {
		fmt.Println("=== Screen Capture (Final) ===")
		fmt.Println(frame.Raw)
		fmt.Println("=== End Screen Capture ===")
	}
}

// RenderPlain prints the final screen to stdout without ANSI codes.
func (s *ScreenCapture) RenderPlain() {
	frame := s.LastFrame()
	if frame.Plain != "" {
		fmt.Println("=== Screen Capture (Final - Plain) ===")
		fmt.Println(frame.Plain)
		fmt.Println("=== End Screen Capture ===")
	}
}

// RenderAllFrames prints all frames with timestamps.
func (s *ScreenCapture) RenderAllFrames() {
	frames := s.Frames()
	for i, f := range frames {
		elapsed := f.Timestamp.Sub(s.startTime)
		fmt.Printf("=== Frame %d (%.3fs) Phase: %s ===\n", i, elapsed.Seconds(), f.Phase)
		fmt.Println(f.Plain)
		fmt.Println()
	}
}

// SaveScreen saves the final screen to a file.
func (s *ScreenCapture) SaveScreen(path string) error {
	frame := s.LastFrame()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(frame.Raw), 0644)
}

// SaveScreenPlain saves the final screen without ANSI codes to a file.
func (s *ScreenCapture) SaveScreenPlain(path string) error {
	frame := s.LastFrame()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(frame.Plain), 0644)
}

// SaveFrames saves each frame to a separate file in the given directory.
func (s *ScreenCapture) SaveFrames(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	frames := s.Frames()
	for i, f := range frames {
		elapsed := f.Timestamp.Sub(s.startTime)
		filename := fmt.Sprintf("frame_%03d_%.3fs.txt", i, elapsed.Seconds())
		path := filepath.Join(dir, filename)
		content := fmt.Sprintf("Phase: %s\nTimestamp: %s\n\n%s", f.Phase, f.Timestamp.Format(time.RFC3339Nano), f.Plain)
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			return err
		}
	}
	return nil
}

// Dump returns a debug string showing all frames.
func (s *ScreenCapture) Dump() string {
	var sb strings.Builder
	frames := s.Frames()
	sb.WriteString(fmt.Sprintf("Screen Capture: %d frames\n", len(frames)))
	for i, f := range frames {
		elapsed := f.Timestamp.Sub(s.startTime)
		sb.WriteString(fmt.Sprintf("\n--- Frame %d (%.3fs) Phase: %s ---\n", i, elapsed.Seconds(), f.Phase))
		sb.WriteString(f.Plain)
		sb.WriteString("\n")
	}
	return sb.String()
}

// ansiRegex matches ANSI escape sequences.
var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]|\x1b\][^\x07]*\x07`)

// StripANSI removes ANSI escape sequences from a string.
func StripANSI(s string) string {
	return ansiRegex.ReplaceAllString(s, "")
}

// DebugScreensEnabled returns true if DEBUG_SCREENS environment variable is set.
func DebugScreensEnabled() bool {
	return os.Getenv("DEBUG_SCREENS") != ""
}

// SaveFramesEnabled returns true if SAVE_FRAMES environment variable is set.
func SaveFramesEnabled() bool {
	return os.Getenv("SAVE_FRAMES") != ""
}
