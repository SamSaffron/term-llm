package uploads

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDataURL(t *testing.T) {
	mt, b64 := ParseDataURL("data:image/jpeg;base64,/9j/4AAQ")
	if mt != "image/jpeg" {
		t.Fatalf("media type = %q, want image/jpeg", mt)
	}
	if b64 != "/9j/4AAQ" {
		t.Fatalf("base64 = %q, want /9j/4AAQ", b64)
	}
	mt, b64 = ParseDataURL("not-a-data-url")
	if mt != "" || b64 != "" {
		t.Fatalf("expected empty for invalid data URL, got %q %q", mt, b64)
	}
	mt, b64 = ParseDataURL("data:text/plain;charset=utf-8,hello")
	if mt != "" || b64 != "" {
		t.Fatalf("expected empty for non-base64 data URL, got %q %q", mt, b64)
	}
}

func TestDecodeUploadedFile_AllowsWrappedBase64(t *testing.T) {
	raw, err := DecodeFile("hello.txt", "aGVs\r\nbG8=")
	if err != nil {
		t.Fatalf("DecodeFile() error = %v", err)
	}
	if string(raw) != "hello" {
		t.Fatalf("DecodeFile() = %q, want hello", string(raw))
	}
}

func TestDecodeUploadedFile_RejectsOversizedPayloadBeforeDecode(t *testing.T) {
	b64 := strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxAttachmentBytes+1)-1) + "!"
	_, err := DecodeFile("too-large.bin", b64)
	if err == nil {
		t.Fatal("DecodeFile() error = nil, want size limit error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d MB limit", MaxAttachmentBytes>>20)) {
		t.Fatalf("DecodeFile() error = %v, want size limit error", err)
	}
	if strings.Contains(err.Error(), "decode base64") {
		t.Fatalf("DecodeFile() error = %v, want size check before base64 decode", err)
	}
}

func TestSaveBytesWithLimitUsesPrivateUploadsDir(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	path, err := SaveBytesWithLimit("../notes.txt", []byte("hello"), 5)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != filepath.Join(dataHome, "term-llm", "uploads") {
		t.Fatalf("path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err = %v", info.Mode().Perm(), err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "hello" {
		t.Fatalf("saved bytes = %q, err = %v", raw, err)
	}
}

func TestNormalizeMediaTypeUsesTextEvidence(t *testing.T) {
	if got := NormalizeMediaType("app.ts", "video/mp2t", []byte("export const x = 1\n")); got != "text/x-typescript" {
		t.Fatalf("text TypeScript media type = %q", got)
	}
	if got := NormalizeMediaType("clip.ts", "video/mp2t", []byte{0x47, 0, 0x10, 0, 0xff}); got != "video/mp2t" {
		t.Fatalf("binary transport stream media type = %q", got)
	}
}

func TestResizeImageForLLMRefusesImagesAbove50Megapixels(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4//8/AwAI/AL+KDvV3wAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(raw[16:20], 10_000)
	binary.BigEndian.PutUint32(raw[20:24], 5_001)
	binary.BigEndian.PutUint32(raw[29:33], crc32.ChecksumIEEE(raw[12:29]))
	raw = append(raw, make([]byte, MaxLLMImageBytes-len(raw)+1)...)

	var diagnostics bytes.Buffer
	got, mediaType := resizeImageForLLMWithLogger(raw, "image/png", func(format string, args ...any) {
		fmt.Fprintf(&diagnostics, format, args...)
	})
	if !strings.Contains(diagnostics.String(), "refusing to decode 10000x5001 image") {
		t.Fatalf("missing pixel-limit rejection: %s", diagnostics.String())
	}
	if !bytes.Equal(got, raw) || mediaType != "image/png" {
		t.Fatalf("oversized pixel dimensions were decoded: bytes equal=%t, media type=%q", bytes.Equal(got, raw), mediaType)
	}
}

func TestImageDisplayDimensionsAppliesEXIFOrientation(t *testing.T) {
	raw := makeOrientation6JPEG(t, 400, 200)
	if width, height := ImageDisplayDimensions(raw); width != 200 || height != 400 {
		t.Fatalf("dimensions = %dx%d, want 200x400", width, height)
	}
}

func makeOrientation6JPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height)), &jpeg.Options{Quality: 80}); err != nil {
		t.Fatal(err)
	}
	raw := encoded.Bytes()
	exif := []byte{'E', 'x', 'i', 'f', 0, 0, 'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0}
	segmentLength := len(exif) + 2
	withEXIF := make([]byte, 0, len(raw)+len(exif)+4)
	withEXIF = append(withEXIF, raw[:2]...)
	withEXIF = append(withEXIF, 0xff, 0xe1, byte(segmentLength>>8), byte(segmentLength))
	withEXIF = append(withEXIF, exif...)
	return append(withEXIF, raw[2:]...)
}
