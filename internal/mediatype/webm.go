package mediatype

import "encoding/binary"

type ebmlElement struct {
	id         uint64
	start, end int64
	unknown    bool
}

func readVINT(s *scanner, off int64, id bool) (uint64, int64, bool, error) {
	b, err := s.read(off, 1)
	if err != nil {
		return 0, 0, false, err
	}
	width := int64(1)
	mask := byte(0x80)
	for mask != 0 && b[0]&mask == 0 {
		mask >>= 1
		width++
	}
	if mask == 0 || id && width > 4 {
		return 0, 0, false, ErrUnsupported
	}
	b, err = s.read(off, width)
	if err != nil {
		return 0, 0, false, err
	}
	v := uint64(b[0])
	if !id {
		v &= uint64(mask - 1)
	}
	for _, c := range b[1:] {
		v = v<<8 | uint64(c)
	}
	unknown := !id && v == (uint64(1)<<(7*width))-1
	return v, width, unknown, nil
}

func readEBML(s *scanner, off, end int64) (ebmlElement, error) {
	id, a, _, err := readVINT(s, off, true)
	if err != nil {
		return ebmlElement{}, err
	}
	n, b, unknown, err := readVINT(s, off+a, false)
	if err != nil {
		return ebmlElement{}, err
	}
	start := off + a + b
	if start > end || !unknown && n > uint64(end-start) {
		return ebmlElement{}, ErrUnsupported
	}
	limit := end
	if !unknown {
		limit = start + int64(n)
	}
	return ebmlElement{id, start, limit, unknown}, nil
}

func inspectWebM(s *scanner) (string, error) {
	header, err := readEBML(s, 0, s.size)
	if err != nil {
		return "", err
	}
	if header.id != 0x1a45dfa3 || header.unknown {
		return "", ErrUnsupported
	}
	if err = webMDocType(s, header); err != nil {
		return "", err
	}
	seenSegment := false
	mime := "video/webm"
	for off := header.end; off < s.size; {
		segment, err := readEBML(s, off, s.size)
		if err == errBudget {
			return "video/webm", nil
		}
		if err != nil {
			return "", err
		}
		if segment.id == 0x18538067 {
			if seenSegment {
				return "", ErrUnsupported
			}
			seenSegment = true
			mime, err = webMSegment(s, segment)
			if err != nil {
				return "", err
			}
		} else if segment.unknown {
			return "", ErrUnsupported
		}
		off = segment.end
	}
	return mime, nil
}

func webMDocType(s *scanner, header ebmlElement) error {
	found := false
	for off := header.start; off < header.end; {
		e, err := readEBML(s, off, header.end)
		if err != nil {
			return err
		}
		if e.unknown {
			return ErrUnsupported
		}
		if e.id == 0x4282 {
			if found || e.end-e.start != 4 {
				return ErrUnsupported
			}
			b, err := s.read(e.start, 4)
			if err != nil {
				return err
			}
			if string(b) != "webm" {
				return ErrUnsupported
			}
			found = true
		}
		off = e.end
	}
	if !found {
		return ErrUnsupported
	}
	return nil
}

func webMSegment(s *scanner, segment ebmlElement) (string, error) {
	for off := segment.start; off < segment.end; {
		e, err := readEBML(s, off, segment.end)
		if err == errBudget {
			return "video/webm", nil
		}
		if err != nil {
			return "", err
		}
		if e.unknown {
			if e.id == 0x1f43b675 {
				return "video/webm", nil
			}
			return "", ErrUnsupported
		}
		if e.id == 0x1654ae6b {
			audio, video, complete, err := webMTracks(s, e)
			if err == errBudget {
				return "video/webm", nil
			}
			if err != nil {
				return "", err
			}
			// Tracks is complete; live payload elements need not have known sizes.
			if complete && audio && !video {
				return "audio/webm", nil
			}
			return "video/webm", nil
		}
		off = e.end
	}
	return "video/webm", nil
}

func webMTracks(s *scanner, tracks ebmlElement) (audio, video, complete bool, err error) {
	complete = true
	count := 0
	for off := tracks.start; off < tracks.end; {
		e, err := readEBML(s, off, tracks.end)
		if err != nil {
			return false, false, false, err
		}
		if e.unknown {
			return false, false, false, ErrUnsupported
		}
		if e.id == 0xae {
			kind, codec, err := webMTrack(s, e)
			if err != nil {
				return false, false, false, err
			}
			count++
			video = video || kind == 1
			audio = audio || kind == 2
			complete = complete && (kind == 1 || kind == 2 && (codec == "A_OPUS" || codec == "A_VORBIS"))
		}
		off = e.end
	}
	return audio, video, complete && count > 0, nil
}

func webMTrack(s *scanner, entry ebmlElement) (kind uint64, codec string, err error) {
	for off := entry.start; off < entry.end; {
		e, err := readEBML(s, off, entry.end)
		if err != nil {
			return 0, "", err
		}
		if e.unknown {
			return 0, "", ErrUnsupported
		}
		n := e.end - e.start
		switch e.id {
		case 0x83:
			if n < 1 || n > 8 || kind != 0 {
				return 0, "", ErrUnsupported
			}
			b, err := s.read(e.start, n)
			if err != nil {
				return 0, "", err
			}
			var padded [8]byte
			copy(padded[8-int(n):], b)
			kind = binary.BigEndian.Uint64(padded[:])
		case 0x86:
			if n == 0 || n > 64 || codec != "" {
				return 0, "", ErrUnsupported
			}
			b, err := s.read(e.start, n)
			if err != nil {
				return 0, "", err
			}
			codec = string(b)
		}
		off = e.end
	}
	return kind, codec, nil
}
