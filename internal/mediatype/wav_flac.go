package mediatype

import "encoding/binary"

func inspectWAV(s *scanner) (string, error) {
	b, err := s.read(0, 12)
	if err != nil {
		return "", err
	}
	riffSize := binary.LittleEndian.Uint32(b[4:8])
	end := int64(riffSize) + 8
	if riffSize == 0xffffffff {
		end = s.size
	}
	if string(b[8:12]) != "WAVE" || end < 12 || end > s.size {
		return "", ErrUnsupported
	}
	fmtOK, dataOK := false, false
	for off := int64(12); off < end; {
		if end-off < 8 {
			return "", ErrUnsupported
		}
		h, err := s.read(off, 8)
		if err != nil {
			return "", err
		}
		n := int64(binary.LittleEndian.Uint32(h[4:]))
		start := off + 8
		// Streaming encoders may leave the final data length unknown. Only
		// payload can run to EOF; metadata and ordinary padding remain strict.
		if string(h[:4]) == "data" && end == s.size && (n == 0xffffffff || n > end-start) {
			dataOK = end > start
			break
		}
		if n > end-start || n+(n&1) > end-start {
			return "", ErrUnsupported
		}
		switch string(h[:4]) {
		case "fmt ":
			if fmtOK || n < 16 {
				return "", ErrUnsupported
			}
			f, err := s.read(start, min(n, 40))
			if err != nil {
				return "", err
			}
			if !validWAVFormat(f, n) {
				return "", ErrUnsupported
			}
			fmtOK = true
		case "data":
			dataOK = n > 0
		}
		off = start + n + (n & 1)
	}
	if !fmtOK || !dataOK {
		return "", ErrUnsupported
	}
	return "audio/wav", nil
}

func validWAVFormat(b []byte, chunkSize int64) bool {
	code := binary.LittleEndian.Uint16(b[:2])
	channels := binary.LittleEndian.Uint16(b[2:4])
	rate := binary.LittleEndian.Uint32(b[4:8])
	align := binary.LittleEndian.Uint16(b[12:14])
	bits := binary.LittleEndian.Uint16(b[14:16])
	if code == 0xfffe {
		if len(b) < 40 || binary.LittleEndian.Uint16(b[16:18]) < 22 || int64(binary.LittleEndian.Uint16(b[16:18])) > chunkSize-18 {
			return false
		}
		valid := binary.LittleEndian.Uint16(b[18:20])
		if valid == 0 || valid > bits {
			return false
		}
		// KSDATAFORMAT_SUBTYPE_PCM/IEEE_FLOAT GUID suffix.
		if string(b[26:40]) != string([]byte{0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}) {
			return false
		}
		code = binary.LittleEndian.Uint16(b[24:26])
	}
	if code != 1 && code != 3 {
		return false
	}
	if channels == 0 || channels > 64 || rate == 0 || rate > 768000 || bits == 0 || bits > 64 || bits%8 != 0 {
		return false
	}
	if code == 3 && bits != 32 && bits != 64 {
		return false
	}
	expected := uint32(channels) * uint32(bits/8)
	return uint32(align) == expected && binary.LittleEndian.Uint32(b[8:12]) == rate*expected
}

func inspectFLAC(s *scanner) (string, error) {
	b, err := s.read(0, 42)
	if err != nil {
		return "", err
	}
	if b[4]&0x7f != 0 || b[5] != 0 || b[6] != 0 || b[7] != 34 {
		return "", ErrUnsupported
	}
	minBlock, maxBlock := binary.BigEndian.Uint16(b[8:10]), binary.BigEndian.Uint16(b[10:12])
	minFrame := uint32(b[12])<<16 | uint32(b[13])<<8 | uint32(b[14])
	maxFrame := uint32(b[15])<<16 | uint32(b[16])<<8 | uint32(b[17])
	if maxFrame != 0 && minFrame > maxFrame {
		return "", ErrUnsupported
	}
	packed := binary.BigEndian.Uint64(b[18:26])
	rate := packed >> 44
	bits := (packed >> 36 & 31) + 1
	if minBlock < 16 || maxBlock < minBlock || rate == 0 || rate > 655350 || bits < 4 || bits > 32 {
		return "", ErrUnsupported
	}
	return "audio/flac", nil
}
