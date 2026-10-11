package mediatype_test

import (
	"encoding/binary"
	"testing"

	"github.com/samsaffron/term-llm/internal/mediatype"
	"github.com/samsaffron/term-llm/internal/testutil/mediafixture"
)

func TestWebMTracksBeforeLiveCluster(t *testing.T) {
	for _, c := range []struct {
		name  string
		kinds []byte
		mime  string
	}{
		{"audio", []byte{2}, "audio/webm"},
		{"mixed", []byte{2, 1}, "video/webm"},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := mediafixture.WebM(c.kinds...)
			headerLen := len(mediafixture.WebM())
			data[headerLen+4] = 0xff                          // Unknown-sized Segment.
			data = append(data, 0x1f, 0x43, 0xb6, 0x75, 0xff) // Unknown-sized Cluster.
			if got := mediatype.Detect(data); got != c.mime {
				t.Fatalf("Detect=%q want=%q", got, c.mime)
			}
		})
	}
	// Stop within the Segment, not at file level: valid trailing Void is allowed.
	data := append(mediafixture.WebM(2), mediafixture.Element([]byte{0xec}, []byte{0})...)
	if got := mediatype.Detect(data); got != "audio/webm" {
		t.Fatalf("trailing Void=%q", got)
	}
	// Other unknown-sized elements after completed Tracks must not undo classification.
	data = mediafixture.WebM(2)
	headerLen := len(mediafixture.WebM())
	data[headerLen+4] = 0xff
	data = append(data, 0xec, 0xff)
	if got := mediatype.Detect(data); got != "audio/webm" {
		t.Fatalf("post-Tracks unknown element=%q", got)
	}
	// Metadata following completed Tracks must not consume the inspection budget.
	data = mediafixture.WebM(2)
	data[headerLen+4] = 0xff
	for i := 0; i < 5000; i++ {
		data = append(data, 0xec, 0x80) // Empty Void.
	}
	if got := mediatype.Detect(data); got != "audio/webm" {
		t.Fatalf("post-Tracks budget fallback=%q", got)
	}
	// Before Tracks, an unknown-sized Cluster still prevents an audio guess.
	header := mediafixture.WebM()
	tracks := mediafixture.WebM(2)[len(header)+5:]
	data = append(header, 0x18, 0x53, 0x80, 0x67, 0xff, 0x1f, 0x43, 0xb6, 0x75, 0xff)
	data = append(data, tracks...)
	if got := mediatype.Detect(data); got != "video/webm" {
		t.Fatalf("pre-Tracks unknown Cluster=%q", got)
	}
}

func TestMP4TrailingPayloadTolerance(t *testing.T) {
	for _, c := range []struct{ brand, handler, mime string }{
		{"isom", "vide", "video/mp4"}, {"isom", "soun", "audio/mp4"}, {"M4A ", "soun", "audio/mp4"},
	} {
		t.Run(c.brand+c.handler, func(t *testing.T) {
			base := mediafixture.MP4(c.brand, c.handler)
			for n := 1; n < 8; n++ {
				data := append(append([]byte{}, base...), make([]byte, n)...)
				if got := mediatype.Detect(data); got != c.mime {
					t.Fatalf("%d trailing bytes: %q", n, got)
				}
			}
			for _, kind := range []string{"mdat", "free", "skip"} {
				tail := mediafixture.Box(kind, []byte{0, 0, 0, 0})
				binary.BigEndian.PutUint32(tail, 100)
				if got := mediatype.Detect(append(append([]byte{}, base...), tail...)); got != c.mime {
					t.Fatalf("overrunning %s=%q", kind, got)
				}
				tail = append([]byte{0, 0, 0, 1}, []byte(kind)...)
				tail = append(tail, make([]byte, 8)...)
				binary.BigEndian.PutUint64(tail[8:], 100)
				if got := mediatype.Detect(append(append([]byte{}, base...), tail...)); got != c.mime {
					t.Fatalf("extended overrunning %s=%q", kind, got)
				}
			}
			tail := mediafixture.Box("moov")
			binary.BigEndian.PutUint32(tail, 100)
			if got := mediatype.Detect(append(base, tail...)); got != "" {
				t.Fatalf("overrunning metadata=%q", got)
			}
		})
	}
	// Nested boxes remain strict even when they happen to end at EOF.
	tail := mediafixture.Box("free")
	binary.BigEndian.PutUint32(tail, 100)
	data := append(mediafixture.MP4("isom"), mediafixture.Box("moov", tail)...)
	if got := mediatype.Detect(data); got != "" {
		t.Fatalf("overrunning nested box=%q", got)
	}
}

func TestMP4MajorBrandAllowlist(t *testing.T) {
	for _, brand := range []string{"avif", "avis", "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1", "crx ", "3gp6", "xxxx"} {
		for _, compat := range []string{"iso8", "M4A "} {
			if compat == "M4A " && (brand == "xxxx" || brand == "3gp6" || brand == "crx ") {
				continue
			}
			t.Run(brand+compat, func(t *testing.T) {
				data := mediafixture.Box("ftyp", []byte(brand), make([]byte, 4), []byte(compat))
				if got := mediatype.Detect(data); got != "" {
					t.Fatalf("major=%q compatible=%q accepted as %q", brand, compat, got)
				}
			})
		}
	}
	for _, brand := range []string{"isom", "iso2", "iso3", "iso4", "iso5", "iso6", "iso7", "iso8", "iso9", "mp41", "mp42", "avc1", "dash", "M4V ", "MSNV"} {
		data := mediafixture.Box("ftyp", []byte(brand), make([]byte, 4))
		if got := mediatype.Detect(data); got != "video/mp4" {
			t.Errorf("legacy brand %q=%q", brand, got)
		}
	}
}

func TestWAVStreamedSizes(t *testing.T) {
	for _, c := range []struct {
		name       string
		riff, data uint32
	}{
		{"unknown RIFF", 0xffffffff, 4}, {"unknown data", 40, 0xffffffff},
		{"pipe", 0xffffffff, 0xffffffff}, {"overrunning data", 40, 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := mediafixture.WAV()
			binary.LittleEndian.PutUint32(b[4:], c.riff)
			binary.LittleEndian.PutUint32(b[40:], c.data)
			if got := mediatype.Detect(b); got != "audio/wav" {
				t.Fatalf("Detect=%q", got)
			}
		})
	}
	for _, off := range []int{4, 16} {
		b := mediafixture.WAV()
		binary.LittleEndian.PutUint32(b[off:], 100)
		if got := mediatype.Detect(b); got != "" {
			t.Fatalf("overflow at %d accepted as %q", off, got)
		}
	}
}
