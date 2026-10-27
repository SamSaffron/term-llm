package linkeddevices

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/pairing"
)

func TestEncodeQR(t *testing.T) {
	link := pairing.Link{
		Broker:               "https://broker.example/rendezvous",
		HostKey:              bytes.Repeat([]byte{1}, ed25519.PublicKeySize),
		InvitationID:         bytes.Repeat([]byte{2}, pairing.InvitationIDSize),
		ExpiresAt:            time.Unix(1800000000, 0),
		HostLabel:            "Sam's host 漢字",
		PairingSecret:        bytes.Repeat([]byte{3}, pairing.SecretSize),
		InvitationCapability: "test-capability",
	}
	dataURL, err := encodeQR(link.String())
	if err != nil {
		t.Fatal(err)
	}
	encoded, ok := strings.CutPrefix(dataURL, "data:image/png;base64,")
	if !ok {
		t.Fatal("QR is not a PNG data URL")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if bounds := img.Bounds(); bounds.Dx() != 256 || bounds.Dy() != 256 {
		t.Fatalf("QR dimensions = %v, want 256x256", bounds)
	}
	// The quiet zone must remain white on every side of the invitation.
	for i := 0; i < 256; i++ {
		for _, p := range [][2]int{{i, 0}, {i, 255}, {0, i}, {255, i}} {
			r, g, b, a := img.At(p[0], p[1]).RGBA()
			if r != 0xffff || g != 0xffff || b != 0xffff || a != 0xffff {
				t.Fatalf("quiet zone pixel %v is not opaque white", p)
			}
		}
	}
}

func TestEncodeQRRejectsOversizedContent(t *testing.T) {
	dataURL, err := encodeQR(strings.Repeat("x", pairing.MaxLinkBytes))
	if err == nil || dataURL != "" {
		t.Fatalf("oversized QR returned %d bytes, error %v", len(dataURL), err)
	}
}
