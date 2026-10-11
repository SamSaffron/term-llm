package mediatype_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/samsaffron/term-llm/internal/mediatype"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
)

func TestFormatsAndInspection(t *testing.T) {
	fixtures := mediafixture.Audio()
	fixtures["image/png"] = []byte("\x89PNG\r\n\x1a\n")
	fixtures["image/jpeg"] = []byte{255, 216, 255}
	fixtures["image/gif"] = []byte("GIF89a")
	fixtures["image/webp"] = []byte("RIFF0000WEBP")
	fixtures["image/bmp"] = []byte("BM")
	fixtures["video/mp4"] = mediafixture.MP4("isom", "vide")
	fixtures["video/webm"] = mediafixture.WebM(1)
	for mime, data := range fixtures {
		t.Run(mime, func(t *testing.T) {
			f, err := mediatype.Inspect(bytes.NewReader(data), int64(len(data)))
			if err != nil || f.MIME != mime || mediatype.Detect(data) != mime {
				t.Fatalf("Inspect=%+v,%v Detect=%q", f, err, mediatype.Detect(data))
			}
			lookup, ok := mediatype.Lookup(" " + mime + " ")
			if !ok || lookup != f {
				t.Fatalf("Lookup=%+v,%v", lookup, ok)
			}
			ext, ok := mediatype.ForExtension(f.Extension)
			if !ok || ext != f {
				t.Fatalf("ForExtension=%+v,%v", ext, ok)
			}
			limits := map[mediatype.Kind]int64{mediatype.Image: 25 << 20, mediatype.Video: 256 << 20, mediatype.Audio: 100 << 20}
			if f.MaxBytes != limits[f.Kind] {
				t.Fatalf("wrong limit %+v", f)
			}
		})
	}
	for _, mime := range []string{"audio/aac", "text/plain", "image/svg+xml", "audio/mpeg; charset=utf-8"} {
		if _, ok := mediatype.Lookup(mime); ok {
			t.Errorf("accepted %q", mime)
		}
	}
}
func TestMP3StructuralVariants(t *testing.T) {
	base := mediafixture.MP3()
	vbr := append([]byte{}, base...)
	vbr[419] = 0xa0
	vbr = append(vbr, make([]byte, 105)...)
	id3 := func(version, flags byte, n int) []byte {
		h := make([]byte, 10+n)
		copy(h, "ID3")
		h[3] = version
		h[5] = flags
		v := n
		for i := 9; i >= 6; i-- {
			h[i] = byte(v & 127)
			v >>= 7
		}
		if flags&16 != 0 {
			footer := append([]byte{}, h[:10]...)
			copy(footer, "3DI")
			h = append(h, footer...)
		}
		return append(h, base...)
	}
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"bare", base, true}, {"VBR", vbr, true}, {"ID3v2", id3(2, 0, 2), true}, {"ID3v3 large", id3(3, 0, 70000), true}, {"ID3 footer", id3(4, 16, 3), true},
		{"single frame", base[:417], false}, {"truncated second", base[:833], false}, {"sync text", []byte("\xffhello"), false}, {"all ff", bytes.Repeat([]byte{255}, 1000), false},
		{"random ID3", []byte("ID3 text is not audio"), false}, {"ADTS", append([]byte{255, 241, 80, 128}, base...), false},
	}
	for _, version := range []byte{0xf3, 0xe3} {
		// MPEG2 / MPEG2.5 Layer III, 64kbps at 22050 / 11025Hz.
		length := 208
		if version == 0xe3 {
			length = 417
		}
		b := make([]byte, length*2)
		copy(b, []byte{255, version, 0x80, 0})
		copy(b[length:], b[:4])
		cases = append(cases, struct {
			name string
			data []byte
			ok   bool
		}{"lower MPEG", b, true})
	}
	for _, mutation := range []struct {
		name  string
		index int
		value byte
	}{
		{"reserved version", 1, 0xeb}, {"layer II", 1, 0xfd}, {"free bitrate", 2, 0}, {"reserved bitrate", 2, 0xf0}, {"reserved rate", 2, 0x9c}, {"reserved emphasis", 3, 2},
	} {
		b := append([]byte{}, base...)
		b[mutation.index] = mutation.value
		cases = append(cases, struct {
			name string
			data []byte
			ok   bool
		}{mutation.name, b, false})
	}
	for _, mutation := range []struct {
		name  string
		index int
		value byte
	}{{"version", 3, 5}, {"revision", 4, 255}, {"flags", 5, 1}, {"synchsafe", 6, 128}} {
		b := id3(3, 0, 0)
		b[mutation.index] = mutation.value
		cases = append(cases, struct {
			name string
			data []byte
			ok   bool
		}{mutation.name, b, false})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mediatype.Detect(c.data); (got == "audio/mpeg") != c.ok {
				t.Fatalf("Detect=%q want audio=%v", got, c.ok)
			}
		})
	}
}
func TestWAVFLACOggValidation(t *testing.T) {
	wav := mediafixture.WAV()
	float := append([]byte{}, wav...)
	binary.LittleEndian.PutUint16(float[20:], 3)
	binary.LittleEndian.PutUint16(float[34:], 32)
	binary.LittleEndian.PutUint16(float[32:], 4)
	binary.LittleEndian.PutUint32(float[28:], 32000)
	padded := append([]byte{}, wav[:12]...)
	padded = append(padded, []byte("JUNK\x01\x00\x00\x00X\x00")...)
	padded = append(padded, wav[12:]...)
	binary.LittleEndian.PutUint32(padded[4:], uint32(len(padded)-8))
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"PCM", wav, "audio/wav"}, {"float", float, "audio/wav"}, {"padded", padded, "audio/wav"}, {"Opus", mediafixture.Ogg(true), "audio/ogg"}, {"Vorbis", mediafixture.Ogg(false), "audio/ogg"}, {"FLAC", mediafixture.FLAC(), "audio/flac"},
		{"Ogg marker", []byte("OggS"), ""}, {"FLAC marker", []byte("fLaC"), ""}, {"Theora", mediafixture.OggPage([]byte("\x80theora identification"), 1), ""},
	}
	for _, mutation := range []struct {
		name  string
		index int
		value byte
	}{{"compressed", 20, 6}, {"zero channels", 22, 0}, {"zero align", 32, 0}, {"fmt length overflow", 19, 255}, {"bad RIFF", 4, 255}} {
		b := append([]byte{}, wav...)
		b[mutation.index] = mutation.value
		cases = append(cases, struct {
			name string
			data []byte
			mime string
		}{mutation.name, b, ""})
	}
	mixed := append(mediafixture.Ogg(true), mediafixture.OggPage([]byte("\x80theora identification"), 2)...)
	cases = append(cases, struct {
		name string
		data []byte
		mime string
	}{"mixed Ogg", mixed, ""})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mediatype.Detect(c.data); got != c.mime {
				t.Fatalf("Detect=%q want=%q", got, c.mime)
			}
		})
	}
	for _, opus := range []bool{true, false} {
		b := mediafixture.Ogg(opus)
		got, err := mediatype.IsOggOpus(bytes.NewReader(b), int64(len(b)))
		if err != nil || got != opus {
			t.Fatalf("Opus=%v,%v", got, err)
		}
	}
}
func TestContainerClassification(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		mime string
	}{
		{"generic audio", mediafixture.MP4("isom", "soun"), "audio/mp4"}, {"mixed", mediafixture.MP4("isom", "soun", "vide"), "video/mp4"},
		{"contradictory M4A", mediafixture.MP4("M4A ", "vide"), "video/mp4"}, {"M4B", mediafixture.MP4("M4B "), "audio/mp4"}, {"fallback", mediafixture.MP4("isom"), "video/mp4"},
		{"AVIF", mediafixture.Box("ftyp", []byte("avif"), make([]byte, 4)), ""}, {"embedded soun", append(mediafixture.MP4("isom"), mediafixture.Box("mdat", []byte("soun"))...), "video/mp4"},
		{"audio WebM", mediafixture.WebM(2), "audio/webm"}, {"mixed WebM", mediafixture.WebM(2, 1), "video/webm"}, {"fallback WebM", mediafixture.WebM(), "video/webm"},
		{"bad VINT", []byte{0x1a, 0x45, 0xdf, 0xa3, 0}, ""}, {"overflow MP4", append([]byte{255, 255, 255, 255}, []byte("ftypisom0000")...), ""},
	}
	audio := mediafixture.MP4("isom", "soun")
	ftyp := audio[:20]
	moov := audio[20:]
	trailing := append(append(append([]byte{}, ftyp...), mediafixture.Box("mdat", make([]byte, 100000))...), moov...)
	cases = append(cases, struct {
		name string
		data []byte
		mime string
	}{"trailing moov", trailing, "audio/mp4"})
	extended := make([]byte, 16)
	binary.BigEndian.PutUint32(extended, 1)
	copy(extended[4:], "mdat")
	binary.BigEndian.PutUint64(extended[8:], 16)
	cases = append(cases, struct {
		name string
		data []byte
		mime string
	}{"extended box", append(append(append([]byte{}, ftyp...), extended...), moov...), "audio/mp4"})
	zero := mediafixture.Box("mdat", []byte("soun"))
	binary.BigEndian.PutUint32(zero, 0)
	cases = append(cases, struct {
		name string
		data []byte
		mime string
	}{"EOF box", append(append([]byte{}, ftyp...), zero...), "video/mp4"})
	unknown := mediafixture.WebM(2)
	unknown[16] = 255 // segment size follows 12-byte header + 4-byte ID
	cases = append(cases, struct {
		name string
		data []byte
		mime string
	}{"unknown segment", unknown, "audio/webm"})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mediatype.Detect(c.data); got != c.mime {
				t.Fatalf("Detect=%q want=%q", got, c.mime)
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) ReadAt([]byte, int64) (int, error) { return 0, r.err }
func TestInspectErrorsAndBounds(t *testing.T) {
	expected := errors.New("device failure")
	if _, err := mediatype.Inspect(failingReader{expected}, 100); !errors.Is(err, expected) {
		t.Fatalf("I/O error=%v", err)
	}
	if _, err := mediatype.Inspect(bytes.NewReader([]byte("abc")), 100); !errors.Is(err, mediatype.ErrUnsupported) {
		t.Fatalf("short read=%v", err)
	}
	if _, err := mediatype.Inspect(failingReader{io.EOF}, -1); !errors.Is(err, mediatype.ErrUnsupported) {
		t.Fatalf("size=%v", err)
	}
}
func FuzzInspect(f *testing.F) {
	for _, b := range mediafixture.Audio() {
		f.Add(b)
	}
	f.Add(mediafixture.MP4("isom", "vide"))
	f.Add([]byte("unsupported"))
	f.Fuzz(func(t *testing.T, b []byte) {
		format, err := mediatype.Inspect(bytes.NewReader(b), int64(len(b)))
		if err == nil {
			if _, ok := mediatype.Lookup(format.MIME); !ok {
				t.Fatal("unregistered format")
			}
		}
		mediatype.Detect(b)
		mediatype.IsOggOpus(bytes.NewReader(b), int64(len(b)))
	})
}

func TestWAVExtensibleAndIdentificationBounds(t *testing.T) {
	pcm := mediafixture.WAV()
	ext := append([]byte{}, pcm[:36]...)
	binary.LittleEndian.PutUint32(ext[16:], 40)
	binary.LittleEndian.PutUint16(ext[20:], 0xfffe)
	extra := make([]byte, 24)
	binary.LittleEndian.PutUint16(extra, 22)
	binary.LittleEndian.PutUint16(extra[2:], 16)
	copy(extra[8:], []byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
	ext = append(ext, extra...)
	ext = append(ext, pcm[36:]...)
	binary.LittleEndian.PutUint32(ext[4:], uint32(len(ext)-8))
	if got := mediatype.Detect(ext); got != "audio/wav" {
		t.Fatalf("extensible PCM=%q", got)
	}
	for _, index := range []int{36, 38, 46} {
		b := append([]byte{}, ext...)
		b[index] = 255
		if got := mediatype.Detect(b); got != "" {
			t.Fatalf("invalid extensible field %d=%q", index, got)
		}
	}
	flac := mediafixture.FLAC()
	flac[7] = 33
	if mediatype.Detect(flac) != "" {
		t.Fatal("invalid STREAMINFO length accepted")
	}
	ogg := mediafixture.Ogg(true)
	ogg[27] = 255
	if mediatype.Detect(ogg) != "" {
		t.Fatal("truncated lacing accepted")
	}
	tag := make([]byte, 10)
	copy(tag, "ID3")
	tag[3] = 4
	tag[6] = 127
	tag[7] = 127
	tag[8] = 127
	tag[9] = 127
	if mediatype.Detect(append(tag, mediafixture.MP3()...)) != "" {
		t.Fatal("out of bounds ID3 accepted")
	}
}

// Reader instrumentation proves payload skipping and bounded work, rather than
// merely using small files where a full read would also pass.
type boundedReader struct {
	reader       io.ReaderAt
	reads, bytes int64
	maxEnd       int64
}

func (r *boundedReader) ReadAt(b []byte, off int64) (int, error) {
	r.reads++
	r.bytes += int64(len(b))
	r.maxEnd = max(r.maxEnd, off+int64(len(b)))
	return r.reader.ReadAt(b, off)
}
func TestMP4SkipsPayloadAndBudgetFallback(t *testing.T) {
	ftyp := mediafixture.MP4("isom")
	moov := mediafixture.MP4("isom", "soun")[len(ftyp):]
	b := append([]byte{}, ftyp...)
	b = append(b, mediafixture.Box("mdat", make([]byte, 5<<20))...)
	b = append(b, moov...)
	r := &boundedReader{reader: bytes.NewReader(b)}
	f, err := mediatype.Inspect(r, int64(len(b)))
	if err != nil || f.MIME != "audio/mp4" || r.bytes > 70000 {
		t.Fatalf("skipping=%+v,%v bytes=%d", f, err, r.bytes)
	}
	// Absent/unavailable tracks conservatively keep legacy video classification.
	var many []byte
	for i := 0; i < 5000; i++ {
		many = append(many, mediafixture.Box("free")...)
	}
	for _, brand := range []string{"isom", "M4A "} {
		data := append(mediafixture.MP4(brand), many...)
		data = append(data, moov...)
		r := &boundedReader{reader: bytes.NewReader(data)}
		f, err := mediatype.Inspect(r, int64(len(data)))
		want := "video/mp4"
		if brand == "M4A " {
			want = "audio/mp4"
		}
		if err != nil || f.MIME != want || r.reads > 4096 || r.bytes > 4<<20 {
			t.Fatalf("budget=%+v,%v reads=%d bytes=%d", f, err, r.reads, r.bytes)
		}
	}
	// An extended box claiming more than int64/file bounds is never a fallback.
	huge := make([]byte, 16)
	binary.BigEndian.PutUint32(huge, 1)
	copy(huge[4:], "mdat")
	binary.BigEndian.PutUint64(huge[8:], ^uint64(0))
	if mediatype.Detect(append(ftyp, huge...)) != "" {
		t.Fatal("overflowing box accepted")
	}
}

func TestCompatibleBrandsAndWebMCodecFallback(t *testing.T) {
	compatible := mediafixture.Box("ftyp", []byte("unknown!"), []byte("M4B "))
	// Major brand is unknown, compatible M4B still identifies an audio container.
	if got := mediatype.Detect(compatible); got != "audio/mp4" {
		t.Fatalf("compatible brand=%q", got)
	}
	webm := mediafixture.WebM(2)
	unknownCodec := bytes.ReplaceAll(webm, []byte("A_OPUS"), []byte("A_FAKE"))
	if got := mediatype.Detect(unknownCodec); got != "video/webm" {
		t.Fatalf("unsupported codec guessed audio=%q", got)
	}
	// Unknown-sized Tracks cannot establish a complete audio list.
	tracksAt := bytes.Index(webm, []byte{0x16, 0x54, 0xae, 0x6b})
	webm[tracksAt+4] = 255
	if got := mediatype.Detect(webm); got != "" {
		t.Fatalf("unknown Tracks=%q", got)
	}
}

func TestOggRejectsMixedAudioCodecs(t *testing.T) {
	vorbis := mediafixture.Ogg(false)
	binary.LittleEndian.PutUint32(vorbis[14:], 2)
	mixed := append(mediafixture.Ogg(true), vorbis...)
	if mediatype.Detect(mixed) != "" {
		t.Fatal("mixed Opus/Vorbis streams accepted")
	}
}

func TestWebMDoesNotIgnoreMalformedOrDuplicateSegments(t *testing.T) {
	webm := mediafixture.WebM(2)
	for _, suffix := range [][]byte{{0}, {0x18, 0x53, 0x80, 0x67, 0x80}} {
		data := append(append([]byte{}, webm...), suffix...)
		if got := mediatype.Detect(data); got != "" {
			t.Fatalf("uninspected trailing segment/structure accepted as %q", got)
		}
	}
}

func TestOggIdentificationContinuation(t *testing.T) {
	// Mapping family 255 permits an explicitly mapped, multi-channel OpusHead.
	packet := make([]byte, 276)
	copy(packet, "OpusHead")
	packet[8] = 1
	packet[9] = 255
	packet[18] = 255
	packet[19] = 255
	for i := 0; i < 255; i++ {
		packet[21+i] = byte(i)
	}
	first := make([]byte, 28)
	copy(first, "OggS")
	first[5] = 2
	binary.LittleEndian.PutUint32(first[14:], 1)
	first[26] = 1
	first[27] = 255
	first = append(first, packet[:255]...)
	second := make([]byte, 28)
	copy(second, "OggS")
	second[5] = 1
	binary.LittleEndian.PutUint32(second[14:], 1)
	binary.LittleEndian.PutUint32(second[18:], 1)
	second[26] = 1
	second[27] = 21
	second = append(second, packet[255:]...)
	data := append(append([]byte{}, first...), second...)
	if got := mediatype.Detect(data); got != "audio/ogg" {
		t.Fatalf("continued OpusHead=%q", got)
	}
	second[18] = 2
	if mediatype.Detect(append(first, second...)) != "" {
		t.Fatal("invalid page sequence accepted")
	}
}

type errorAtReader struct {
	data  []byte
	after int64
	err   error
}

func (r errorAtReader) ReadAt(b []byte, off int64) (int, error) {
	if off >= r.after {
		return 0, r.err
	}
	return bytes.NewReader(r.data).ReadAt(b, off)
}
func TestInspectPropagatesMetadataIOErrors(t *testing.T) {
	ftyp := mediafixture.MP4("isom")
	moov := mediafixture.MP4("isom", "soun")[len(ftyp):]
	b := append(append(append([]byte{}, ftyp...), mediafixture.Box("mdat", make([]byte, 100000))...), moov...)
	expected := errors.New("metadata disk error")
	_, err := mediatype.Inspect(errorAtReader{b, int64(len(b) - len(moov)), expected}, int64(len(b)))
	if !errors.Is(err, expected) || errors.Is(err, mediatype.ErrUnsupported) {
		t.Fatalf("I/O error lost: %v", err)
	}
	_, err = mediatype.Inspect(failingReader{io.ErrUnexpectedEOF}, 10)
	if !errors.Is(err, mediatype.ErrUnsupported) {
		t.Fatalf("truncation reported as I/O failure: %v", err)
	}
}

func TestOggInspectionNeverReadsPastInitialPageBudget(t *testing.T) {
	packet := mediafixture.Ogg(true)[28:]
	page := func(serial uint32, filler []byte) []byte {
		header := make([]byte, 27)
		copy(header, "OggS")
		header[5] = 2
		binary.LittleEndian.PutUint32(header[14:], serial)
		header[26] = byte(1 + len(filler))
		b := append(header, byte(len(packet)))
		b = append(b, filler...)
		b = append(b, packet...)
		for _, n := range filler {
			b = append(b, make([]byte, int(n))...)
		}
		return b
	}
	first := page(1, bytes.Repeat([]byte{255}, 254)) // 65071 bytes
	second := page(2, []byte{255, 150})              // next page starts at 65525
	third := mediafixture.Ogg(true)
	binary.LittleEndian.PutUint32(third[14:], 3)
	data := append(append(first, second...), third...)
	r := &boundedReader{reader: bytes.NewReader(data)}
	_, err := mediatype.IsOggOpus(r, int64(len(data)))
	if !errors.Is(err, mediatype.ErrUnsupported) || r.maxEnd > 65536 {
		t.Fatalf("Ogg page budget err=%v last read end=%d", err, r.maxEnd)
	}
}
