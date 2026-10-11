package mediatype

import (
	"encoding/binary"
	"io"
)

// IsOggOpus reports whether bounded Ogg identification inspection confirms Opus
// in every initial logical stream. Vorbis returns false, nil; invalid or unknown
// Ogg returns ErrUnsupported. It does not infer a codec from MIME or a suffix.
func IsOggOpus(r io.ReaderAt, size int64) (bool, error) {
	if r == nil || size <= 0 {
		return false, ErrUnsupported
	}
	return inspectOgg(&scanner{r: r, size: size})
}

type oggStream struct {
	packet     []byte
	done, opus bool
	sequence   uint32
}

func inspectOgg(s *scanner) (bool, error) {
	streams := map[uint32]*oggStream{}
	off := int64(0)
	for pages := 0; off < s.size && pages < 32; pages++ {
		if off > 65536-27 {
			return false, ErrUnsupported
		}
		h, err := s.read(off, 27)
		if err != nil {
			return false, err
		}
		if string(h[:4]) != "OggS" || h[4] != 0 || h[5]&^byte(7) != 0 {
			return false, ErrUnsupported
		}
		serial := binary.LittleEndian.Uint32(h[14:18])
		seq := binary.LittleEndian.Uint32(h[18:22])
		st := streams[serial]
		if h[5]&2 != 0 {
			if st != nil || h[5]&1 != 0 || seq != 0 || len(streams) >= 16 {
				return false, ErrUnsupported
			}
			st = &oggStream{}
			streams[serial] = st
		} else {
			if st == nil {
				return false, ErrUnsupported
			}
			if allOggIdentified(streams) {
				return oggIdentifiedCodec(streams)
			}
			if st.done || h[5]&1 == 0 || seq != st.sequence+1 {
				return false, ErrUnsupported
			}
		}
		st.sequence = seq
		if off+27+int64(h[26]) > 65536 {
			return false, ErrUnsupported
		}
		lace, err := s.read(off+27, int64(h[26]))
		if err != nil {
			return false, err
		}
		payload := off + 27 + int64(len(lace))
		var total int64
		for _, n := range lace {
			total += int64(n)
		}
		if total > s.size-payload || payload+total > 65536 {
			return false, ErrUnsupported
		}
		if err := oggIdentification(s, st, lace, payload); err != nil {
			return false, err
		}
		off = payload + total
	}
	if off < s.size || !allOggIdentified(streams) {
		return false, ErrUnsupported
	}
	return oggIdentifiedCodec(streams)
}

func oggIdentification(s *scanner, st *oggStream, lace []byte, off int64) error {
	for _, n := range lace {
		if st.done {
			break
		}
		if len(st.packet)+int(n) > 8192 {
			return ErrUnsupported
		}
		b, err := s.read(off, int64(n))
		if err != nil {
			return err
		}
		off += int64(n)
		st.packet = append(st.packet, b...)
		if n < 255 {
			opus, ok := validOggPacket(st.packet)
			if !ok {
				return ErrUnsupported
			}
			st.done = true
			st.opus = opus
			st.packet = nil
		}
	}
	return nil
}

func allOggIdentified(streams map[uint32]*oggStream) bool {
	if len(streams) == 0 {
		return false
	}
	for _, s := range streams {
		if !s.done {
			return false
		}
	}
	return true
}

// Different codec identification streams are not a single audio presentation.
func oggIdentifiedCodec(streams map[uint32]*oggStream) (bool, error) {
	var opus bool
	first := true
	for _, s := range streams {
		if !first && s.opus != opus {
			return false, ErrUnsupported
		}
		opus = s.opus
		first = false
	}
	return opus, nil
}

func validOggPacket(b []byte) (opus, ok bool) {
	if len(b) >= 19 && string(b[:8]) == "OpusHead" {
		if b[8] == 0 || b[8] > 15 || b[9] == 0 {
			return false, false
		}
		if b[18] == 0 {
			return true, len(b) == 19 && b[9] <= 2
		}
		channels := int(b[9])
		if len(b) != 21+channels || b[19] == 0 || b[20] > b[19] || int(b[19])+int(b[20]) > 255 {
			return false, false
		}
		for _, mapping := range b[21:] {
			if mapping != 255 && int(mapping) >= int(b[19])+int(b[20]) {
				return false, false
			}
		}
		return true, true
	}
	if len(b) != 30 || string(b[:7]) != "\x01vorbis" {
		return false, false
	}
	lo, hi := b[28]&15, b[28]>>4
	return false, binary.LittleEndian.Uint32(b[7:11]) == 0 && b[11] > 0 && binary.LittleEndian.Uint32(b[12:16]) > 0 && binary.LittleEndian.Uint32(b[12:16]) <= 768000 && lo >= 6 && hi <= 13 && hi >= lo && b[29] == 1
}
