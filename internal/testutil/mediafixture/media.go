// Package mediafixture synthesizes small structural identification fixtures.
// These bytes are not promises of decoder-valid playable media.
package mediafixture

import "encoding/binary"

func Box(kind string, payload ...[]byte) []byte {
	b := make([]byte, 8)
	copy(b[4:], kind)
	for _, p := range payload {
		b = append(b, p...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}

func MP4(brand string, handlers ...string) []byte {
	ftyp := Box("ftyp", []byte(brand), make([]byte, 4), []byte("isom"))
	if len(handlers) == 0 {
		return ftyp
	}
	var tracks []byte
	for _, h := range handlers {
		payload := make([]byte, 24)
		copy(payload[8:], h)
		tracks = append(tracks, Box("trak", Box("mdia", Box("hdlr", payload)))...)
	}
	return append(ftyp, Box("moov", tracks)...)
}

func MP3() []byte {
	b := make([]byte, 834)
	copy(b, []byte{0xff, 0xfb, 0x90, 0})
	copy(b[417:], b[:4])
	return b
}

func WAV() []byte {
	b := make([]byte, 48)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], 40)
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], 8000)
	binary.LittleEndian.PutUint32(b[28:], 16000)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], 4)
	return b
}

func Ogg(opus bool) []byte {
	packet := make([]byte, 19)
	if opus {
		copy(packet, "OpusHead")
		packet[8] = 1
		packet[9] = 1
	} else {
		packet = make([]byte, 30)
		copy(packet, "\x01vorbis")
		packet[11] = 1
		binary.LittleEndian.PutUint32(packet[12:], 8000)
		packet[28] = 0x76
		packet[29] = 1
	}
	return OggPage(packet, 1)
}

func OggPage(packet []byte, serial uint32) []byte {
	b := make([]byte, 28)
	copy(b, "OggS")
	b[5] = 2
	binary.LittleEndian.PutUint32(b[14:], serial)
	b[26] = 1
	b[27] = byte(len(packet))
	return append(b, packet...)
}

func FLAC() []byte {
	b := make([]byte, 42)
	copy(b, "fLaC")
	b[4] = 0x80
	b[7] = 34
	binary.BigEndian.PutUint16(b[8:], 4096)
	binary.BigEndian.PutUint16(b[10:], 4096)
	binary.BigEndian.PutUint64(b[18:], uint64(44100)<<44|uint64(15)<<36)
	return b
}

// Element encodes a small EBML element using a one-byte size.
func Element(id []byte, payload ...[]byte) []byte {
	var data []byte
	for _, p := range payload {
		data = append(data, p...)
	}
	if len(data) > 126 {
		panic("fixture element too large")
	}
	b := append([]byte{}, id...)
	b = append(b, byte(len(data))|0x80)
	return append(b, data...)
}

func WebM(kinds ...byte) []byte {
	header := Element([]byte{0x1a, 0x45, 0xdf, 0xa3}, Element([]byte{0x42, 0x82}, []byte("webm")))
	if len(kinds) == 0 {
		return header
	}
	var tracks []byte
	for _, kind := range kinds {
		tracks = append(tracks, Element([]byte{0xae}, Element([]byte{0x83}, []byte{kind}), Element([]byte{0x86}, []byte("A_OPUS")))...)
	}
	segment := Element([]byte{0x18, 0x53, 0x80, 0x67}, Element([]byte{0x16, 0x54, 0xae, 0x6b}, tracks))
	return append(header, segment...)
}

func Audio() map[string][]byte {
	return map[string][]byte{"audio/mpeg": MP3(), "audio/wav": WAV(), "audio/ogg": Ogg(true), "audio/flac": FLAC(), "audio/mp4": MP4("M4A ", "soun"), "audio/webm": WebM(2)}
}
