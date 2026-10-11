package mediatype

import "encoding/binary"

type mp3Frame struct {
	length        int64
	version, rate byte
}

func inspectMP3(s *scanner) (string, error) {
	off, err := mp3Start(s)
	if err != nil {
		return "", err
	}
	a, err := readMP3Frame(s, off)
	if err != nil {
		return "", err
	}
	b, err := readMP3Frame(s, off+a.length)
	if err != nil {
		return "", err
	}
	if a.version != b.version || a.rate != b.rate {
		return "", ErrUnsupported
	}
	return "audio/mpeg", nil
}

func mp3Start(s *scanner) (int64, error) {
	b, err := s.read(0, min(s.size, 10))
	if err != nil {
		return 0, err
	}
	if len(b) < 3 || string(b[:3]) != "ID3" {
		return 0, nil
	}
	if len(b) < 10 || b[3] < 2 || b[3] > 4 || b[4] == 255 {
		return 0, ErrUnsupported
	}
	masks := map[byte]byte{2: 0xc0, 3: 0xe0, 4: 0xf0}
	if b[5]&^masks[b[3]] != 0 {
		return 0, ErrUnsupported
	}
	var n int64
	for _, v := range b[6:10] {
		if v&0x80 != 0 {
			return 0, ErrUnsupported
		}
		n = n<<7 | int64(v)
	}
	off := 10 + n
	if b[3] == 4 && b[5]&0x10 != 0 {
		footer, err := s.read(off, 10)
		if err != nil {
			return 0, err
		}
		if string(footer[:3]) != "3DI" || string(footer[3:]) != string(b[3:]) {
			return 0, ErrUnsupported
		}
		off += 10
	}
	if off > s.size {
		return 0, ErrUnsupported
	}
	return off, nil
}

func readMP3Frame(s *scanner, off int64) (mp3Frame, error) {
	b, err := s.read(off, 4)
	if err != nil {
		return mp3Frame{}, err
	}
	h := binary.BigEndian.Uint32(b)
	// MPEG version bits: 3 = MPEG1, 2 = MPEG2, 0 = MPEG2.5; layer 1 is Layer III.
	version := byte(h >> 19 & 3)
	rate := byte(h >> 10 & 3)
	bitrate := int(h >> 12 & 15)
	if h>>21 != 0x7ff || version == 1 || h>>17&3 != 1 || bitrate == 0 || bitrate == 15 || rate == 3 || h&3 == 2 {
		return mp3Frame{}, ErrUnsupported
	}
	rates := [3]int{44100, 48000, 32000}
	sampleRate := rates[rate]
	high := [15]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	low := [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
	coefficient, kbps := 144, high[bitrate]
	if version != 3 {
		coefficient = 72
		kbps = low[bitrate]
		sampleRate /= 2
	}
	if version == 0 {
		sampleRate /= 2
	}
	length := int64(coefficient*kbps*1000/sampleRate) + int64(h>>9&1)
	if length < 4 || length > s.size-off {
		return mp3Frame{}, ErrUnsupported
	}
	return mp3Frame{length, version, rate}, nil
}
