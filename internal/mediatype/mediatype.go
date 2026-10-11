// Package mediatype provides the conservative presentation-media allowlist and
// bounded structural inspection shared by tools, serving, and export. Inspection
// identifies containers, not decoder support, duration, or complete codec validity.
package mediatype

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Kind is a presentation family.
type Kind string

const (
	Image Kind = "image"
	Video Kind = "video"
	Audio Kind = "audio"
)

// Format describes a supported MIME type, canonical stored suffix, and byte cap.
type Format struct {
	MIME      string
	Extension string
	Kind      Kind
	MaxBytes  int64
}

var formats = []Format{
	{"image/png", ".png", Image, 25 << 20}, {"image/jpeg", ".jpg", Image, 25 << 20},
	{"image/gif", ".gif", Image, 25 << 20}, {"image/webp", ".webp", Image, 25 << 20},
	{"image/bmp", ".bmp", Image, 25 << 20}, {"video/mp4", ".mp4", Video, 256 << 20},
	{"video/webm", ".webm", Video, 256 << 20}, {"audio/mpeg", ".mp3", Audio, 100 << 20},
	{"audio/wav", ".wav", Audio, 100 << 20}, {"audio/ogg", ".ogg", Audio, 100 << 20},
	{"audio/flac", ".flac", Audio, 100 << 20}, {"audio/mp4", ".m4a", Audio, 100 << 20},
	{"audio/webm", ".weba", Audio, 100 << 20},
}

// Lookup looks up an exact MIME (case/outer whitespace insensitive). Parameters
// and aliases are intentionally not accepted at the publication trust boundary.
func Lookup(mime string) (Format, bool) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	for _, f := range formats {
		if f.MIME == mime {
			return f, true
		}
	}
	return Format{}, false
}

// ForExtension maps a dotted canonical suffix to its format. It never inspects
// content; .mp4 and .webm remain video, while .m4a and .weba are audio.
func ForExtension(ext string) (Format, bool) {
	ext = strings.ToLower(strings.TrimSpace(ext))
	for _, f := range formats {
		if f.Extension == ext {
			return f, true
		}
	}
	return Format{}, false
}

// ErrUnsupported denotes unsupported, malformed, or truncated input. Genuine
// reader errors are returned separately (and may be tested with errors.Is).
var ErrUnsupported = errors.New("unsupported or malformed media")
var errBudget = errors.New("media inspection budget exhausted")

// Detect inspects only the supplied bytes, returning an empty MIME on failure.
// Supply the complete file for structural formats: a header slice of MP4,
// WebM, WAV, or ID3-prefixed MP3 may be rejected as truncated. Detect cannot
// inspect metadata or audio frames outside the supplied bytes.
func Detect(data []byte) string {
	f, err := Inspect(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return ""
	}
	return f.MIME
}

// Inspect inspects a ReaderAt of the specified size with bounded reads (4 MiB),
// entries (4096), and a fixed, shallow container hierarchy (below 16 levels).
// Payloads are skipped, not decoded. It does not enforce Format.MaxBytes;
// callers must enforce that cap before copying.
func Inspect(r io.ReaderAt, size int64) (Format, error) {
	if size <= 0 || r == nil {
		return Format{}, ErrUnsupported
	}
	s := &scanner{r: r, size: size}
	data, err := s.read(0, min(size, 65536))
	if err != nil {
		return Format{}, err
	}
	mime := imageType(data)
	if mime == "" {
		mime, err = inspectContainer(s, data)
	}
	if errors.Is(err, errBudget) {
		err = ErrUnsupported
	}
	if err != nil {
		return Format{}, fmt.Errorf("inspect media: %w", err)
	}
	f, ok := Lookup(mime)
	if !ok {
		return Format{}, ErrUnsupported
	}
	return f, nil
}

func inspectContainer(s *scanner, b []byte) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte("ID3")), len(b) >= 2 && b[0] == 0xff && b[1]&0xe0 == 0xe0:
		return inspectMP3(s)
	case bytes.HasPrefix(b, []byte("RIFF")):
		return inspectWAV(s)
	case bytes.HasPrefix(b, []byte("OggS")):
		_, err := inspectOgg(s)
		return "audio/ogg", err
	case bytes.HasPrefix(b, []byte("fLaC")):
		return inspectFLAC(s)
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return inspectMP4(s)
	case bytes.HasPrefix(b, []byte{0x1a, 0x45, 0xdf, 0xa3}):
		return inspectWebM(s)
	default:
		return "", ErrUnsupported
	}
}

type scanner struct {
	r       io.ReaderAt
	size    int64
	bytes   int64
	entries int
}

func (s *scanner) read(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off > s.size || n > s.size-off || n > 65536 {
		return nil, ErrUnsupported
	}
	if s.bytes+n > 4<<20 || s.entries >= 4096 {
		return nil, errBudget
	}
	s.bytes += n
	s.entries++
	b := make([]byte, int(n))
	got, err := s.r.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if got != len(b) {
		return nil, ErrUnsupported
	}
	return b, nil
}
