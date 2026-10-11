package mediatype

import (
	"encoding/binary"
	"math"
)

type mp4Box struct {
	kind       string
	start, end int64
}

func readMP4Box(s *scanner, off, end int64, allowPayloadTail bool) (mp4Box, error) {
	if end-off < 8 {
		return mp4Box{}, ErrUnsupported
	}
	b, err := s.read(off, 8)
	if err != nil {
		return mp4Box{}, err
	}
	n := uint64(binary.BigEndian.Uint32(b[:4]))
	header := int64(8)
	if n == 1 {
		if end-off < 16 {
			return mp4Box{}, ErrUnsupported
		}
		extra, err := s.read(off+8, 8)
		if err != nil {
			return mp4Box{}, err
		}
		n = binary.BigEndian.Uint64(extra)
		header = 16
	}
	if n == 0 {
		n = uint64(end - off)
	}
	kind := string(b[4:8])
	if allowPayloadTail && n <= math.MaxInt64 && n > uint64(end-off) && (kind == "mdat" || kind == "free" || kind == "skip") {
		n = uint64(end - off)
	}
	if n < uint64(header) || n > uint64(end-off) {
		return mp4Box{}, ErrUnsupported
	}
	return mp4Box{kind, off + header, off + int64(n)}, nil
}

func inspectMP4(s *scanner) (string, error) {
	box, err := readMP4Box(s, 0, s.size, false)
	if err != nil {
		return "", err
	}
	audioBrand, ok, err := mp4Brands(s, box)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrUnsupported
	}
	tracks := mp4Tracks{complete: true}
	for off := box.end; s.size-off >= 8; {
		box, err = readMP4Box(s, off, s.size, true)
		if err == errBudget {
			return mp4Fallback(audioBrand, tracks.video), nil
		}
		if err != nil {
			return "", err
		}
		if box.kind == "moov" {
			err = scanMP4Tracks(s, box, &tracks)
			if err == errBudget {
				return mp4Fallback(audioBrand, tracks.video), nil
			}
			if err != nil {
				return "", err
			}
		}
		off = box.end
	}
	if tracks.video {
		return "video/mp4", nil
	}
	if audioBrand || tracks.complete && tracks.audio && tracks.count > 0 {
		return "audio/mp4", nil
	}
	return "video/mp4", nil
}

func mp4Fallback(audio, video bool) string {
	if audio && !video {
		return "audio/mp4"
	}
	return "video/mp4"
}

func mp4Brands(s *scanner, box mp4Box) (audio, ok bool, err error) {
	n := box.end - box.start
	if box.kind != "ftyp" || n < 8 || n%4 != 0 {
		return false, false, ErrUnsupported
	}
	for off := box.start; off < box.end; off += 4 {
		if off == box.start+4 {
			continue
		}
		b, err := s.read(off, 4)
		if err != nil {
			return false, false, err
		}
		brand := string(b)
		if off == box.start {
			if mp4ImageBrand(brand) {
				return false, false, ErrUnsupported
			}
			ok = mp4VideoBrand(brand)
		}
		if brand == "M4A " || brand == "M4B " {
			audio = true
			ok = true
		}
	}
	return audio, ok, nil
}

func mp4VideoBrand(brand string) bool {
	switch brand {
	case "isom", "iso2", "iso3", "iso4", "iso5", "iso6", "iso7", "iso8", "iso9", "mp41", "mp42", "avc1", "dash", "M4V ", "MSNV":
		return true
	}
	return false
}

func mp4ImageBrand(brand string) bool {
	switch brand {
	case "avif", "avis", "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
		return true
	}
	return false
}

type mp4Tracks struct {
	audio, video, complete bool
	count                  int
}

func scanMP4Tracks(s *scanner, moov mp4Box, t *mp4Tracks) error {
	for off := moov.start; off < moov.end; {
		box, err := readMP4Box(s, off, moov.end, false)
		if err != nil {
			return err
		}
		if box.kind == "trak" {
			handler, err := mp4TrackHandler(s, box, "mdia")
			if err != nil {
				return err
			}
			t.count++
			t.audio = t.audio || handler == "soun"
			t.video = t.video || handler == "vide"
			t.complete = t.complete && handler != ""
		}
		off = box.end
	}
	return nil
}

func mp4TrackHandler(s *scanner, parent mp4Box, want string) (string, error) {
	found := ""
	seen := false
	for off := parent.start; off < parent.end; {
		box, err := readMP4Box(s, off, parent.end, false)
		if err != nil {
			return "", err
		}
		if box.kind == want {
			if seen {
				return "", ErrUnsupported
			}
			seen = true
			if want == "mdia" {
				found, err = mp4TrackHandler(s, box, "hdlr")
			} else {
				if box.end-box.start < 24 {
					return "", ErrUnsupported
				}
				var b []byte
				b, err = s.read(box.start, 12)
				if err == nil {
					if b[0] != 0 {
						return "", ErrUnsupported
					}
					found = string(b[8:12])
				}
			}
			if err != nil {
				return "", err
			}
		}
		off = box.end
	}
	return found, nil
}
